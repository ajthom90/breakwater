// Package control implements the agent control plane client:
// persistent Channel dial-out, Hello/Heartbeat, job dispatch, reconnect
// with jittered exponential backoff, and completed-job idempotency.
//
// Contract (server/internal/agentgw/channel.go):
//
//   - Dial :9443 with mTLS; open ControlService.Channel
//   - First message: Hello with enrolled machine_id
//   - Heartbeats ≤30 s; gRPC keepalive Time=30s PermitWithoutStream
//   - Branch on JobStart.type; JobResult echoes job_id
//   - Never re-run a completed job_id
//   - JobCancel → stop work; always send terminal JobResult
package control

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ajthom90/breakwater/agent/internal/identity"
	"github.com/ajthom90/breakwater/agent/internal/inventory"
	"github.com/ajthom90/breakwater/agent/internal/state"
	"github.com/ajthom90/breakwater/pkg/backup"
	"github.com/ajthom90/breakwater/pkg/contentid"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/pkg/restore"
)

// ClientParameters match the stage-2 channel contract.
var ClientParameters = keepalive.ClientParameters{
	Time:                30 * time.Second,
	Timeout:             10 * time.Second,
	PermitWithoutStream: true,
}

// Backoff caps for reconnect (jittered exponential).
const (
	BackoffInitial = 1 * time.Second
	BackoffMax     = 60 * time.Second
	HeartbeatEvery = 25 * time.Second // ≤30 s
)

// Config for the control loop.
type Config struct {
	State   *state.Dir
	Meta    *state.Identity
	Creds   *identity.Identity
	Version string
	Log     *slog.Logger

	// HeartbeatInterval defaults to HeartbeatEvery.
	HeartbeatInterval time.Duration
	// BackoffInitial/Max override defaults (tests).
	BackoffInitial time.Duration
	BackoffMax     time.Duration

	// Dial optional: tests inject a pre-built conn factory.
	// If nil, dials Meta.ServerAddr with mTLS.
	Dial func(ctx context.Context) (*grpc.ClientConn, error)
}

// Agent is the long-running control client.
type Agent struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	active   map[string]context.CancelFunc // job_id → cancel
	running  bool
	stopOnce sync.Once
	stopCh   chan struct{}

	// sendMu serializes every stream.Send / CloseSend on the control channel.
	// gRPC forbids concurrent SendMsg (S4-F1). Heartbeat, JobProgress, JobResult,
	// InventoryReport, and Hello all take this lock — no exceptions.
	sendMu sync.Mutex
}

// New creates an Agent. Call Run.
func New(cfg Config) *Agent {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = HeartbeatEvery
	}
	if cfg.BackoffInitial <= 0 {
		cfg.BackoffInitial = BackoffInitial
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = BackoffMax
	}
	return &Agent{
		cfg:    cfg,
		log:    log,
		active: make(map[string]context.CancelFunc),
		stopCh: make(chan struct{}),
	}
}

// Stop requests graceful shutdown: cancel active jobs, close channel loop.
func (a *Agent) Stop() {
	a.stopOnce.Do(func() { close(a.stopCh) })
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, cancel := range a.active {
		cancel()
		delete(a.active, id)
	}
}

// Run dials, maintains the control channel, and reconnects until Stop or ctx done.
func (a *Agent) Run(ctx context.Context) error {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return fmt.Errorf("control: already running")
	}
	a.running = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.running = false
		a.mu.Unlock()
	}()

	backoff := a.cfg.BackoffInitial
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.stopCh:
			return nil
		default:
		}

		err := a.session(ctx)
		if err == nil || ctx.Err() != nil {
			return err
		}
		select {
		case <-a.stopCh:
			return nil
		default:
		}
		a.log.Warn("control channel ended; reconnecting", "err", err, "backoff", backoff)

		// Jittered exponential backoff — avoid thundering herd.
		jitter := time.Duration(rand.Int63n(int64(backoff/2) + 1))
		wait := backoff/2 + jitter
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-a.stopCh:
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > a.cfg.BackoffMax {
			backoff = a.cfg.BackoffMax
		}
	}
}

func (a *Agent) dial(ctx context.Context) (*grpc.ClientConn, error) {
	if a.cfg.Dial != nil {
		return a.cfg.Dial(ctx)
	}
	tlsCfg := identity.ClientTLSConfig(a.cfg.Creds, a.cfg.Meta.ServerFP)
	return grpc.NewClient(a.cfg.Meta.ServerAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithKeepaliveParams(ClientParameters),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)),
	)
}

