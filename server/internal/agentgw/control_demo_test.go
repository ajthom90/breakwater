package agentgw_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// testEnv is a full gateway + control plane for M2 stage 2 tests.
type testEnv struct {
	t        *testing.T
	DB       *catalog.DB
	GW       *agentgw.Gateway
	Engine   *scheduler.Engine
	Registry *agentgw.Registry
	Addr     string
	ServerFP string
	Auditor  *audit.Writer
}

func startControlEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()

	db, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "keys", "master.key"))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	serverID, err := mtls.GenerateServerIdentity("breakwater-m2s2", []string{"127.0.0.1", "localhost"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	serverFP := serverID.Fingerprint()
	vm := vault.NewManager(filepath.Join(tmp, "repos"), filepath.Join(tmp, "data"))
	t.Cleanup(func() { _ = vm.CloseAll(ctx) })

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	auditor := audit.NewWriter(db)
	enrollSvc := &enroll.Service{
		DB: db, Keystore: ks, Vaults: &realVault{m: vm}, ServerFP: serverFP,
		DefaultPolicy: "01DEFAULTPOLICY000000000000", Log: log,
	}
	locks := scheduler.NewRepoLocks()
	engine := scheduler.NewEngine(db, locks, log)
	reg := agentgw.NewRegistry(log)

	gw := agentgw.New(serverID, enrollSvc, log)
	gw.Auditor = auditor
	gw.ServerVersion = "0.0.1-m2s2-test"
	gw.AttachControlPlane(db, engine, reg)
	addr, err := gw.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(gw.GracefulStop)

	return &testEnv{t: t, DB: db, GW: gw, Engine: engine, Registry: reg, Addr: addr, ServerFP: serverFP, Auditor: auditor}
}

func (e *testEnv) mintAndEnroll(hostname string) (machineID string, agentID *mtls.Identity, conn *grpc.ClientConn) {
	e.t.Helper()
	ctx := context.Background()
	rawTok, secret, err := enroll.Mint(e.Addr, e.ServerFP)
	if err != nil {
		e.t.Fatal(err)
	}
	tokID := "tok-" + hostname
	if err := e.DB.InsertEnrollToken(ctx, tokID, secret, "test", time.Now().UTC().Add(time.Hour)); err != nil {
		e.t.Fatal(err)
	}
	agentID, err = mtls.GenerateAgentIdentity(hostname, 365*24*time.Hour)
	if err != nil {
		e.t.Fatal(err)
	}
	conn, err = grpc.NewClient(e.Addr, grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(agentID, e.ServerFP))))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = conn.Close() })

	resp, err := breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: hostname, Os: "linux", OsVersion: "test", AgentVersion: "0.0.1-fake", Arch: "amd64",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err != nil {
		e.t.Fatalf("enroll: %v", err)
	}
	return resp.MachineId, agentID, conn
}

// fakeAgent is an in-test agent with a single reader goroutine (no dual Recv races).
type fakeAgent struct {
	t         *testing.T
	machineID string
	stream    breakwaterv1.ControlService_ChannelClient

	mu        sync.Mutex
	jobStarts []string
	// onJob overrides default inventory/noop handling when non-nil.
	onJob func(job *breakwaterv1.JobStart) error

	// skipResult when true (with onJob nil) records JobStart but does not send JobResult.
	skipResult bool

	stop chan struct{}
	wg   sync.WaitGroup
}

func openChannel(t *testing.T, conn *grpc.ClientConn, machineID string) *fakeAgent {
	t.Helper()
	ctx := context.Background()
	stream, err := breakwaterv1.NewControlServiceClient(conn).Channel(ctx)
	if err != nil {
		t.Fatalf("Channel: %v", err)
	}
	if err := stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Hello{
			Hello: &breakwaterv1.Hello{MachineId: machineID, AgentVersion: "0.0.1-fake"},
		},
	}); err != nil {
		t.Fatalf("Hello: %v", err)
	}

	// Block for HelloAck before starting the background reader.
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("HelloAck recv: %v", err)
	}
	if msg.GetHelloAck() == nil {
		t.Fatalf("expected HelloAck, got %T", msg.Msg)
	}

	a := &fakeAgent{
		t:         t,
		machineID: machineID,
		stream:    stream,
		stop:      make(chan struct{}),
	}
	a.wg.Add(1)
	go a.readLoop()
	t.Cleanup(a.close)
	return a
}

