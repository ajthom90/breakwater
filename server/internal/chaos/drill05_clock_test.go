package chaos_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/timestamppb"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/chaos"
	"github.com/ajthom90/breakwater/server/internal/clock"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/scheduler"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestChaos05_AgentClockSkew is PLAN chaos drill #5:
// agent clock ±3 days → server clock governs snapshot timestamps and retention;
// a warning is surfaced.
func TestChaos05_AgentClockSkew(t *testing.T) {
	seed := chaos.Seed(t, time.Now().UnixNano())
	t.Logf("chaos#5 seed=%d", seed)
	ctx := context.Background()

	serverNow := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	agentPlus3d := serverNow.Add(3 * 24 * time.Hour)
	agentMinus3d := serverNow.Add(-3 * 24 * time.Hour)

	for _, tc := range []struct {
		name    string
		agentTS time.Time
	}{
		{"agent_plus_3d", agentPlus3d},
		{"agent_minus_3d", agentMinus3d},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runClockSkewCase(t, ctx, serverNow, tc.agentTS)
		})
	}
}

func runClockSkewCase(t *testing.T, ctx context.Context, serverNow, agentTS time.Time) {
	t.Helper()
	tmp := t.TempDir()
	db, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "keys", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := mtls.GenerateServerIdentity("chaos5", []string{"127.0.0.1", "localhost"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverFP := serverID.Fingerprint()
	vm := vault.NewManager(filepath.Join(tmp, "repos"), filepath.Join(tmp, "data"))
	t.Cleanup(func() { _ = vm.CloseAll(ctx) })

	var logBuf bytes.Buffer
	log := slog.New(slog.NewTextHandler(io.MultiWriter(&logBuf, os.Stderr), &slog.HandlerOptions{Level: slog.LevelInfo}))
	auditor := audit.NewWriter(db)
	enrollSvc := &enroll.Service{
		DB: db, Keystore: ks, Vaults: &chaosVaultAdapter{m: vm}, ServerFP: serverFP,
		DefaultPolicy: "01DEFAULTPOLICY000000000000", Log: log,
	}
	locks := scheduler.NewRepoLocks()
	engine := scheduler.NewEngine(db, locks, log)
	reg := agentgw.NewRegistry(log)
	clk := clock.NewFake(serverNow)

	gw := agentgw.New(serverID, enrollSvc, log)
	gw.Auditor = auditor
	gw.AttachControlPlane(db, engine, reg)
	gw.DataService = &agentgw.DataServer{
		Engine: engine, Catalog: db, Keystore: ks, Vaults: vm, Auditor: auditor, Log: log,
		Clock: clk,
	}
	addr, err := gw.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.GracefulStop)

	rawTok, secret, err := enroll.Mint(addr, serverFP)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEnrollToken(ctx, "tok-"+t.Name(), secret, "test", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	agentID, err := mtls.GenerateAgentIdentity("skew-"+t.Name(), 365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(agentID, serverFP))),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20), grpc.MaxCallSendMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	er, err := breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "skew-host", Os: "linux", AgentVersion: "0.0.1", Arch: "amd64",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	machineID := er.MachineId

	// Control channel: Hello → HelloAck, then hold lease open (no JobResult).
	stream, err := breakwaterv1.NewControlServiceClient(conn).Channel(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Hello{Hello: &breakwaterv1.Hello{
			MachineId: machineID, AgentVersion: "0.0.1",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil {
		t.Fatalf("HelloAck: %v", err)
	}
	if ack.GetHelloAck() == nil {
		t.Fatalf("expected HelloAck, got %T", ack.Msg)
	}
	jobStarts := make(chan string, 4)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				return
			}
			if js := msg.GetJobStart(); js != nil {
				jobStarts <- js.GetJobId()
			}
		}
	}()
	// Heartbeat so agent is online.
	_ = stream.Send(&breakwaterv1.AgentToServer{
		Msg: &breakwaterv1.AgentToServer_Heartbeat{Heartbeat: &breakwaterv1.Heartbeat{
			ClientTime: timestamppb.Now(), FreeBytes: 1 << 30,
		}},
	})
	time.Sleep(50 * time.Millisecond)

	params, _ := json.Marshal(map[string]string{"source": "/tmp"})
	jobID, err := engine.Submit(ctx, scheduler.SubmitRequest{
		MachineID: machineID, Type: scheduler.TypeFileBackup, ParamsJSON: string(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-jobStarts:
	case <-time.After(5 * time.Second):
		t.Fatal("no JobStart — agent not online / dispatch failed")
	}
	// Wait for running + lease.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if engine.HasLease(jobID) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !engine.HasLease(jobID) {
		t.Fatal("no vault lease for job")
	}

	// Build a valid tree root in the vault (under the job's lease — open via manager is OK;
	// CommitSnapshot validates root format).
	pw, err := ks.GetRepoPassword(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := vm.Open(ctx, machineID, pw)
	if err != nil {
		t.Fatal(err)
	}
	root, err := writeTreeNoT(ctx, v, map[string]string{"f.txt": "skew-payload"})
	if err != nil {
		t.Fatal(err)
	}

	// FAULT: agent reports FinishedAt ±3 days from server clock.
	skew := serverNow.Sub(agentTS)
	if skew < 0 {
		skew = -skew
	}
	if skew <= agentgw.ClockSkewWarnThreshold {
		t.Fatalf("test skew %v must exceed threshold", skew)
	}
	t.Logf("FAULT injected: agent FinishedAt=%v serverNow=%v skew=%v", agentTS, serverNow, skew)

	ds := breakwaterv1.NewDataServiceClient(conn)
	resp, err := ds.CommitSnapshot(ctx, &breakwaterv1.CommitSnapshotRequest{
		JobId:        jobID,
		Kind:         breakwaterv1.SnapshotKind_SNAPSHOT_KIND_FILE,
		RootObjectId: string(root),
		Source:       "/skew",
		FinishedAt:   timestamppb.New(agentTS),
		BytesRead:    1,
	})
	if err != nil {
		t.Fatalf("CommitSnapshot: %v", err)
	}

	// Catalog created_at = server clock (retention input).
	sn, err := db.SnapshotByID(ctx, resp.SnapshotId)
	if err != nil || sn == nil {
		t.Fatalf("catalog: %v", err)
	}
	if !sn.CreatedAt.Equal(serverNow) {
		t.Fatalf("catalog created_at=%v want server %v (agent sent %v) — agent clock leaked",
			sn.CreatedAt, serverNow, agentTS)
	}
	if sn.CreatedAt.Equal(agentTS) {
		t.Fatal("catalog created_at equals agent FinishedAt")
	}

	// Vault Timestamp = server clock.
	rec, err := v.GetSnapshotRecord(ctx, vault.SnapshotRecordID(resp.ManifestRef))
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Timestamp.Equal(serverNow) {
		t.Fatalf("vault Timestamp=%v want server %v (agent sent %v)", rec.Timestamp, serverNow, agentTS)
	}
	if rec.Timestamp.Equal(agentTS) {
		t.Fatal("vault Timestamp equals agent FinishedAt — server does not govern")
	}
	// Agent time retained for diagnostics only.
	if rec.Extra["agent_finished_at"] == "" {
		t.Fatal("expected agent_finished_at in Extra for diagnostics")
	}
	if rec.Extra["agent_clock_skew"] == "" {
		t.Fatal("expected agent_clock_skew in Extra when skew exceeds threshold")
	}

	// Warning surfaced in logs.
	if !strings.Contains(logBuf.String(), "agent clock skew") {
		t.Fatalf("expected clock skew warning in logs; got:\n%s", logBuf.String())
	}

	// Audit detail includes skew warning (Detail is raw JSON string).
	rows, err := auditor.ListByAction(ctx, audit.ActionSnapshotCommit)
	if err != nil {
		t.Fatal(err)
	}
	foundWarn := false
	for _, r := range rows {
		if r.Target != resp.SnapshotId {
			continue
		}
		if strings.Contains(r.Detail, "clock_skew") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Fatal("audit missing clock_skew_warned detail")
	}

	// Retention uses catalog CreatedAt (= server) — a -3d agent time would place
	// this snap in the wrong GFS bucket; prove CreatedAt is serverNow.
	t.Logf("chaos#5 OK: server_ts=%v agent_sent=%v catalog=%v vault=%v warn=log+audit",
		serverNow, agentTS, sn.CreatedAt, rec.Timestamp)
}

type chaosVaultAdapter struct {
	m *vault.Manager
}

func (a *chaosVaultAdapter) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	v, err := a.m.Create(ctx, repoID, password)
	if err != nil {
		return nil, "", err
	}
	return v.HashingKey(ctx)
}
