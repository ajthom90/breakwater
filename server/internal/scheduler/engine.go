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

// Dispatcher delivers JobStart / JobCancel messages to a live agent channel.
// Implemented by agentgw.Registry.
type Dispatcher interface {
	// SendJobStart delivers a job to the machine's live channel.
	// Returns (false, nil) when offline or queue-full (caller leaves/reverts pending).
	// Returns (false, err) only for unexpected failures (also treated as pending).
	SendJobStart(machineID, jobID, jobType string, paramsJSON []byte) (sent bool, err error)
	// SendJobCancel notifies the agent to stop a running job (S2-F6).
	// Returns (false, nil) when offline.
	SendJobCancel(machineID, jobID, reason string) (sent bool, err error)
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
	// CancelTimeout bounds how long cancelling vault jobs hold a lease (S3-F10).
	// Zero means CancelConfirmTimeout.
	CancelTimeout time.Duration

	mu sync.Mutex
	// leases held by in-flight jobs (jobID → Lease). Released on terminal or disconnect.
	leases map[string]Lease
	// runningByMachine tracks agent job IDs currently running per machine (for disconnect).
	runningByMachine map[string]map[string]struct{}
	// cancelTimers force-fail cancelling jobs after CancelTimeout (S3-F10).
	cancelTimers map[string]*time.Timer
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
		cancelTimers:     make(map[string]*time.Timer),
	}
}

// SubmitRequest creates a job for a machine (or server-only with machine for repo).
type SubmitRequest struct {
	MachineID  string
	Type       string
	ParamsJSON string // optional; merged with kind + initiator (server-side metadata)
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

	if e.Dispatch != nil && e.Dispatch.IsOnline(req.MachineID) {
		if err := e.tryDispatch(ctx, id); err != nil {
			e.Log.Warn("dispatch after submit failed; job remains pending", "job_id", id, "err", err)
		}
	}
	return id, nil
}

// tryDispatch moves pending → running and sends JobStart. Idempotent: if already
// running or terminal, no-op. Does NOT re-send JobStart for running jobs that were
// actually delivered (reconnect idempotency). Undelivered JobStarts are reverted
// to pending by RevertUndeliveredJobStarts (S2-F2).
//
// S3-F9: claim the job with pending→running CAS **before** acquiring the lease.
// Concurrent DeliverPending/Submit cannot both acquire Shared for the same job
// (which would overwrite the lease map and leak a lock forever).
func (e *Engine) tryDispatch(ctx context.Context, jobID string) error {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job %s not found", jobID)
	}
	if j.State != catalog.JobStatePending {
		return nil
	}
	if !IsAgentDispatchable(j.Type) {
		return fmt.Errorf("job type %q is not agent-dispatchable", j.Type)
	}

	// Atomic claim first (S3-F9): only one goroutine may proceed to lease acquisition.
	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStatePending},
		catalog.JobStateRunning,
		catalog.JobTransition{SetStarted: true},
	)
	if err != nil {
		return err
	}
	if !applied {
		return nil // another dispatcher claimed it
	}

	repoID := j.MachineID
	if mode, ok := LockModeForJobType(j.Type); ok {
		// Non-blocking / short-timeout (M2-S3): if prune holds exclusive, revert
		// to pending and do not stall DeliverPending for other jobs.
		lease, err := e.tryAcquireLease(ctx, repoID, mode, jobID)
		if err != nil {
			e.Log.Info("dispatch deferred: repo lease unavailable",
				"job_id", jobID, "repo_id", repoID, "mode", mode.String(), "err", err)
			// Revert claim so a later DeliverPending can retry.
			e.untrackRunning(j.MachineID, jobID)
			_, _ = e.DB.TransitionJob(ctx, jobID,
				[]string{catalog.JobStateRunning},
				catalog.JobStatePending,
				catalog.JobTransition{},
			)
			return nil
		}
		e.mu.Lock()
		e.leases[jobID] = lease
		e.mu.Unlock()
	}

	e.trackRunning(j.MachineID, jobID)

	if e.Dispatch == nil {
		return nil
	}
	sent, err := e.Dispatch.SendJobStart(j.MachineID, jobID, j.Type, []byte(j.ParamsJSON))
	if err != nil || !sent {
		// Offline, queue-full, or transient send failure → pending (S2-F2).
		// Never hard-fail: the agent may reconnect and DeliverPending.
		if err != nil {
			e.Log.Warn("JobStart send failed; reverting to pending", "job_id", jobID, "err", err)
		}
		e.revertToPending(ctx, j.MachineID, jobID)
		if err != nil {
			return err
		}
		return nil
	}
	e.Log.Info("job dispatched", "job_id", jobID, "machine_id", j.MachineID, "type", j.Type)
	return nil
}