func (a *Agent) session(ctx context.Context) error {
	conn, err := a.dial(ctx)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	stream, err := breakwaterv1.NewControlServiceClient(conn).Channel(ctx)
	if err != nil {
		return fmt.Errorf("Channel: %w", err)
	}

	ver := a.cfg.Version
	if ver == "" {
		ver = "0.0.1-dev"
	}
	if err := a.send(stream, &breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Hello{
			Hello: &breakwaterv1.Hello{
				MachineId:    a.cfg.Meta.MachineID,
				AgentVersion: ver,
			},
		},
	}); err != nil {
		return fmt.Errorf("Hello: %w", err)
	}

	// Wait for HelloAck before starting work loops.
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("HelloAck: %w", err)
	}
	if msg.GetHelloAck() == nil {
		return fmt.Errorf("expected HelloAck, got %T", msg.Msg)
	}
	a.log.Info("control channel up",
		"machine_id", a.cfg.Meta.MachineID,
		"session_id", msg.GetHelloAck().GetSessionId(),
		"server_version", msg.GetHelloAck().GetServerVersion(),
	)

	// DataService + RestoreService share the same mTLS conn.
	dataCl := breakwaterv1.NewDataServiceClient(conn)
	restoreCl := breakwaterv1.NewRestoreServiceClient(conn)
	hasher, err := a.hasher()
	if err != nil {
		return err
	}

	sessCtx, sessCancel := context.WithCancel(ctx)
	defer sessCancel()

	// Heartbeat ticker.
	hbErr := make(chan error, 1)
	go func() {
		t := time.NewTicker(a.cfg.HeartbeatInterval)
		defer t.Stop()
		// Immediate heartbeat after Hello.
		if err := a.sendHeartbeat(stream); err != nil {
			hbErr <- err
			return
		}
		for {
			select {
			case <-sessCtx.Done():
				hbErr <- nil
				return
			case <-a.stopCh:
				hbErr <- nil
				return
			case <-t.C:
				if err := a.sendHeartbeat(stream); err != nil {
					hbErr <- err
					return
				}
			}
		}
	}()

	// Reader loop.
	readErr := make(chan error, 1)
	go func() {
		for {
			m, err := stream.Recv()
			if err != nil {
				readErr <- err
				return
			}
			a.handleServer(sessCtx, stream, dataCl, restoreCl, hasher, m)
		}
	}()

	select {
	case <-ctx.Done():
		sessCancel()
		_ = a.closeSend(stream)
		return ctx.Err()
	case <-a.stopCh:
		sessCancel()
		// Cancel active jobs; they must send JobResult.
		a.cancelAll()
		_ = a.closeSend(stream)
		return nil
	case err := <-hbErr:
		sessCancel()
		if err != nil {
			return fmt.Errorf("heartbeat: %w", err)
		}
		return nil
	case err := <-readErr:
		sessCancel()
		return fmt.Errorf("recv: %w", err)
	}
}

// send is the single choke point for ControlService.Channel client sends (S4-F1).
func (a *Agent) send(stream breakwaterv1.ControlService_ChannelClient, msg *breakwaterv1.AgentToServer) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return stream.Send(msg)
}

func (a *Agent) closeSend(stream breakwaterv1.ControlService_ChannelClient) error {
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	return stream.CloseSend()
}

func (a *Agent) sendHeartbeat(stream breakwaterv1.ControlService_ChannelClient) error {
	return a.send(stream, &breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Heartbeat{
			Heartbeat: &breakwaterv1.Heartbeat{
				ClientTime: timestamppb.Now(),
			},
		},
	})
}

func (a *Agent) hasher() (*contentid.Hasher, error) {
	key, err := a.cfg.Meta.HashingKey()
	if err != nil {
		return nil, fmt.Errorf("hashing key: %w", err)
	}
	return contentid.New(a.cfg.Meta.HashingAlgorithm, key)
}

func (a *Agent) handleServer(
	ctx context.Context,
	stream breakwaterv1.ControlService_ChannelClient,
	dataCl breakwaterv1.DataServiceClient,
	restoreCl breakwaterv1.RestoreServiceClient,
	hasher *contentid.Hasher,
	msg *breakwaterv1.ServerToAgent,
) {
	if msg == nil {
		return
	}
	switch m := msg.Msg.(type) {
	case *breakwaterv1.ServerToAgent_HelloAck, *breakwaterv1.ServerToAgent_HeartbeatAck:
		return
	case *breakwaterv1.ServerToAgent_UpdateOffer:
		// Reserved — ignore.
		return
	case *breakwaterv1.ServerToAgent_JobCancel:
		a.cancelJob(m.JobCancel.GetJobId(), m.JobCancel.GetReason())
	case *breakwaterv1.ServerToAgent_JobStart:
		a.startJob(ctx, stream, dataCl, restoreCl, hasher, m.JobStart)
	}
}

func (a *Agent) cancelAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, cancel := range a.active {
		cancel()
		delete(a.active, id)
	}
}

