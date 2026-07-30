package scheduler_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
)

type memDispatch struct {
	mu      sync.Mutex
	online  map[string]bool
	starts  []string // job IDs sent
	sendErr error
}

func newMemDispatch() *memDispatch {
	return &memDispatch{online: make(map[string]bool)}
}

func (m *memDispatch) SetOnline(id string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.online[id] = on
}

func (m *memDispatch) IsOnline(machineID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.online[machineID]
}

func (m *memDispatch) SendJobStart(machineID, jobID, jobType string, paramsJSON []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.online[machineID] {
		return false, nil
	}
	if m.sendErr != nil {
		return false, m.sendErr
	}
	m.starts = append(m.starts, jobID)
	return true, nil
}

func (m *memDispatch) Starts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.starts))
	copy(out, m.starts)
	return out
}

func setupEngine(t *testing.T) (*scheduler.Engine, *catalog.DB, *memDispatch) {
	t.Helper()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Minimal machine row for FK.
	if err := db.InsertMachine(context.Background(), catalog.Machine{
		ID: "mach1", CertFP: "fp1", Hostname: "h", Status: "enrolled", RepoID: "mach1",
	}); err != nil {
		t.Fatal(err)
	}
	d := newMemDispatch()
	e := scheduler.NewEngine(db, scheduler.NewRepoLocks(), nil)
	e.Dispatch = d
	return e, db, d
}

func TestEngine_SubmitDispatchAndComplete(t *testing.T) {
	e, db, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", true)

	id, err := e.Submit(ctx, scheduler.SubmitRequest{
		MachineID: "mach1", Type: scheduler.TypeInventory, Initiator: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.Job(ctx, id)
	if err != nil || j == nil {
		t.Fatalf("job: %v", err)
	}
	if j.State != catalog.JobStateRunning {
		t.Fatalf("state=%s want running", j.State)
	}
	if len(d.Starts()) != 1 || d.Starts()[0] != id {
		t.Fatalf("starts=%v", d.Starts())
	}

	if err := e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: true}); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateSuccess {
		t.Fatalf("state=%s", j.State)
	}

	// Duplicate result is no-op.
	if err := e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: false, ErrorMessage: "should ignore"}); err != nil {
		t.Fatal(err)
	}
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateSuccess {
		t.Fatalf("duplicate mutated state to %s", j.State)
	}
	_ = db
}

func TestEngine_OfflineQueueAndDeliver(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", false)

	id, err := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStatePending {
		t.Fatalf("want pending, got %s", j.State)
	}
	if len(d.Starts()) != 0 {
		t.Fatal("should not dispatch offline")
	}

	d.SetOnline("mach1", true)
	e.DeliverPending(ctx, "mach1")
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("after deliver: %s", j.State)
	}
	if len(d.Starts()) != 1 {
		t.Fatalf("starts=%v", d.Starts())
	}
}

func TestEngine_ReconnectDoesNotRedispatchRunning(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", true)
	id, err := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Starts()) != 1 {
		t.Fatal(d.Starts())
	}
	// Simulate "reconnect deliver" while still running — must not second-send.
	e.DeliverPending(ctx, "mach1")
	if len(d.Starts()) != 1 {
		t.Fatalf("redispatched running job: starts=%v", d.Starts())
	}
	// Duplicate JobResult after complete is covered above; also after first success:
	_ = e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: true})
	_ = e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: true})
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStateSuccess {
		t.Fatal(j.State)
	}
}

func TestEngine_RejectServerOnlySubmit(t *testing.T) {
	e, _, _ := setupEngine(t)
	_, err := e.Submit(context.Background(), scheduler.SubmitRequest{
		MachineID: "mach1", Type: scheduler.TypePrune,
	})
	if err == nil {
		t.Fatal("expected prune submit to fail (server-only)")
	}
}