// revertToPending moves running → pending and releases lease (undelivered / offline).
func (e *Engine) revertToPending(ctx context.Context, machineID, jobID string) {
	e.untrackRunning(machineID, jobID)
	e.releaseLease(jobID)
	_, _ = e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStateRunning},
		catalog.JobStatePending,
		catalog.JobTransition{},
	)
}

// DeliverPending is called when a machine's control channel comes up.
// Dispatches pending jobs oldest-first up to MaxPending. Running jobs are NOT
// re-dispatched (idempotent reconnect for delivered jobs).
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

// OnAgentDisconnect fails all running/cancelling jobs for the machine and releases leases.
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

// RevertUndeliveredJobStarts moves jobs whose JobStart was only buffered (never
// stream.Send-delivered) back to pending and releases leases so DeliverPending
// can re-dispatch (S2-F2). Conditional: only running → pending.
func (e *Engine) RevertUndeliveredJobStarts(ctx context.Context, jobIDs []string) {
	for _, id := range jobIDs {
		j, err := e.DB.JobByID(ctx, id)
		if err != nil || j == nil {
			continue
		}
		if j.State != catalog.JobStateRunning {
			continue
		}
		e.Log.Info("reverting undelivered JobStart to pending", "job_id", id, "machine_id", j.MachineID)
		e.revertToPending(ctx, j.MachineID, id)
	}
}

// RecoverOnStartup fails all orphaned running/cancelling rows left by a previous process.
// Channels and in-memory leases cannot survive a restart (S2-F5).
func (e *Engine) RecoverOnStartup(ctx context.Context) error {
	msg := "server restarted"
	for _, state := range []string{catalog.JobStateRunning, catalog.JobStateCancelling} {
		jobs, err := e.DB.ListJobsByState(ctx, state)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			applied, err := e.DB.TransitionJob(ctx, j.ID,
				[]string{state},
				catalog.JobStateFailed,
				catalog.JobTransition{SetFinished: true, ErrorMessage: &msg},
			)
			if err != nil {
				return err
			}
			if applied {
				e.Log.Info("recovered orphaned job", "job_id", j.ID, "machine_id", j.MachineID, "was", state)
			}
		}
	}
	return nil
}

// HandleProgress records progress for a running job. No-op if not running.
// Unknown job / wrong machine returns error for the caller to log — must not
// be stream-fatal (S2-F1).
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
		return nil
	}
	return e.DB.UpdateJobProgress(ctx, jobID, bytesDone, bytesTotal)
}

