package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/oklog/ulid/v2"
)

// Dispatcher delivers JobStart messages to a live agent channel.
// Implemented by agentgw.Registry. Returns false if the machine is offline.
type Dispatcher interface {
	// SendJobStart delivers a job to the machine's live channel.
	// Returns (false, nil) when offline (caller should leave job pending).
	// Returns (false, err) on send failure (channel may be dying).
	SendJobStart(machineID, jobID, jobType string, paramsJSON []byte) (sent bool, err error)
	// IsOnline reports whether a control channel is live for machineID.
	IsOnline(machineID string) bool
}

// Engine is the job lifecycle core: create/dispatch/progress/complete/fail/cancel.
//
// Initiator is stored in params_json under "initiator" so later stages can emit
// job.run_manual / job.cancel audit events when jobs become human-triggerable.
// Stage 2 does not write those audit events (see server/internal/audit policy).
type Engine struct {
	DB    *catalog.DB
	Locks *RepoLocks
	Log   *slog.Logger
	// Dispatch is optional until the control registry is wired.
	Dispatch Dispatcher

	// MaxPending is the offline queue bound; defaults to MaxPendingJobsPerMachine.
	MaxPending int

	mu sync.Mutex
	// leases held by in-flight jobs (jobID → Lease). Released on terminal or disconnect.
	leases map[string]Lease
	// runningByMachine tracks agent job IDs currently running per machine (for disconnect).
	runningByMachine map[string]map[string]struct{}
}

// NewEngine constructs a job engine. locks may be shared with vault Close/Open callers.
func NewEngine(db *catalog.DB, locks *RepoLocks, log *slog.Logger) *Engine {
	if locks == nil {
		locks = NewRepoLocks()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		DB:               db,
		Locks:            locks,
		Log:              log,
		MaxPending:       MaxPendingJobsPerMachine,
		leases:           make(map[string]Lease),
		runningByMachine: make(map[string]map[string]struct{}),
	}
}

// SubmitRequest creates a job for a machine (or server-only with machine for repo).
type SubmitRequest struct {
	MachineID  string
	Type       string
	ParamsJSON string // optional; merged with kind + initiator
	// Initiator is who requested the job (user id, "system", "test"). Stored in params
	// for future job.run_manual / job.cancel audit — not audited this stage.
	Initiator string
	// RepoID overrides machine_id for vault locking (defaults to MachineID).
	RepoID string
}

// Submit creates a pending job and attempts immediate dispatch if the machine is online.
// Server-only types (prune/verify) are never sent on the channel; they require a
// separate RunServerJob path (stage 3+). Submit of server-only types is rejected
// here so the channel offers no path for agents to trigger prune.
func (e *Engine) Submit(ctx context.Context, req SubmitRequest) (jobID string, err error) {
	if !KnownJobType(req.Type) {
		return "", fmt.Errorf("unknown job type %q", req.Type)
	}
	if IsServerOnly(req.Type) {
		// Structural: agents must never cause prune/verify/replicate creation via
		// any Channel path. Server-side scheduling will call SubmitServer later.
		return "", fmt.Errorf("job type %q is server-only and cannot be submitted for agent dispatch", req.Type)
	}
	if req.MachineID == "" {
		return "", fmt.Errorf("machine_id required")
	}
	if e.MaxPending <= 0 {
		e.MaxPending = MaxPendingJobsPerMachine
	}

	pending, err := e.DB.CountJobsByMachineState(ctx, req.MachineID, catalog.JobStatePending)
	if err != nil {
		return "", err
	}
	if pending >= e.MaxPending {
		return "", fmt.Errorf("pending job queue full for machine %s (bound %d)", req.MachineID, e.MaxPending)
	}

	params, err := mergeParams(req.ParamsJSON, req.Type, req.Initiator)
	if err != nil {
		return "", err
	}

	id := newULID()
	j := catalog.Job{
		ID:         id,
		MachineID:  req.MachineID,
		Type:       req.Type,
		State:      catalog.JobStatePending,
		ParamsJSON: params,
	}
	if err := e.DB.InsertJob(ctx, j); err != nil {
		return "", err
	}
	e.Log.Info("job created", "job_id", id, "machine_id", req.MachineID, "type", req.Type)

	// Try dispatch if online.
	if e.Dispatch != nil && e.Dispatch.IsOnline(req.MachineID) {
		if err := e.tryDispatch(ctx, id); err != nil {
			e.Log.Warn("dispatch after submit failed; job remains pending", "job_id", id, "err", err)
		}
	}
	return id, nil
}

