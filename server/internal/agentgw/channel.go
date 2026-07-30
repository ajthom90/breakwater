package agentgw

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/oklog/ulid/v2"
)

// controlServer implements breakwater.v1.ControlService.
//
// Agent channel contract (for the stage-4 Windows agent):
//
//   - Dial :9443 with mTLS (enrolled cert); open ControlService.Channel.
//   - First AgentToServer message MUST be Hello with machine_id matching the
//     cert's enrolled machine (mismatch → PermissionDenied; cross-machine isolation).
//   - Expect HelloAck with session_id; then Heartbeat roughly every ≤30s.
//   - Server enforces gRPC keepalive (see KeepaliveServerParameters): Time=30s.
//     Client should enable keepalive with Time≈30s and PermitWithoutStream as needed.
//   - On JobStart: branch on JobStart.type (JOB_TYPE_INVENTORY, JOB_TYPE_NOOP,
//     JOB_TYPE_FILE_BACKUP, …). Stream JobProgress; finish with JobResult (echo job_id).
//   - inventory (JOB_TYPE_INVENTORY): send InventoryReport then JobResult.
//     noop (JOB_TYPE_NOOP): JobResult success immediately.
//   - Reconnect: open a new Channel (supersedes old). Do not re-run a job_id you
//     already completed; server ignores duplicate JobResult for terminal jobs and
//     does not re-send JobStart for jobs already delivered (running/terminal).
//     JobStarts that never left the server buffer are reverted to pending and
//     re-dispatched on the new channel.
//   - UpdateOffer on the wire is reserved; agents may ignore. Server does not send it yet.
//   - JobCancel: stop work for job_id; report JobResult (success=false) when stopped.
//
// Append-only: Channel never accepts agent-originated prune/verify/delete. JobCancel
// cancels work only. Server-only job types cannot be submitted via this surface.
//
// Application-level errors (bad job id, inventory persistence, DB blips) are logged
// and do NOT tear down the stream (S2-F1). Only protocol violations (duplicate Hello,
// machine_id mismatch) are stream-fatal.
type controlServer struct {
	breakwaterv1.UnimplementedControlServiceServer
	gw *Gateway
}

// Channel is the long-lived bidi control stream.
func (c *controlServer) Channel(stream breakwaterv1.ControlService_ChannelServer) error {
	ctx := stream.Context()
	pi, ok := PeerFromContext(ctx)
	if !ok || pi.MachineID == "" {
		return status.Error(codes.PermissionDenied, "not enrolled")
	}
	log := c.gw.Log
	if log == nil {
		log = slog.Default()
	}

	// First message must be Hello.
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first Channel message must be Hello")
	}
	if hello.GetMachineId() == "" {
		return status.Error(codes.InvalidArgument, "Hello.machine_id required")
	}
	// Cross-machine isolation: Hello machine_id MUST match cert-bound machine.
	if hello.GetMachineId() != pi.MachineID {
		log.Warn("Channel Hello machine_id mismatch",
			"cert_machine", pi.MachineID, "hello_machine", hello.GetMachineId(), "fp", pi.CertFP[:16]+"…")
		return status.Error(codes.PermissionDenied, "machine_id does not match enrolled certificate")
	}
	machineID := pi.MachineID

	if c.gw.Registry == nil || c.gw.Engine == nil {
		return status.Error(codes.Internal, "control plane not configured")
	}

	sessionID := ulid.MustNew(ulid.Timestamp(time.Now()), ulid.Monotonic(rand.Reader, 0)).String()
	sess := c.gw.Registry.Register(machineID, sessionID)
	defer func() {
		c.gw.Registry.Unregister(sess)
		// Only mark offline if we are still not online (no superseding session).
		if !c.gw.Registry.IsOnline(machineID) {
			if c.gw.Catalog != nil {
				_ = c.gw.Catalog.SetMachineOffline(context.WithoutCancel(ctx), machineID)
			}
			c.gw.Engine.OnAgentDisconnect(context.WithoutCancel(ctx), machineID)
		}
	}()

	if c.gw.Catalog != nil {
		if err := c.gw.Catalog.SetMachineOnline(ctx, machineID); err != nil {
			log.Error("set machine online", "machine_id", machineID, "err", err)
		}
	}
	log.Info("control channel up", "machine_id", machineID, "session_id", sessionID, "agent_version", hello.GetAgentVersion())

	// Writer goroutine: session.send → stream.Send; tracks delivered JobStarts (S2-F2).
	writeErr := make(chan error, 1)
	go func() {
		for {
			select {
			case <-sess.done:
				writeErr <- nil
				return
			case <-ctx.Done():
				writeErr <- ctx.Err()
				return
			case msg, ok := <-sess.send:
				if !ok {
					writeErr <- nil
					return
				}
				if err := stream.Send(msg); err != nil {
					writeErr <- err
					return
				}
				// Mark JobStart delivered only after stream.Send succeeds.
				if js := msg.GetJobStart(); js != nil {
					sess.noteJobStartDelivered(js.GetJobId())
				}
			}
		}
	}()

	// HelloAck
	ack := &breakwaterv1.ServerToAgent{
		Msg: &breakwaterv1.ServerToAgent_HelloAck{
			HelloAck: &breakwaterv1.HelloAck{
				ServerVersion: c.gw.ServerVersion,
				ServerTime:    timestamppb.Now(),
				SessionId:     sessionID,
			},
		},
	}
	select {
	case sess.send <- ack:
	case <-sess.done:
		return status.Error(codes.Aborted, "session superseded before HelloAck")
	case <-ctx.Done():
		return ctx.Err()
	}

	// Deliver any pending jobs (not already running — engine enforces).
	c.gw.Engine.DeliverPending(ctx, machineID)

	// Reader loop
	readErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				readErr <- err
				return
			}
			if err := c.handleAgentMsg(ctx, machineID, msg, sess, log); err != nil {
				readErr <- err
				return
			}
		}
	}()

	select {
	case err := <-readErr:
		// Unregister in defer closes session and reverts undelivered JobStarts (S2-F2).
		if err == io.EOF || err == nil {
			return nil
		}
		if status.Code(err) != codes.OK && status.Code(err) != codes.Unknown {
			return err
		}
		if err == context.Canceled || ctx.Err() != nil {
			return nil
		}
		if err == io.EOF {
			return nil
		}
		return err
	case err := <-writeErr:
		if err == nil || err == context.Canceled {
			return nil
		}
		return err
	case <-sess.done:
		// Superseded by a newer channel (undelivered already flushed by Register).
		return status.Error(codes.Aborted, "session superseded by newer channel")
	case <-ctx.Done():
		return nil
	}
}