func (a *Agent) cancelJob(jobID, reason string) {
	a.mu.Lock()
	cancel, ok := a.active[jobID]
	a.mu.Unlock()
	if ok {
		a.log.Info("JobCancel received", "job_id", jobID, "reason", reason)
		cancel()
	}
}

func (a *Agent) startJob(
	ctx context.Context,
	stream breakwaterv1.ControlService_ChannelClient,
	dataCl breakwaterv1.DataServiceClient,
	restoreCl breakwaterv1.RestoreServiceClient,
	hasher *contentid.Hasher,
	js *breakwaterv1.JobStart,
) {
	jobID := js.GetJobId()
	if jobID == "" {
		return
	}
	// Reconnect idempotency: never re-run a completed job_id; replay the *real*
	// outcome (S4-F3) — never synthesize Success for a failed job.
	if a.cfg.State != nil {
		if ok, success, errMsg := a.cfg.State.CompletedOutcome(jobID); ok {
			a.log.Info("skip already-completed job_id", "job_id", jobID, "success", success)
			msg := "already completed (idempotent)"
			if errMsg != "" {
				msg = errMsg + " [idempotent replay]"
			}
			_ = a.send(stream, &breakwaterv1.AgentToServer{
				Msg: &breakwaterv1.AgentToServer_JobResult{
					JobResult: &breakwaterv1.JobResult{
						JobId: jobID, Success: success, ErrorMessage: msg,
					},
				},
			})
			return
		}
	}

	jobCtx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	if _, exists := a.active[jobID]; exists {
		a.mu.Unlock()
		cancel()
		a.log.Warn("duplicate JobStart while active", "job_id", jobID)
		return
	}
	a.active[jobID] = cancel
	a.mu.Unlock()

	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.active, jobID)
			a.mu.Unlock()
			cancel()
		}()
		res := a.runJob(jobCtx, stream, dataCl, restoreCl, hasher, js)
		// Always send a terminal JobResult (cancel confirmation contract).
		if err := a.send(stream, &breakwaterv1.AgentToServer{
			Msg: &breakwaterv1.AgentToServer_JobResult{JobResult: res},
		}); err != nil {
			a.log.Error("send JobResult", "job_id", jobID, "err", err)
			return
		}
		if a.cfg.State != nil {
			if err := a.cfg.State.MarkCompleted(jobID, res.Success, res.ErrorMessage); err != nil {
				a.log.Error("mark completed", "job_id", jobID, "err", err)
			}
		}
	}()
}

func (a *Agent) runJob(
	ctx context.Context,
	stream breakwaterv1.ControlService_ChannelClient,
	dataCl breakwaterv1.DataServiceClient,
	restoreCl breakwaterv1.RestoreServiceClient,
	hasher *contentid.Hasher,
	js *breakwaterv1.JobStart,
) *breakwaterv1.JobResult {
	jobID := js.GetJobId()
	res := &breakwaterv1.JobResult{JobId: jobID}

	// Honour cancel promptly.
	if ctx.Err() != nil {
		res.Success = false
		res.ErrorMessage = "cancelled"
		return res
	}

	switch js.GetType() {
	case breakwaterv1.JobType_JOB_TYPE_NOOP:
		res.Success = true
		return res

	case breakwaterv1.JobType_JOB_TYPE_INVENTORY:
		inv := inventory.Collect()
		if err := a.send(stream, &breakwaterv1.AgentToServer{
			Msg: &breakwaterv1.AgentToServer_Inventory{Inventory: inv},
		}); err != nil {
			res.Success = false
			res.ErrorMessage = err.Error()
			return res
		}
		res.Success = true
		return res

	case breakwaterv1.JobType_JOB_TYPE_FILE_BACKUP:
		return a.runFileBackup(ctx, stream, dataCl, hasher, js)

	case breakwaterv1.JobType_JOB_TYPE_RESTORE:
		return a.runRestore(ctx, stream, restoreCl, js)

	default:
		res.Success = false
		res.ErrorMessage = fmt.Sprintf("unsupported job type %v", js.GetType())
		return res
	}
}

