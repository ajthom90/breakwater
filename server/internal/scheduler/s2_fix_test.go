package scheduler_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// TestS2F3_ExclusiveNotStarvedBySharedChain (S2-F3): with continuous overlapping
// shared holders, exclusive (prune) must complete within a bound once current
// shareds drain under writer preference. FAILS on 70e26a2 (new shared keeps
// arriving while exclusive waits → starvation until chain fully stops).
func TestS2F3_ExclusiveNotStarvedBySharedChain(t *testing.T) {
	locks := scheduler.NewRepoLocks()
	const repo = "repo-starve"
	const hold = 30 * time.Millisecond

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Continuous shared chain: each holder spawns the next before releasing.
	startShared := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-startShared
		for !stop.Load() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			l, err := locks.AcquireShared(ctx, repo, "backup-chain")
			cancel()
			if err != nil {
				return
			}
			time.Sleep(hold)
			// Try to keep shared continuously occupied.
			l.Release()
			// Tiny gap — without writer preference exclusive can miss forever
			// if another shared sneaks in; with preference, exclusive is next.
		}
	}()

	// First shared so exclusive must wait.
	l0, err := locks.AcquireShared(context.Background(), repo, "backup-0")
	if err != nil {
		t.Fatal(err)
	}
	close(startShared)

	exDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// Give exclusive a moment to enter the wait queue while shared held.
		time.Sleep(5 * time.Millisecond)
		l, err := locks.AcquireExclusive(ctx, repo, "prune-1")
		if err != nil {
			exDone <- err
			return
		}
		l.Release()
		exDone <- nil
	}()

	// Release initial shared after exclusive has registered as waiter.
	time.Sleep(20 * time.Millisecond)
	l0.Release()

	select {
	case err := <-exDone:
		stop.Store(true)
		wg.Wait()
		if err != nil {
			t.Fatalf("S2-F3: exclusive starved/failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		stop.Store(true)
		wg.Wait()
		t.Fatal("S2-F3: exclusive did not acquire within bound (writer preference missing)")
	}
}

// TestS2F4_ResultIgnoredForPendingJob (S2-F4): JobResult for a still-pending job
// must not terminal-ize it; later delivery still works. FAILS on 70e26a2.
// M2-S3 hardening: also covers FAILURE JobResult for pending (reviewer F4 mutant).
func TestS2F4_ResultIgnoredForPendingJob(t *testing.T) {
	e, db, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", false) // stay pending

	id, err := e.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach1", Type: scheduler.TypeNoop, Initiator: "s2-f4",
	})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStatePending {
		t.Fatalf("precondition: %s", j.State)
	}

	// Agent (or attacker) claims success for a job never dispatched.
	if err := e.HandleResult(ctx, "mach1", scheduler.Result{
		JobID: id, Success: true, BytesRead: 999,
	}); err != nil {
		t.Fatalf("HandleResult success: %v", err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStatePending {
		t.Fatalf("S2-F4: pending job terminal-ized by success result: state=%s want pending", j.State)
	}

	// FAILURE path must also be ignored (stage-3 hardening — completeJob double-guard
	// does not cover failJob which historically allowed pending→failed).
	if err := e.HandleResult(ctx, "mach1", scheduler.Result{
		JobID: id, Success: false, ErrorMessage: "fake failure for pending",
	}); err != nil {
		t.Fatalf("HandleResult failure: %v", err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStatePending {
		t.Fatalf("S2-F4: pending job terminal-ized by failure result: state=%s want pending", j.State)
	}

	// Later delivery still works.
	d.SetOnline("mach1", true)
	e.DeliverPending(ctx, "mach1")
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("after deliver: state=%s want running", j.State)
	}
	if err := e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: true}); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateSuccess {
		t.Fatalf("after real result: %s", j.State)
	}
	_ = db
}