// HandleResult applies a JobResult. Duplicate results for terminal jobs are no-ops.
// Results apply only to running or cancelling jobs — never terminal-ize a pending
// job the agent was never sent (S2-F4). Cancelling + any result releases the lease.
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

	if isTerminal(j.State) {
		e.Log.Debug("duplicate JobResult for terminal job (no-op)", "job_id", result.JobID, "state", j.State)
		return nil
	}
	// Cancelling: agent confirmed stop (or finished despite cancel) → terminal + release lease.
	if j.State == catalog.JobStateCancelling {
		msg := result.ErrorMessage
		if msg == "" {
			msg = "cancelled"
		}
		if result.Success {
			// Agent finished successfully despite cancel request — accept success.
			return e.completeJobFrom(ctx, result.JobID, catalog.JobStateCancelling, result.BytesRead, result.BytesStored)
		}
		// Treat as cancelled (not failed) when agent acknowledges cancel.
		return e.finishCancelled(ctx, result.JobID, msg)
	}
	if j.State != catalog.JobStateRunning {
		// Pending or other: ignore (S2-F4). Job may still be delivered later.
		e.Log.Info("ignoring JobResult for non-running job", "job_id", result.JobID, "state", j.State)
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

// CancelConfirmTimeout is how long a cancelling vault-writing job may hold its
// Shared lease waiting for agent JobResult before the server force-fails it
// (S3-F10). Prevents a hung agent on a live channel from wedging prune forever.
// Overridable via Engine.CancelTimeout for tests.
const CancelConfirmTimeout = 2 * time.Minute

// Cancel notifies the agent (when running) and cancels the job.
//
// Lease discipline (S2-F6 / M2-S3): for vault-touching job types that are
// already running, transition to cancelling, send JobCancel, and keep the
// shared lease until agent confirmation (JobResult), channel teardown, or
// CancelConfirmTimeout (S3-F10). Releasing the lease while the agent may still
// write allows prune to race an in-flight backup. Non-vault jobs (inventory/noop)
// cancel immediately. Pending jobs (never dispatched) cancel immediately with no lease.
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
	if j.State == catalog.JobStateCancelling {
		// Already waiting for agent confirmation.
		return nil
	}

	// Running vault-writing job: soft-cancel (lease held until result/teardown/timeout).
	if j.State == catalog.JobStateRunning && HoldsVaultLease(j.Type) {
		if e.Dispatch != nil && j.MachineID != "" {
			if _, err := e.Dispatch.SendJobCancel(j.MachineID, jobID, reason); err != nil {
				e.Log.Warn("JobCancel send failed", "job_id", jobID, "err", err)
			}
		}
		applied, err := e.DB.TransitionJob(ctx, jobID,
			[]string{catalog.JobStateRunning},
			catalog.JobStateCancelling,
			catalog.JobTransition{ErrorMessage: &reason},
		)
		if err != nil {
			return err
		}
		if applied {
			e.Log.Info("job cancelling (lease held until agent confirms or timeout)",
				"job_id", jobID, "reason", reason)
			e.scheduleCancelDeadline(jobID)
		}
		return nil
	}

	// Notify agent when running (non-vault).
	if j.State == catalog.JobStateRunning && e.Dispatch != nil && j.MachineID != "" {
		if _, err := e.Dispatch.SendJobCancel(j.MachineID, jobID, reason); err != nil {
			e.Log.Warn("JobCancel send failed", "job_id", jobID, "err", err)
		}
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

// HoldsVaultLease reports whether a job type acquires a repo lease at dispatch.
func HoldsVaultLease(jobType string) bool {
	_, ok := LockModeForJobType(jobType)
	return ok
}

// DispatchLeaseTimeout is the max wait for a shared/exclusive lease at dispatch.
// On timeout the job stays pending (non-blocking DeliverPending).
const DispatchLeaseTimeout = 50 * time.Millisecond

func (e *Engine) tryAcquireLease(ctx context.Context, repoID string, mode LockMode, jobID string) (Lease, error) {
	// Prefer TryAcquire when available (zero wait).
	if l, ok := e.Locks.TryAcquire(repoID, mode, jobID); ok {
		return l, nil
	}
	// Short timeout so exclusive holders (prune) do not stall the dispatch loop.
	cctx, cancel := context.WithTimeout(ctx, DispatchLeaseTimeout)
	defer cancel()
	return e.Locks.Acquire(cctx, repoID, mode, jobID)
}

// VaultForJob returns a vault handle only when this engine holds a lease for
// jobID (structural lease discipline for DataService). The opener is provided
// by the data plane (keystore password + Manager.Open); this method only
// gates access on the lease table.
//
// Manager.Open must not be called from the data plane for job RPCs without
// going through this check first.
func (e *Engine) VaultForJob(jobID string) (leaseOK bool, repoID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.leases[jobID]
	if !ok || l == nil {
		return false, ""
	}
	return true, l.RepoID()
}

// HasLease reports whether the engine currently holds a lease for jobID (tests).
func (e *Engine) HasLease(jobID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.leases[jobID]
	return ok
}

// scheduleCancelDeadline force-fails a cancelling job if the agent never confirms (S3-F10).
func (e *Engine) scheduleCancelDeadline(jobID string) {
	timeout := e.CancelTimeout
	if timeout <= 0 {
		timeout = CancelConfirmTimeout
	}
	e.mu.Lock()
	if t, ok := e.cancelTimers[jobID]; ok {
		t.Stop()
	}
	e.cancelTimers[jobID] = time.AfterFunc(timeout, func() {
		e.forceCancelTimeout(jobID)
	})
	e.mu.Unlock()
}

func (e *Engine) clearCancelTimer(jobID string) {
	e.mu.Lock()
	if t, ok := e.cancelTimers[jobID]; ok {
		t.Stop()
		delete(e.cancelTimers, jobID)
	}
	e.mu.Unlock()
}

func (e *Engine) forceCancelTimeout(jobID string) {
	ctx := context.Background()
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil || j == nil {
		return
	}
	if j.State != catalog.JobStateCancelling {
		return
	}
	msg := "cancel confirmation timeout: agent did not confirm; lease force-released"
	e.Log.Error(msg, "job_id", jobID, "machine_id", j.MachineID)
	_, _ = e.failJob(ctx, jobID, msg)
}

// Job returns the catalog row (for tests/API).
func (e *Engine) Job(ctx context.Context, jobID string) (*catalog.Job, error) {
	return e.DB.JobByID(ctx, jobID)
}

func (e *Engine) completeJob(ctx context.Context, jobID string, bytesRead, bytesStored int64) error {
	return e.completeJobFrom(ctx, jobID, catalog.JobStateRunning, bytesRead, bytesStored)
}

func (e *Engine) completeJobFrom(ctx context.Context, jobID, fromState string, bytesRead, bytesStored int64) error {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job not found")
	}
	// Only running/cancelling → success (S2-F4: never from pending via result path).
	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{fromState},
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

func (e *Engine) finishCancelled(ctx context.Context, jobID, msg string) error {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return err
	}
	if j == nil {
		return fmt.Errorf("job not found")
	}
	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStateCancelling},
		catalog.JobStateCancelled,
		catalog.JobTransition{SetFinished: true, ErrorMessage: &msg},
	)
	if err != nil {
		return err
	}
	if applied {
		e.untrackRunning(j.MachineID, jobID)
		e.releaseLease(jobID)
		e.Log.Info("job cancelled (agent confirmed)", "job_id", jobID)
	}
	return nil
}

// failJob transitions running, cancelling, or pending → failed (server-initiated
// paths: disconnect, RecoverOnStartup). Agent JobResult failures go through
// HandleResult which only allows running/cancelling.
func (e *Engine) failJob(ctx context.Context, jobID, msg string) (bool, error) {
	j, err := e.DB.JobByID(ctx, jobID)
	if err != nil {
		return false, err
	}
	if j == nil {
		return false, fmt.Errorf("job not found")
	}
	applied, err := e.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStateRunning, catalog.JobStateCancelling, catalog.JobStatePending},
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
	e.clearCancelTimer(jobID)
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

// mergeParams stores server-side metadata (kind, initiator). Wire JobType is the
// agent-facing discriminator (S2-F7); kind here is catalog convenience only.
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