func (a *fakeAgent) readLoop() {
	defer a.wg.Done()
	for {
		msg, err := a.stream.Recv()
		if err != nil {
			return
		}
		a.handleServer(msg)
	}
}

func (a *fakeAgent) heartbeat() {
	a.t.Helper()
	if err := a.stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Heartbeat{
			Heartbeat: &breakwaterv1.Heartbeat{ClientTime: timestamppb.Now(), FreeBytes: 1 << 30},
		},
	}); err != nil {
		a.t.Fatalf("heartbeat: %v", err)
	}
	// Give the server time to TouchLastSeen + HeartbeatAck.
	time.Sleep(50 * time.Millisecond)
}

func (a *fakeAgent) handleServer(msg *breakwaterv1.ServerToAgent) {
	if msg == nil {
		return
	}
	switch m := msg.Msg.(type) {
	case *breakwaterv1.ServerToAgent_HeartbeatAck, *breakwaterv1.ServerToAgent_HelloAck:
		return
	case *breakwaterv1.ServerToAgent_JobStart:
		js := m.JobStart
		a.mu.Lock()
		a.jobStarts = append(a.jobStarts, js.GetJobId())
		onJob := a.onJob
		skip := a.skipResult
		a.mu.Unlock()
		if onJob != nil {
			if err := onJob(js); err != nil {
				a.t.Errorf("onJob: %v", err)
			}
			return
		}
		if skip {
			return
		}
		a.defaultHandleJob(js)
	case *breakwaterv1.ServerToAgent_JobCancel:
		// Server-initiated cancel; work stop is agent-local.
	case *breakwaterv1.ServerToAgent_UpdateOffer:
		// Reserved — ignore gracefully.
	}
}

func (a *fakeAgent) defaultHandleJob(js *breakwaterv1.JobStart) {
	// Stage-4 agent contract: branch on JobStart.type (S2-F7), not params_json.kind.
	if js.GetType() == breakwaterv1.JobType_JOB_TYPE_INVENTORY {
		_ = a.stream.Send(&breakwaterv1.AgentToServer{
			Msg: &breakwaterv1.AgentToServer_Inventory{
				Inventory: &breakwaterv1.InventoryReport{
					Volumes: []*breakwaterv1.VolumeInfo{
						{Id: "vol-c", Mount: "C:\\", SizeBytes: 100_000_000_000, FsType: "ntfs"},
					},
					Vms: []*breakwaterv1.VMInfo{
						{Id: "vm-1", Name: "dc01", RctCapable: true, State: "Running"},
					},
				},
			},
		})
	}
	_ = a.stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_JobResult{
			JobResult: &breakwaterv1.JobResult{JobId: js.GetJobId(), Success: true},
		},
	})
}

func (a *fakeAgent) waitJobStarts(n int, timeout time.Duration) []string {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		if len(a.jobStarts) >= n {
			out := append([]string(nil), a.jobStarts...)
			a.mu.Unlock()
			return out
		}
		a.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.jobStarts...)
}

func (a *fakeAgent) jobStartCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.jobStarts)
}