// TestS3_CancelVaultJobHoldsLeaseUntilResult: file-backup Cancel → cancelling,
// lease held; JobResult releases lease.
func TestS3_CancelVaultJobHoldsLeaseUntilResult(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", true)

	id, err := e.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach1", Type: scheduler.TypeFileBackup, Initiator: "s3-cancel",
		ParamsJSON: `{"source":"/tmp"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for dispatch.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := e.Job(ctx, id)
		if j != nil && j.State == catalog.JobStateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("state=%s want running", j.State)
	}
	if !e.HasLease(id) {
		t.Fatal("expected lease held while running")
	}

	if err := e.Cancel(ctx, id, "operator cancel"); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateCancelling {
		t.Fatalf("after Cancel: state=%s want cancelling", j.State)
	}
	if !e.HasLease(id) {
		t.Fatal("lease must remain held while cancelling (agent may still write)")
	}
	if got := d.Cancels(); len(got) < 1 || got[0] != id {
		t.Fatalf("expected JobCancel sent, got %v", got)
	}

	if err := e.HandleResult(ctx, "mach1", scheduler.Result{
		JobID: id, Success: false, ErrorMessage: "cancelled by agent",
	}); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateCancelled {
		t.Fatalf("after result: state=%s want cancelled", j.State)
	}
	if e.HasLease(id) {
		t.Fatal("lease should be released after agent confirmation")
	}
}

// TestS3_DispatchLeaseNonBlocking: exclusive holder blocks shared; file job
// stays pending and DeliverPending does not hang.
func TestS3_DispatchLeaseNonBlocking(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", true)

	// Hold exclusive (prune).
	ex, err := e.Locks.AcquireExclusive(ctx, "mach1", "prune-block")
	if err != nil {
		t.Fatal(err)
	}
	defer ex.Release()

	start := time.Now()
	id, err := e.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach1", Type: scheduler.TypeFileBackup, Initiator: "s3-lease",
		ParamsJSON: `{"source":"/tmp"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	// DeliverPending must return promptly (non-blocking lease).
	e.DeliverPending(ctx, "mach1")
	if time.Since(start) > 2*time.Second {
		t.Fatalf("DeliverPending blocked too long: %s", time.Since(start))
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStatePending {
		t.Fatalf("state=%s want pending (lease blocked)", j.State)
	}

	ex.Release()
	e.DeliverPending(ctx, "mach1")
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("after exclusive release: state=%s want running", j.State)
	}
}

// TestS2F5_RecoverOnStartup fails orphaned running rows.
func TestS2F5_RecoverOnStartup(t *testing.T) {
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "mach1", CertFP: "fp", Hostname: "h", Status: "enrolled", RepoID: "mach1",
	}); err != nil {
		t.Fatal(err)
	}
	// Seed an orphaned running row (as if server crashed mid-job).
	if err := db.InsertJob(ctx, catalog.Job{
		ID: "01ORPHANJOB0000000000000001", MachineID: "mach1",
		Type: scheduler.TypeNoop, State: catalog.JobStatePending,
	}); err != nil {
		t.Fatal(err)
	}
	ok, err := db.TransitionJob(ctx, "01ORPHANJOB0000000000000001",
		[]string{catalog.JobStatePending}, catalog.JobStateRunning,
		catalog.JobTransition{SetStarted: true})
	if err != nil || !ok {
		t.Fatalf("seed running: ok=%v err=%v", ok, err)
	}

	e := scheduler.NewEngine(db, scheduler.NewRepoLocks(), nil)
	if err := e.RecoverOnStartup(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := e.Job(ctx, "01ORPHANJOB0000000000000001")
	if err != nil || j == nil {
		t.Fatal(err)
	}
	if j.State != catalog.JobStateFailed {
		t.Fatalf("S2-F5: orphaned running state=%s want failed", j.State)
	}
	if j.ErrorMessage != "server restarted" {
		t.Fatalf("error_message=%q", j.ErrorMessage)
	}
}

// TestS2F6_CancelDeliversJobCancel is covered in agentgw; engine unit checks
// Dispatcher.SendJobCancel is invoked — see agentgw TestS2F6.
