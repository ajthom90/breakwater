package scheduler_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// TestS3F9_ConcurrentDispatchNoLeaseLeak (S3-F9): concurrent DeliverPending for
// one pending job must leave lock accounting at zero after the job completes.
// On 24300b1: shared=1 stuck forever after terminal.
func TestS3F9_ConcurrentDispatchNoLeaseLeak(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", true)

	// Seed a pending file-backup job without inline dispatch.
	d.SetOnline("mach1", false)
	id, err := e.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach1", Type: scheduler.TypeFileBackup, Initiator: "s3-f9",
		ParamsJSON: `{"source":"/tmp"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStatePending {
		t.Fatalf("precondition: %s", j.State)
	}
	d.SetOnline("mach1", true)

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			e.DeliverPending(ctx, "mach1")
		}()
	}
	wg.Wait()

	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("after concurrent deliver: state=%s want running", j.State)
	}
	// Exactly one lease must be tracked.
	if !e.HasLease(id) {
		t.Fatal("S3-F9: no lease tracked for running job (map overwrite race)")
	}
	shared, ex := e.Locks.Held("mach1")
	if shared != 1 || ex != 0 {
		t.Fatalf("after dispatch: shared=%d exclusive=%d want 1,0", shared, ex)
	}

	if err := e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: true}); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateSuccess {
		t.Fatalf("state=%s", j.State)
	}
	if e.HasLease(id) {
		t.Fatal("lease should be released on terminal")
	}
	shared, ex = e.Locks.Held("mach1")
	if shared != 0 || ex != 0 {
		t.Fatalf("S3-F9 LEASE LEAK: after terminal shared=%d exclusive=%d want 0,0", shared, ex)
	}
	// Exclusive must be acquirable (prune not wedged).
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	l, err := e.Locks.AcquireExclusive(cctx, "mach1", "prune-check")
	if err != nil {
		t.Fatalf("S3-F9: exclusive blocked (prune wedged): %v", err)
	}
	l.Release()
	t.Logf("S3-F9 PASS: concurrent dispatch no lease leak")
}

// TestS3F9_StressLockAccounting: N goroutines × M jobs → locks return to zero.
func TestS3F9_StressLockAccounting(t *testing.T) {
	e, db, d := setupEngine(t)
	ctx := context.Background()
	// Extra machines for concurrent jobs.
	for _, mid := range []string{"mach2", "mach3"} {
		if err := db.InsertMachine(ctx, catalog.Machine{
			ID: mid, CertFP: "fp-" + mid, Hostname: mid, Status: "enrolled", RepoID: mid,
		}); err != nil {
			t.Fatal(err)
		}
	}

	const jobsPer = 8
	const workers = 8
	var ids []string
	for _, mid := range []string{"mach1", "mach2", "mach3"} {
		d.SetOnline(mid, false)
		for i := 0; i < jobsPer; i++ {
			id, err := e.Submit(ctx, scheduler.SubmitRequest{
				MachineID: mid, Type: scheduler.TypeFileBackup, Initiator: "stress",
				ParamsJSON: `{"source":"/tmp"}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		d.SetOnline(mid, true)
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, mid := range []string{"mach1", "mach2", "mach3"} {
				e.DeliverPending(ctx, mid)
			}
		}()
	}
	wg.Wait()

	// Complete all jobs.
	for _, id := range ids {
		j, _ := e.Job(ctx, id)
		if j == nil {
			continue
		}
		mid := j.MachineID
		// May still be pending if lease contention — force deliver then complete.
		e.DeliverPending(ctx, mid)
		j, _ = e.Job(ctx, id)
		if j.State == catalog.JobStateRunning {
			_ = e.HandleResult(ctx, mid, scheduler.Result{JobID: id, Success: true})
		} else if j.State == catalog.JobStatePending {
			// exclusive contention shouldn't apply (no exclusive holders)
			e.DeliverPending(ctx, mid)
			j, _ = e.Job(ctx, id)
			if j.State == catalog.JobStateRunning {
				_ = e.HandleResult(ctx, mid, scheduler.Result{JobID: id, Success: true})
			}
		}
	}

	// Drain any remaining.
	for _, mid := range []string{"mach1", "mach2", "mach3"} {
		e.DeliverPending(ctx, mid)
	}
	for _, id := range ids {
		j, _ := e.Job(ctx, id)
		if j != nil && j.State == catalog.JobStateRunning {
			_ = e.HandleResult(ctx, j.MachineID, scheduler.Result{JobID: id, Success: true})
		}
	}

	if e.LeaseCount() != 0 {
		t.Fatalf("S3-F9 stress: LeaseCount=%d want 0", e.LeaseCount())
	}
	for _, mid := range []string{"mach1", "mach2", "mach3"} {
		s, x := e.Locks.Held(mid)
		if s != 0 || x != 0 {
			t.Fatalf("S3-F9 stress: %s held shared=%d exclusive=%d", mid, s, x)
		}
	}
	t.Logf("S3-F9 stress PASS: %d jobs, locks at zero", len(ids))
}

// TestS3F10_CancelTimeoutReleasesLease: hung agent → force-fail after timeout.
func TestS3F10_CancelTimeoutReleasesLease(t *testing.T) {
	e, _, d := setupEngine(t)
	e.CancelTimeout = 80 * time.Millisecond
	ctx := context.Background()
	d.SetOnline("mach1", true)

	id, err := e.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach1", Type: scheduler.TypeFileBackup, Initiator: "s3-f10",
		ParamsJSON: `{"source":"/tmp"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := e.Job(ctx, id)
		if j != nil && j.State == catalog.JobStateRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !e.HasLease(id) {
		t.Fatal("expected lease while running")
	}
	if err := e.Cancel(ctx, id, "operator"); err != nil {
		t.Fatal(err)
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStateCancelling {
		t.Fatalf("state=%s want cancelling", j.State)
	}
	// Wait for the full S3-F10 post-timeout invariant — not merely "state=failed".
	// failJob transitions the catalog row before releaseLease deletes the map
	// entry, so a poll that exits on state alone races HasLease on loaded CI
	// (nightly 30739892213). Poll until all three conditions hold.
	deadline = time.Now().Add(3 * time.Second)
	var (
		state      string
		hasLease   bool
		shared, ex int
	)
	for time.Now().Before(deadline) {
		j, _ = e.Job(ctx, id)
		if j != nil {
			state = j.State
		} else {
			state = ""
		}
		hasLease = e.HasLease(id)
		shared, ex = e.Locks.Held("mach1")
		if state == catalog.JobStateFailed && !hasLease && shared == 0 && ex == 0 {
			t.Logf("S3-F10 PASS: cancel timeout force-failed and released lease")
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("S3-F10: timeout waiting for failed+lease-released+locks-zero; "+
		"state=%q hasLease=%v held shared=%d exclusive=%d",
		state, hasLease, shared, ex)
}