func (a *fakeAgent) close() {
	select {
	case <-a.stop:
		return
	default:
		close(a.stop)
	}
	_ = a.stream.CloseSend()
	// Wait briefly for reader to exit on EOF/cancel.
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// TestM2S2_ControlPlaneDemo is the stage-2 demo gate:
// enroll → Channel → online → inventory job → terminal history → offline →
// reconnect + duplicate JobResult is idempotent.
func TestM2S2_ControlPlaneDemo(t *testing.T) {
	env := startControlEnv(t)
	ctx := context.Background()

	machineID, _, conn := env.mintAndEnroll("fake-linux-m2s2")

	// --- Open channel; machine goes online ---
	agent := openChannel(t, conn, machineID)
	agent.heartbeat()
	waitOnline(t, env.DB, machineID, 3*time.Second)

	m, err := env.DB.MachineByID(ctx, machineID)
	if err != nil || m == nil {
		t.Fatalf("machine: %v", err)
	}
	if m.Status != catalog.MachineStatusActive {
		t.Fatalf("status=%s want active", m.Status)
	}
	if m.LastSeenAt == nil {
		t.Fatal("last_seen_at not set")
	}
	t.Logf("machine online status=%s last_seen=%s", m.Status, m.LastSeenAt.Format(time.RFC3339Nano))

	// --- Submit inventory job ---
	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID,
		Type:      scheduler.TypeInventory,
		Initiator: "test-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	starts := agent.waitJobStarts(1, 3*time.Second)
	if len(starts) < 1 || starts[0] != jobID {
		t.Fatalf("expected JobStart %s; got %v", jobID, starts)
	}

	waitJobState(t, env.Engine, jobID, catalog.JobStateSuccess, 3*time.Second)

	inv, err := env.DB.ListMachineInventory(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv) < 2 {
		t.Fatalf("inventory rows=%d want ≥2 (volume+vm)", len(inv))
	}
	var sawVol, sawVM bool
	for _, it := range inv {
		if it.Kind == "volume" && it.ExternalID == "vol-c" {
			sawVol = true
		}
		if it.Kind == "vm" && it.ExternalID == "vm-1" && it.RCTCapable {
			sawVM = true
		}
	}
	if !sawVol || !sawVM {
		t.Fatalf("inventory incomplete: %+v", inv)
	}

	jobs, err := env.DB.ListJobsByMachine(ctx, machineID, 10)
	if err != nil || len(jobs) < 1 {
		t.Fatalf("jobs: %v len=%d", err, len(jobs))
	}
	if jobs[0].ID != jobID || jobs[0].State != catalog.JobStateSuccess {
		t.Fatalf("job history: %+v", jobs[0])
	}
	t.Logf("inventory job %s success; inventory=%d rows", jobID, len(inv))

	// --- Disconnect → offline ---
	agent.close()
	waitOffline(t, env.DB, machineID, 3*time.Second)
	m, _ = env.DB.MachineByID(ctx, machineID)
	if m.Status != catalog.MachineStatusEnrolled {
		t.Fatalf("after disconnect status=%s want enrolled", m.Status)
	}
	t.Logf("machine offline status=%s", m.Status)

	// --- Reconnect ---
	agent2 := openChannel(t, conn, machineID)
	agent2.heartbeat()
	waitOnline(t, env.DB, machineID, 3*time.Second)

	// noop + duplicate JobResult idempotency
	noopID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeNoop, Initiator: "test-demo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := agent2.waitJobStarts(1, 3*time.Second); len(got) < 1 {
		t.Fatalf("no JobStart for noop: %v", got)
	}
	waitJobState(t, env.Engine, noopID, catalog.JobStateSuccess, 3*time.Second)

	if err := agent2.stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_JobResult{
			JobResult: &breakwaterv1.JobResult{
				JobId: noopID, Success: false, ErrorMessage: "duplicate should be ignored",
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	j, err := env.Engine.Job(ctx, noopID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != catalog.JobStateSuccess {
		t.Fatalf("duplicate JobResult mutated state to %s", j.State)
	}

	// Running job must not receive a second JobStart on DeliverPending.
	agent2.mu.Lock()
	agent2.skipResult = true
	agent2.onJob = nil
	startsBefore := len(agent2.jobStarts)
	agent2.mu.Unlock()

	heldID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeNoop,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = agent2.waitJobStarts(startsBefore+1, 3*time.Second)
	j, _ = env.Engine.Job(ctx, heldID)
	if j.State != catalog.JobStateRunning {
		t.Fatalf("held job state=%s", j.State)
	}
	env.Engine.DeliverPending(ctx, machineID)
	time.Sleep(80 * time.Millisecond)
	if agent2.jobStartCount() != startsBefore+1 {
		t.Fatalf("redispatched running job: before=%d after=%d", startsBefore, agent2.jobStartCount())
	}
	// Complete held job.
	_ = agent2.stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_JobResult{
			JobResult: &breakwaterv1.JobResult{JobId: heldID, Success: true},
		},
	})
	waitJobState(t, env.Engine, heldID, catalog.JobStateSuccess, 3*time.Second)

	enrolls, err := env.Auditor.ListByAction(ctx, audit.ActionMachineEnroll)
	if err != nil {
		t.Fatal(err)
	}
	if len(enrolls) < 1 {
		t.Fatal("expected enroll audit")
	}

	t.Log("M2S2 DEMO PASSED: online → inventory → jobs history → offline → reconnect idempotent")
}

// TestM2S2_HelloMachineMismatch rejects Hello.machine_id ≠ cert machine.
func TestM2S2_HelloMachineMismatch(t *testing.T) {
	env := startControlEnv(t)
	machineID, _, conn := env.mintAndEnroll("mismatch-host")
	stream, err := breakwaterv1.NewControlServiceClient(conn).Channel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Hello{
			Hello: &breakwaterv1.Hello{MachineId: "01NOTTHEMACHINE000000000000", AgentVersion: "x"},
		},
	})
	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected rejection for machine_id mismatch")
	}
	if st, ok := status.FromError(err); ok {
		if st.Code() != codes.PermissionDenied && st.Code() != codes.Canceled && st.Code() != codes.Unavailable {
			// PermissionDenied is ideal; some stacks surface as transport close.
			t.Logf("status code=%v (machine was %s)", st.Code(), machineID)
		}
		if st.Code() == codes.PermissionDenied {
			t.Logf("mismatch rejected with PermissionDenied: %v", err)
			return
		}
	}
	if err == io.EOF {
		t.Logf("mismatch closed stream (EOF); machine was %s", machineID)
		return
	}
	// Accept any error as rejection.
	t.Logf("mismatch rejected: %v", err)
}