func TestEngine_DisconnectFailsRunningAndReleasesLease(t *testing.T) {
	e, db, d := setupEngine(t)
	ctx := context.Background()
	// Use a vault-touching type so a lease is taken.
	// file backup is agent-dispatchable and takes shared lock.
	d.SetOnline("mach1", true)
	id, err := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeFileBackup})
	if err != nil {
		t.Fatal(err)
	}
	if e.LeaseCount() != 1 {
		t.Fatalf("lease count=%d want 1", e.LeaseCount())
	}
	// Exclusive must block while shared held.
	if _, ok := e.Locks.TryAcquire("mach1", scheduler.Exclusive, "probe"); ok {
		t.Fatal("exclusive should be blocked by backup lease")
	}

	e.OnAgentDisconnect(ctx, "mach1")
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStateFailed {
		t.Fatalf("state=%s want failed", j.State)
	}
	if e.LeaseCount() != 0 {
		t.Fatalf("lease leak: %d", e.LeaseCount())
	}
	// Exclusive now free.
	if l, ok := e.Locks.TryAcquire("mach1", scheduler.Exclusive, "probe"); !ok {
		t.Fatal("exclusive still blocked after disconnect")
	} else {
		l.Release()
	}
	_ = db
}

func TestEngine_CrossMachineIsolationOnResult(t *testing.T) {
	e, db, d := setupEngine(t)
	ctx := context.Background()
	if err := db.InsertMachine(ctx, catalog.Machine{
		ID: "mach2", CertFP: "fp2", Hostname: "h2", Status: "enrolled", RepoID: "mach2",
	}); err != nil {
		t.Fatal(err)
	}
	d.SetOnline("mach1", true)
	id, _ := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop})
	err := e.HandleResult(ctx, "mach2", scheduler.Result{JobID: id, Success: true})
	if err == nil {
		t.Fatal("expected cross-machine result rejection")
	}
}

func TestEngine_PendingQueueBound(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", false)
	e.MaxPending = 2
	if _, err := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop}); err == nil {
		t.Fatal("expected queue full")
	}
}

func TestEngine_NoTerminalResurrection(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", true)
	id, _ := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop})
	_ = e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: true})
	// tryDispatch must not resurrect.
	if err := e.HandleResult(ctx, "mach1", scheduler.Result{JobID: id, Success: false}); err != nil {
		t.Fatal(err)
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStateSuccess {
		t.Fatal(j.State)
	}
}

func TestEngine_Cancel(t *testing.T) {
	e, _, d := setupEngine(t)
	ctx := context.Background()
	d.SetOnline("mach1", false)
	id, _ := e.Submit(ctx, scheduler.SubmitRequest{MachineID: "mach1", Type: scheduler.TypeNoop})
	if err := e.Cancel(ctx, id, "user cancel"); err != nil {
		t.Fatal(err)
	}
	j, _ := e.Job(ctx, id)
	if j.State != catalog.JobStateCancelled {
		t.Fatal(j.State)
	}
	// Deliver should not revive.
	d.SetOnline("mach1", true)
	e.DeliverPending(ctx, "mach1")
	j, _ = e.Job(ctx, id)
	if j.State != catalog.JobStateCancelled {
		t.Fatal(j.State)
	}
}

func TestIsServerOnly(t *testing.T) {
	if !scheduler.IsServerOnly(scheduler.TypePrune) {
		t.Fatal()
	}
	if scheduler.IsAgentDispatchable(scheduler.TypePrune) {
		t.Fatal("prune must not be agent-dispatchable")
	}
	if !scheduler.IsAgentDispatchable(scheduler.TypeInventory) {
		t.Fatal()
	}
}

func TestEngine_InventoryPersist(t *testing.T) {
	e, db, _ := setupEngine(t)
	ctx := context.Background()
	items := []catalog.InventoryItem{
		{Kind: "volume", ExternalID: "vol-1", Name: "C:\\", Details: map[string]any{"size_bytes": 100}},
		{Kind: "vm", ExternalID: "vm-1", Name: "dc01", RCTCapable: true},
	}
	if err := e.HandleInventory(ctx, "mach1", items); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListMachineInventory(ctx, "mach1")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %v err %v", got, err)
	}
	// Full replace.
	if err := e.HandleInventory(ctx, "mach1", items[:1]); err != nil {
		t.Fatal(err)
	}
	got, _ = db.ListMachineInventory(ctx, "mach1")
	if len(got) != 1 {
		t.Fatalf("len=%d", len(got))
	}
	_ = time.Now
}