func (c *controlServer) handleAgentMsg(ctx context.Context, machineID string, msg *breakwaterv1.AgentToServer, sess *session, log *slog.Logger) error {
	switch m := msg.Msg.(type) {
	case *breakwaterv1.AgentToServer_Hello:
		// Protocol violation — stream-fatal.
		return status.Error(codes.InvalidArgument, "unexpected Hello after session established")

	case *breakwaterv1.AgentToServer_Heartbeat:
		// Re-assert online (S2-F8): self-heals supersede race where old teardown
		// SetMachineOffline wins after new SetMachineOnline.
		if c.gw.Catalog != nil {
			if err := c.gw.Catalog.SetMachineOnline(ctx, machineID); err != nil {
				log.Error("heartbeat set online", "machine_id", machineID, "err", err)
			}
		}
		// Pending-job retrigger (M2-S3): queue-full / lease-blocked jobs retry
		// without waiting for reconnect.
		if c.gw.Engine != nil {
			c.gw.Engine.DeliverPending(ctx, machineID)
		}
		hb := &breakwaterv1.ServerToAgent{
			Msg: &breakwaterv1.ServerToAgent_HeartbeatAck{
				HeartbeatAck: &breakwaterv1.HeartbeatAck{
					ServerTime: timestamppb.Now(),
				},
			},
		}
		select {
		case sess.send <- hb:
		case <-sess.done:
		case <-ctx.Done():
		}
		return nil

	case *breakwaterv1.AgentToServer_JobProgress:
		p := m.JobProgress
		if p == nil {
			return nil
		}
		// Application-level errors: log, keep stream (S2-F1).
		if err := c.gw.Engine.HandleProgress(ctx, machineID, p.GetJobId(), p.GetBytesDone(), p.GetBytesTotal()); err != nil {
			log.Error("HandleProgress", "machine_id", machineID, "job_id", p.GetJobId(), "err", err)
		}
		return nil

	case *breakwaterv1.AgentToServer_JobResult:
		r := m.JobResult
		if r == nil {
			return nil
		}
		if err := c.gw.Engine.HandleResult(ctx, machineID, scheduler.Result{
			JobID:        r.GetJobId(),
			Success:      r.GetSuccess(),
			ErrorMessage: r.GetErrorMessage(),
			BytesRead:    r.GetBytesRead(),
			BytesStored:  r.GetBytesStored(),
			SnapshotID:   r.GetSnapshotId(),
		}); err != nil {
			log.Error("HandleResult", "machine_id", machineID, "job_id", r.GetJobId(), "err", err)
		}
		return nil

	case *breakwaterv1.AgentToServer_Inventory:
		inv := m.Inventory
		if inv == nil {
			return nil
		}
		items := inventoryToItems(machineID, inv)
		if err := c.gw.Engine.HandleInventory(ctx, machineID, items); err != nil {
			log.Error("persist inventory", "machine_id", machineID, "err", err)
			// Not stream-fatal (S2-F1).
		}
		return nil

	default:
		log.Debug("ignoring unknown AgentToServer message", "machine_id", machineID)
		return nil
	}
}

func inventoryToItems(machineID string, inv *breakwaterv1.InventoryReport) []catalog.InventoryItem {
	var items []catalog.InventoryItem
	for _, v := range inv.GetVolumes() {
		items = append(items, catalog.InventoryItem{
			MachineID:  machineID,
			Kind:       "volume",
			ExternalID: v.GetId(),
			Name:       v.GetMount(),
			Details: map[string]any{
				"mount":      v.GetMount(),
				"size_bytes": v.GetSizeBytes(),
				"fs_type":    v.GetFsType(),
			},
		})
	}
	for _, vm := range inv.GetVms() {
		items = append(items, catalog.InventoryItem{
			MachineID:  machineID,
			Kind:       "vm",
			ExternalID: vm.GetId(),
			Name:       vm.GetName(),
			RCTCapable: vm.GetRctCapable(),
			Details: map[string]any{
				"state": vm.GetState(),
			},
		})
	}
	return items
}

// KeepaliveServerParameters documents and returns gRPC server keepalive for PLAN's 30s.
//
// Client expectation (stage-4 agent):
//
//	grpc.WithKeepaliveParams(keepalive.ClientParameters{
//	    Time:                30 * time.Second,
//	    Timeout:             10 * time.Second,
//	    PermitWithoutStream: true,
//	})
//
// Application-level Heartbeat messages remain useful for last_seen / free_bytes
// telemetry and re-assert online status (S2-F8); transport keepalive keeps the
// HTTP/2 connection from going idle through NATs/LBs.
func KeepaliveServerParameters() (time.Duration, time.Duration) {
	return 30 * time.Second, 10 * time.Second
}

// serverVersionDefault is used when Gateway.ServerVersion is empty.
const serverVersionDefault = "0.0.1-dev"