// TestM2S2_AgentCannotSubmitPrune ensures engine rejects prune (channel offers no path).
func TestM2S2_AgentCannotSubmitPrune(t *testing.T) {
	env := startControlEnv(t)
	machineID, _, _ := env.mintAndEnroll("prune-probe")
	_, err := env.Engine.Submit(context.Background(), scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypePrune,
	})
	if err == nil {
		t.Fatal("prune must not be submittable for agent dispatch")
	}
	if !scheduler.IsServerOnly(scheduler.TypePrune) {
		t.Fatal()
	}
}

// TestM2S2_OfflineQueueDeliversOnReconnect
func TestM2S2_OfflineQueueDeliversOnReconnect(t *testing.T) {
	env := startControlEnv(t)
	ctx := context.Background()
	machineID, _, conn := env.mintAndEnroll("queue-host")

	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeNoop,
	})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := env.Engine.Job(ctx, jobID)
	if j.State != catalog.JobStatePending {
		t.Fatalf("want pending, got %s", j.State)
	}

	agent := openChannel(t, conn, machineID)
	starts := agent.waitJobStarts(1, 3*time.Second)
	if len(starts) != 1 || starts[0] != jobID {
		t.Fatalf("starts=%v want %s", starts, jobID)
	}
	waitJobState(t, env.Engine, jobID, catalog.JobStateSuccess, 3*time.Second)
}

// TestM2S2_DisconnectReleasesRunningLease
func TestM2S2_DisconnectReleasesRunningLease(t *testing.T) {
	env := startControlEnv(t)
	ctx := context.Background()
	machineID, _, conn := env.mintAndEnroll("lease-host")

	agent := openChannel(t, conn, machineID)
	agent.mu.Lock()
	agent.skipResult = true
	agent.mu.Unlock()

	// file backup takes a shared vault lease
	jobID, err := env.Engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = agent.waitJobStarts(1, 3*time.Second)
	if env.Engine.LeaseCount() != 1 {
		t.Fatalf("lease count=%d", env.Engine.LeaseCount())
	}

	agent.close()
	waitOffline(t, env.DB, machineID, 3*time.Second)
	waitJobState(t, env.Engine, jobID, catalog.JobStateFailed, 3*time.Second)
	if env.Engine.LeaseCount() != 0 {
		t.Fatalf("lease leak after disconnect: %d", env.Engine.LeaseCount())
	}
}

func waitOnline(t *testing.T, db *catalog.DB, id string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m, err := db.MachineByID(context.Background(), id)
		if err == nil && m != nil && m.Status == catalog.MachineStatusActive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("machine %s not online within %s", id, d)
}

func waitOffline(t *testing.T, db *catalog.DB, id string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		m, err := db.MachineByID(context.Background(), id)
		if err == nil && m != nil && m.Status == catalog.MachineStatusEnrolled {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, _ := db.MachineByID(context.Background(), id)
	st := ""
	if m != nil {
		st = m.Status
	}
	t.Fatalf("machine %s not offline within %s (status=%s)", id, d, st)
}

func waitJobState(t *testing.T, e *scheduler.Engine, id, want string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last string
	for time.Now().Before(deadline) {
		j, err := e.Job(context.Background(), id)
		if err == nil && j != nil {
			last = j.State
			if j.State == want {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s state=%s want %s within %s", id, last, want, d)
}
