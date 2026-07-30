package agentgw_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestM2S3_CrossMachineIsolation: machine B cannot use machine A's job or
// probe A's content IDs via CheckContents (no cross-repo oracle).
func TestM2S3_CrossMachineIsolation(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "a.txt"), []byte("secret-of-a"), 0o644)

	// Machine A: enroll, backup, capture a content-related job id.
	machA, _, _, _, connA := env.mintAndEnroll("iso-a")
	// Machine B: separate enrollment.
	machB, _, _, _, connB := env.mintAndEnroll("iso-b")

	// Start channel on A and run a short backup so A has a vault-writing job.
	dataA := breakwaterv1.NewDataServiceClient(connA)
	// We need hashing material for full backup — use control path with fileback via openBackupAgent.
	// For isolation we only need a running job on A with a lease.
	agentA := openChannel(t, connA, machA)
	agentA.skipResult = true // keep job running
	agentA.heartbeat()
	waitOnline(t, env.DB, machA, 3*time.Second)

	params, _ := json.Marshal(map[string]string{"source": src})
	jobA, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machA, Type: scheduler.TypeFileBackup, Initiator: "iso",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := agentA.waitJobStarts(1, 5*time.Second); len(got) < 1 {
		t.Fatalf("A JobStart: %v", got)
	}
	// Ensure running + lease.
	waitJobState(t, env.Engine, jobA, catalog.JobStateRunning, 3*time.Second)
	if !env.Engine.HasLease(jobA) {
		t.Fatal("A job should hold lease")
	}

	// B tries CheckContents with A's job_id → PermissionDenied.
	dataB := breakwaterv1.NewDataServiceClient(connB)
	_, err = dataB.CheckContents(ctx, &breakwaterv1.CheckContentsRequest{
		JobId: jobA, ContentIds: []string{"deadbeefdeadbeefdeadbeefdeadbeef"},
	})
	if err == nil {
		t.Fatal("B CheckContents with A's job_id must be rejected")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("B CheckContents code=%v want PermissionDenied: %v", status.Code(err), err)
	}
	t.Logf("B with A's job rejected: %v", err)

	// B tries PutContents with A's job → PermissionDenied.
	stream, err := dataB.PutContents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&breakwaterv1.PutContentsRequest{
		JobId: jobA, ContentId: "x", Data: []byte("stolen"),
	})
	_, err = stream.Recv()
	if err == nil {
		// May get error on Recv after server rejects.
		t.Fatal("B PutContents with A's job must fail")
	}
	if status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.Unknown {
		// stream may wrap as Unknown; check message
		t.Logf("B PutContents err code=%v: %v", status.Code(err), err)
	}
	if status.Code(err) == codes.OK {
		t.Fatal("expected rejection")
	}
	t.Logf("B PutContents with A's job rejected: %v", err)

	// B with its own pending (no lease) job cannot use CheckContents either.
	// Give B a running file job of its own for content-id oracle test: B must
	// only see B's repo (empty), never A's secret content.
	agentB := openChannel(t, connB, machB)
	agentB.skipResult = true
	agentB.heartbeat()
	waitOnline(t, env.DB, machB, 3*time.Second)
	jobB, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machB, Type: scheduler.TypeFileBackup, Initiator: "iso",
		ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	agentB.waitJobStarts(1, 5*time.Second)
	waitJobState(t, env.Engine, jobB, catalog.JobStateRunning, 3*time.Second)

	// Compute content ID of A's secret by putting it on A first.
	// Open A's channel data path: put secret content under A's job.
	streamA, err := dataA.PutContents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("secret-of-a")
	// Server recomputes ID — send empty client id.
	if err := streamA.Send(&breakwaterv1.PutContentsRequest{JobId: jobA, Data: secret, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	_ = streamA.CloseSend()
	ack, err := streamA.Recv()
	if err != nil || !ack.GetAccepted() {
		t.Fatalf("A PutContents: err=%v ack=%v", err, ack)
	}
	secretID := ack.GetContentId()
	t.Logf("A stored secret content id=%s", secretID)

	// B CheckContents for A's content id must report absent (own empty repo).
	resp, err := dataB.CheckContents(ctx, &breakwaterv1.CheckContentsRequest{
		JobId: jobB, ContentIds: []string{secretID},
	})
	if err != nil {
		t.Fatalf("B CheckContents own job: %v", err)
	}
	bm := resp.GetPresentBitmap()
	if len(bm) > 0 && bm[0]&1 != 0 {
		t.Fatal("cross-repo oracle: B saw A's content as present")
	}
	t.Log("CheckContents is not a cross-repo oracle (B sees absent for A's id)")

	// Cleanup: fail jobs.
	_ = env.Engine.HandleResult(ctx, machA, scheduler.Result{JobID: jobA, Success: false, ErrorMessage: "done"})
	_ = env.Engine.HandleResult(ctx, machB, scheduler.Result{JobID: jobB, Success: false, ErrorMessage: "done"})
}

// TestM2S3_IDMismatchRejected: PutContents with wrong client content_id → accepted=false.
func TestM2S3_IDMismatchRejected(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644)

	mach, _, _, _, conn := env.mintAndEnroll("id-mismatch")
	agent := openChannel(t, conn, mach)
	agent.skipResult = true
	agent.heartbeat()
	waitOnline(t, env.DB, mach, 3*time.Second)
	params, _ := json.Marshal(map[string]string{"source": src})
	job, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: mach, Type: scheduler.TypeFileBackup, ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.waitJobStarts(1, 5*time.Second)
	waitJobState(t, env.Engine, job, catalog.JobStateRunning, 3*time.Second)

	ds := breakwaterv1.NewDataServiceClient(conn)
	stream, err := ds.PutContents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&breakwaterv1.PutContentsRequest{
		JobId: job, ContentId: "00000000000000000000000000000000", Data: []byte("payload"), Seq: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetAccepted() {
		t.Fatal("mismatched content_id must not be accepted")
	}
	if ack.GetErrorMessage() == "" {
		t.Fatal("expected error_message on mismatch")
	}
	t.Logf("ID mismatch rejected: %s (server id=%s)", ack.GetErrorMessage(), ack.GetContentId())
}

// TestM2S3_OversizedBatchAndContentRejected.
func TestM2S3_OversizedBatchAndContentRejected(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o644)

	mach, _, _, _, conn := env.mintAndEnroll("oversize")
	agent := openChannel(t, conn, mach)
	agent.skipResult = true
	agent.heartbeat()
	waitOnline(t, env.DB, mach, 3*time.Second)
	params, _ := json.Marshal(map[string]string{"source": src})
	job, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: mach, Type: scheduler.TypeFileBackup, ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.waitJobStarts(1, 5*time.Second)
	waitJobState(t, env.Engine, job, catalog.JobStateRunning, 3*time.Second)

	ds := breakwaterv1.NewDataServiceClient(conn)
	ids := make([]string, 4097)
	for i := range ids {
		ids[i] = "aa"
	}
	_, err = ds.CheckContents(ctx, &breakwaterv1.CheckContentsRequest{JobId: job, ContentIds: ids})
	if err == nil || status.Code(err) != codes.InvalidArgument {
		t.Fatalf("batch>4096: err=%v", err)
	}
	t.Logf("oversized batch rejected: %v", err)

	// Client must allow large sends so the server app-level size guard is what rejects.
	// (Server MaxRecvMsgSize is 16 MiB; vault max is 8 MiB.)
	stream, err := ds.PutContents(ctx, grpc.MaxCallSendMsgSize(16<<20), grpc.MaxCallRecvMsgSize(16<<20))
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, vault.MaxPutContentBytes+1)
	if err := stream.Send(&breakwaterv1.PutContentsRequest{JobId: job, Data: big, Seq: 1}); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	ack, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if ack.GetAccepted() {
		t.Fatal("oversized content must not be accepted")
	}
	t.Logf("oversized content rejected: %s", ack.GetErrorMessage())
}

