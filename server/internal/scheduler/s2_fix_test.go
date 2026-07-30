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
		t.Fatalf("HandleResult: %v", err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStatePending {
		t.Fatalf("S2-F4: pending job terminal-ized by result: state=%s want pending", j.State)
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