// tryDispatch moves pending → running and sends JobStart. Idempotent: if already
// running or terminal, no-op. Does NOT re-send JobStart for running jobs (reconnect
// idempotency — agent must not run the same job twice).
func (e *Engine) tryDispatch(ctx context.Context, jobID string) error {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %s not found", jobID)
	}
	if j.State != catalog.JobStatePending {
		// Already dispatched or terminal — do not re-send JobStart.
		return nil
	}
	if !IsAgentDispatchable(j.Type) {
		return fmt.Errorf("job type %q is not agent-dispatchable", j.Type)
	}

	// Acquire vault lease if this type needs one (backup/restore in later stages).
	repoID := j.MachineID
	if mode, ok := LockModeForJobType(j.Type); ok {
		lease, err := e.Locks.Acquire(ctx, repoID, mode, jobID)
		if err != nil {
			return fmt.Errorf("acquire repo lock: %w", err)
		}
		e.mu.Lock()
		e.leases[jobID] = lease
		e.mu.Unlock()
	}

	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStatePending},
		catalog.JobStateRunning,
		catalog.JobTransition{SetStarted: true},
	)
	if err != nil {
		e.releaseLease(jobID)
		return err
	}
	if !applied {
		// Lost race — another dispatcher or cancel; drop lease if we took one.
		e.releaseLease(jobID)
		return nil
	}

	e.trackRunning(j.MachineID, jobID)

	if e.Dispatch == nil {
		return nil
	}
	sent, err := e.Dispatch.SendJobStart(j.MachineID, jobID, j.Type, []byte(j.ParamsJSON))
	if err != nil {
		// Channel error: leave running so reconnect policy applies, or fail?
		// Fail on send error so we don't strand "running" without agent knowledge.
		e.Log.Warn("JobStart send failed; marking job failed", "job_id", jobID, "err", err)
		_, _ = e.failJob(ctx, jobID, "dispatch send failed: "+err.Error())
		return err
	}
	if !sent {
		// Went offline between IsOnline and Send — revert to pending for reconnect.
		e.untrackRunning(j.MachineID, jobID)
		e.releaseLease(jobID)
		_, _ = e.DB.TransitionJob(ctx, jobID,
			[]string{catalog.JobStateRunning},
			catalog.JobStatePending,
			catalog.JobTransition{},
		)
		return nil
	}
	e.Log.Info("job dispatched", "job_id", jobID, "machine_id", j.MachineID, "type", j.Type)
	return nil
}

// DeliverPending is called when a machine's control channel comes up.
// Dispatches pending jobs oldest-first up to MaxPending. Running jobs are NOT
// re-dispatched (idempotent reconnect).
func (e *Engine) DeliverPending(ctx context.Context, machineID string) {
	limit := e.MaxPending
	if limit <= 0 {
		limit = MaxPendingJobsPerMachine
	}
	jobs, err := e.DB.ListPendingJobsByMachine(ctx, machineID, limit)
	if err != nil {
		e.Log.Error("list pending jobs", "machine_id", machineID, "err", err)
		return
	}
	for _, j := range jobs {
		if err := e.tryDispatch(ctx, j.ID); err != nil {
			e.Log.Warn("deliver pending failed", "job_id", j.ID, "err", err)
		}
	}
}

// OnAgentDisconnect fails all running jobs for the machine and releases leases.
// Pending jobs stay queued for reconnect delivery.
func (e *Engine) OnAgentDisconnect(ctx context.Context, machineID string) {
	e.mu.Lock()
	ids := make([]string, 0)
	if m, ok := e.runningByMachine[machineID]; ok {
		for id := range m {
			ids = append(ids, id)
		}
	}
	e.mu.Unlock()

	msg := "agent disconnected"
	for _, id := range ids {
		_, _ = e.failJob(ctx, id, msg)
	}
}

// HandleProgress records progress for a running job. No-op if not running.
func (e *Engine) HandleProgress(ctx context.Context, machineID, jobID string, bytesDone, bytesTotal int64) error {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("unknown job %s", jobID)
	}
	if j.MachineID != machineID {
		return fmt.Errorf("job %s does not belong to machine %s", jobID, machineID)
	}
	if j.State != catalog.JobStateRunning {
		return nil // late progress after terminal — ignore
	}
	return e.DB.UpdateJobProgress(ctx, jobID, bytesDone, bytesTotal)
}