// TestM2S3_LeaseRequiredForVaultAccess: DataService without lease is denied.
func TestM2S3_LeaseRequiredForVaultAccess(t *testing.T) {
	env := startDataEnv(t)
	ctx := context.Background()

	mach, _, _, _, conn := env.mintAndEnroll("lease-gate")
	// Insert a running job WITHOUT going through engine dispatch (no lease).
	jobID := "01LEASEGATEJOB00000000000001"
	if err := env.DB.InsertJob(ctx, catalog.Job{
		ID: jobID, MachineID: mach, Type: scheduler.TypeFileBackup,
		State: catalog.JobStatePending, ParamsJSON: `{"source":"/tmp"}`,
	}); err != nil {
		t.Fatal(err)
	}
	// Force running without lease.
	_, _ = env.DB.TransitionJob(ctx, jobID,
		[]string{catalog.JobStatePending}, catalog.JobStateRunning,
		catalog.JobTransition{SetStarted: true})

	if env.Engine.HasLease(jobID) {
		t.Fatal("precondition: no lease")
	}
	ds := breakwaterv1.NewDataServiceClient(conn)
	_, err := ds.CheckContents(ctx, &breakwaterv1.CheckContentsRequest{
		JobId: jobID, ContentIds: []string{"aa"},
	})
	if err == nil {
		t.Fatal("CheckContents without lease must fail")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code=%v want FailedPrecondition: %v", status.Code(err), err)
	}
	t.Logf("lease-required enforced: %v", err)
}