func (a *Agent) runFileBackup(
	ctx context.Context,
	stream breakwaterv1.ControlService_ChannelClient,
	dataCl breakwaterv1.DataServiceClient,
	hasher *contentid.Hasher,
	js *breakwaterv1.JobStart,
) *breakwaterv1.JobResult {
	jobID := js.GetJobId()
	res := &breakwaterv1.JobResult{JobId: jobID}

	var params struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(js.GetParamsJson(), &params); err != nil || params.Source == "" {
		res.Success = false
		res.ErrorMessage = "FILE_BACKUP requires params_json.source"
		return res
	}

	stats, err := backup.Run(ctx, backup.Options{
		Source: params.Source,
		JobID:  jobID,
		Hasher: hasher,
		Client: &backup.GRPCClient{DS: dataCl},
		Progress: func(done, total int64, phase, msg string) {
			_ = a.send(stream, &breakwaterv1.AgentToServer{
				Msg: &breakwaterv1.AgentToServer_JobProgress{
					JobProgress: &breakwaterv1.JobProgress{
						JobId: jobID, BytesDone: done, BytesTotal: total, Phase: phase, Message: msg,
					},
				},
			})
		},
	})
	if err != nil {
		res.Success = false
		if ctx.Err() != nil {
			res.ErrorMessage = "cancelled"
		} else {
			res.ErrorMessage = err.Error()
		}
		return res
	}
	res.Success = true
	res.BytesRead = stats.BytesRead
	res.BytesStored = stats.BytesUploaded
	res.SnapshotId = stats.SnapshotID
	return res
}

// restoreParams is the JOB_TYPE_RESTORE params_json contract.
//
//	source_snapshot_id  — catalog (or manifest) snapshot id
//	source_machine_id   — optional; defaults to this machine (own-repo restore)
//	target_path         — directory to restore into
//	conflict_policy     — overwrite | rename | skip
//	root_object_id      — optional; if empty, fetched via GetSnapshot
type restoreParams struct {
	SourceSnapshotID string `json:"source_snapshot_id"`
	SourceMachineID  string `json:"source_machine_id"`
	TargetPath       string `json:"target_path"`
	ConflictPolicy   string `json:"conflict_policy"`
	RootObjectID     string `json:"root_object_id"`
}

func (a *Agent) runRestore(
	ctx context.Context,
	stream breakwaterv1.ControlService_ChannelClient,
	restoreCl breakwaterv1.RestoreServiceClient,
	js *breakwaterv1.JobStart,
) *breakwaterv1.JobResult {
	jobID := js.GetJobId()
	res := &breakwaterv1.JobResult{JobId: jobID}
	if restoreCl == nil {
		res.Success = false
		res.ErrorMessage = "RestoreService client not available"
		return res
	}

	var params restoreParams
	if err := json.Unmarshal(js.GetParamsJson(), &params); err != nil {
		res.Success = false
		res.ErrorMessage = "RESTORE requires valid params_json"
		return res
	}
	if params.TargetPath == "" {
		res.Success = false
		res.ErrorMessage = "RESTORE requires params_json.target_path"
		return res
	}
	if params.SourceSnapshotID == "" && params.RootObjectID == "" {
		res.Success = false
		res.ErrorMessage = "RESTORE requires source_snapshot_id or root_object_id"
		return res
	}
	policy := restore.ConflictPolicy(params.ConflictPolicy)
	if policy == "" {
		policy = restore.ConflictOverwrite
	}
	if !restore.ValidConflict(policy) {
		res.Success = false
		res.ErrorMessage = fmt.Sprintf("invalid conflict_policy %q", params.ConflictPolicy)
		return res
	}

	rootOID := params.RootObjectID
	if rootOID == "" {
		snap, err := restoreCl.GetSnapshot(ctx, &breakwaterv1.GetSnapshotRequest{
			SnapshotId: params.SourceSnapshotID,
		})
		if err != nil {
			res.Success = false
			if ctx.Err() != nil {
				res.ErrorMessage = "cancelled"
			} else {
				res.ErrorMessage = fmt.Sprintf("GetSnapshot: %v", err)
			}
			return res
		}
		rootOID = snap.GetRootObjectId()
	}
	if rootOID == "" {
		res.Success = false
		res.ErrorMessage = "snapshot has empty root_object_id"
		return res
	}

	stats, err := restore.Run(ctx, restore.Options{
		RootObjectID: rootOID,
		TargetRoot:   params.TargetPath,
		Conflict:     policy,
		Reader:       &restore.GRPCReader{Client: restoreCl},
		Progress: func(done int64, phase, msg string) {
			_ = a.send(stream, &breakwaterv1.AgentToServer{
				Msg: &breakwaterv1.AgentToServer_JobProgress{
					JobProgress: &breakwaterv1.JobProgress{
						JobId: jobID, BytesDone: done, Phase: phase, Message: msg,
					},
				},
			})
		},
	})
	if err != nil {
		res.Success = false
		if ctx.Err() != nil {
			res.ErrorMessage = "cancelled"
		} else {
			res.ErrorMessage = err.Error()
		}
		return res
	}
	// Visible skips never make the job fail by themselves — but we surface count.
	if len(stats.Skipped) > 0 {
		a.log.Info("restore completed with skips", "job_id", jobID, "skips", len(stats.Skipped))
	}
	res.Success = true
	res.BytesRead = stats.Bytes
	res.SnapshotId = params.SourceSnapshotID
	return res
}