// HandleResult applies a JobResult. Duplicate results for terminal jobs are no-ops
// (idempotent reconnect). Terminal → running resurrection is impossible.
func (e *Engine) HandleResult(ctx context.Context, machineID string, result Result) error {
	j, err := e.DB.JobByID(ctx, result.JobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("unknown job %s", result.JobID)
	}
	if j.MachineID != machineID {
		return fmt.Errorf("job %s does not belong to machine %s", result.JobID, machineID)
	}

	// Already terminal → idempotent no-op.
	if isTerminal(j.State) {
		e.Log.Debug("duplicate JobResult for terminal job (no-op)", "job_id", result.JobID, "state", j.State)
		return nil
	}
	if j.State != catalog.JobStateRunning && j.State != catalog.JobStatePending {
		return nil
	}

	if result.Success {
		return e.completeJob(ctx, result.JobID, result.BytesRead, result.BytesStored)
	}
	msg := result.ErrorMessage
	if msg == "" {
		msg = "job failed"
	}
	_, err = e.failJob(ctx, result.JobID, msg)
	return err
}

// Result is the engine-facing JobResult.
type Result struct {
	JobID        string
	Success      bool
	ErrorMessage string
	BytesRead    int64
	BytesStored  int64
	SnapshotID   string
}

// HandleInventory persists an InventoryReport for a machine.
func (e *Engine) HandleInventory(ctx context.Context, machineID string, items []catalog.InventoryItem) error {
	for i := range items {
		items[i].MachineID = machineID
	}
	return e.DB.ReplaceMachineInventory(ctx, machineID, items)
}

// Cancel moves a job to cancelled if not already terminal. Releases lease.
func (e *Engine) Cancel(ctx context.Context, jobID, reason string) error {
	if reason == "" {
		reason = "cancelled"
	}
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %s not found", jobID)
	}
	if isTerminal(j.State) {
		return nil
	}
	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStatePending, catalog.JobStateRunning},
		catalog.JobStateCancelled,
		catalog.JobTransition{
			SetFinished:  true,
			ErrorMessage: &reason,
		},
	)
	if err != nil {
		return err
	}
	if applied {
		e.untrackRunning(j.MachineID, jobID)
		e.releaseLease(jobID)
		e.Log.Info("job cancelled", "job_id", jobID, "reason", reason)
	}
	return nil
}

// Job returns the catalog row (for tests/API).
func (e *Engine) Job(ctx context.Context, jobID string) (*catalog.Job, error) {
	return e.DB.JobByID(ctx, jobID)
}

func (e *Engine) completeJob(ctx context.Context, jobID string, bytesRead, bytesStored int64) error {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job not found")
	}
	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStateRunning, catalog.JobStatePending},
		catalog.JobStateSuccess,
		catalog.JobTransition{
			SetFinished: true,
			BytesRead:   &bytesRead,
			BytesStored: &bytesStored,
		},
	)
	if err != nil {
		return err
	}
	if applied {
		e.untrackRunning(j.MachineID, jobID)
		e.releaseLease(jobID)
		e.Log.Info("job success", "job_id", jobID)
	}
	return nil
}

func (e *Engine) failJob(ctx context.Context, jobID, msg string) (bool, error) {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return false, err
	}
	if j == nil {
		return false, fmt.Errorf("job not found")
	}
	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStateRunning, catalog.JobStatePending},
		catalog.JobStateFailed,
		catalog.JobTransition{
			SetFinished:  true,
			ErrorMessage: &msg,
		},
	)
	if err != nil {
		return false, err
	}
	if applied {
		e.untrackRunning(j.MachineID, jobID)
		e.releaseLease(jobID)
		e.Log.Info("job failed", "job_id", jobID, "err", msg)
	}
	return applied, nil
}

func (e *Engine) releaseLease(jobID string) {
	e.mu.Lock()
	l, ok := e.leases[jobID]
	if ok {
		delete(e.leases, jobID)
	}
	e.mu.Unlock()
	if ok && l != nil {
		l.Release()
	}
}

func (e *Engine) trackRunning(machineID, jobID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.runningByMachine[machineID] == nil {
		e.runningByMachine[machineID] = make(map[string]struct{})
	}
	e.runningByMachine[machineID][jobID] = struct{}{}
}

func (e *Engine) untrackRunning(machineID, jobID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.runningByMachine[machineID]; ok {
		delete(m, jobID)
		if len(m) == 0 {
			delete(e.runningByMachine, machineID)
		}
	}
}

// LeaseCount returns in-memory held leases (tests).
func (e *Engine) LeaseCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.leases)
}

func isTerminal(state string) bool {
	switch state {
	case catalog.JobStateSuccess, catalog.JobStateFailed, catalog.JobStateCancelled:
		return true
	default:
		return false
	}
}

func mergeParams(raw, kind, initiator string) (string, error) {
	m := map[string]any{}
	if raw != "" && raw != "{}" {
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return "", fmt.Errorf("params_json: %w", err)
		}
	}
	m["kind"] = kind
	if initiator != "" {
		m["initiator"] = initiator
	} else if _, ok := m["initiator"]; !ok {
		m["initiator"] = "system"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func newULID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
}
