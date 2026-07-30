package agentgw

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

// TestS2F2_SupersedeRevertsUndeliveredJobStart (S2-F2): a JobStart that was only
// queued into a session buffer (no writer flush) must revert to pending on
// supersession and re-dispatch on the new channel — not wedge in running forever.
//
// White-box: Register a session without a writer, Submit (queues JobStart),
// supersede via second Register. FAILS on 70e26a2 (job stays running).
func TestS2F2_SupersedeRevertsUndeliveredJobStart(t *testing.T) {
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "mach-f2", CertFP: "fp-f2", Hostname: "h", Status: "enrolled", RepoID: "mach-f2",
	}); err != nil {
		t.Fatal(err)
	}

	log := slog.Default()
	eng := scheduler.NewEngine(db, scheduler.NewRepoLocks(), log)
	reg := NewRegistry(log)
	// Wire undelivered callback the way AttachControlPlane does (after fix).
	reg.OnUndelivered = func(jobIDs []string) {
		eng.RevertUndeliveredJobStarts(context.Background(), jobIDs)
	}
	eng.Dispatch = reg

	// Session without a writer — SendJobStart only enqueues.
	_ = reg.Register("mach-f2", "sess-old")

	jobID, err := eng.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach-f2", Type: scheduler.TypeNoop, Initiator: "s2-f2",
	})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := eng.Job(ctx, jobID)
	if j == nil || j.State != catalog.JobStateRunning {
		t.Fatalf("precondition: want running after queue, got %+v", j)
	}

	// Supersede: new session, old closed without flushing send buffer.
	_ = reg.Register("mach-f2", "sess-new")

	// Allow undelivered handler to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ = eng.Job(ctx, jobID)
		if j != nil && j.State == catalog.JobStatePending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	j, _ = eng.Job(ctx, jobID)
	if j.State != catalog.JobStatePending {
		t.Fatalf("S2-F2: undelivered JobStart after supersede: state=%s want pending (job wedged in running)", j.State)
	}

	// New channel delivers pending.
	eng.DeliverPending(ctx, "mach-f2")
	j, _ = eng.Job(ctx, jobID)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("after re-dispatch: state=%s want running", j.State)
	}
}

// TestS2F2_QueueFullRevertsToPending (S2-F2 related): full send queue must not
// hard-fail the job.
func TestS2F2_QueueFullRevertsToPending(t *testing.T) {
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "mach-qf", CertFP: "fp-qf", Hostname: "h", Status: "enrolled", RepoID: "mach-qf",
	}); err != nil {
		t.Fatal(err)
	}
	eng := scheduler.NewEngine(db, scheduler.NewRepoLocks(), slog.Default())
	reg := NewRegistry(slog.Default())
	reg.OnUndelivered = func(ids []string) { eng.RevertUndeliveredJobStarts(context.Background(), ids) }
	eng.Dispatch = reg

	sess := reg.Register("mach-qf", "s1")
	// Fill send buffer to capacity (no writer).
	for i := 0; i < sendBuf; i++ {
		select {
		case sess.send <- &breakwaterv1.ServerToAgent{}:
		default:
			t.Fatalf("could not fill buffer at i=%d", i)
		}
	}

	jobID, err := eng.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach-qf", Type: scheduler.TypeNoop,
	})
	if err != nil {
		t.Fatal(err)
	}
	// tryDispatch may have left job pending (queue full) or briefly running then pending.
	deadline := time.Now().Add(time.Second)
	var state string
	var errMsg string
	for time.Now().Before(deadline) {
		j, _ := eng.Job(ctx, jobID)
		state = j.State
		errMsg = j.ErrorMessage
		if state == catalog.JobStatePending {
			return // desired
		}
		if state == catalog.JobStateFailed {
			t.Fatalf("S2-F2: queue-full hard-failed job (want pending): state=%s err=%s", state, errMsg)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state != catalog.JobStatePending {
		t.Fatalf("S2-F2: queue-full left job in %s, want pending (err=%s)", state, errMsg)
	}
}
