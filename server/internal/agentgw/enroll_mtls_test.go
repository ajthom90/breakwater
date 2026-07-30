package agentgw_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestM1_EnrollmentAndWrongCertRejection is the M1 demo gate:
// fake Linux client enrolls over real protobuf; wrong-cert client is rejected.
func TestM1_EnrollmentAndWrongCertRejection(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	db, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	defer db.Close()

	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "keys", "master.key"))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}

	serverID, err := mtls.GenerateServerIdentity("breakwater-test", []string{"127.0.0.1", "localhost"}, 24*time.Hour)
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	serverFP := serverID.Fingerprint()

	vm := vault.NewManager(filepath.Join(tmp, "repos"), filepath.Join(tmp, "data"))
	defer vm.CloseAll(ctx)

	auditor := audit.NewWriter(db)
	enrollSvc := &enroll.Service{
		DB:            db,
		Keystore:      ks,
		Vaults:        &realVault{m: vm},
		ServerFP:      serverFP,
		DefaultPolicy: "01DEFAULTPOLICY000000000000",
		Log:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	gw := agentgw.New(serverID, enrollSvc, enrollSvc.Log)
	gw.Auditor = auditor
	gw.TestDataService = &agentgw.PostEnrollProbe{}
	addr, err := gw.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start gateway: %v", err)
	}
	defer gw.GracefulStop()

	// Mint enrollment token (server FP inside token — zero TOFU).
	rawTok, secret, err := enroll.Mint(addr, serverFP)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := db.InsertEnrollToken(ctx, "test-tok-1", secret, "test", time.Now().UTC().Add(24*time.Hour)); err != nil {
		t.Fatalf("insert token: %v", err)
	}

	// --- Fake Linux client: correct enrollment over generated protobuf stubs ---
	agentID, err := mtls.GenerateAgentIdentity("fake-linux-client", 365*24*time.Hour)
	if err != nil {
		t.Fatalf("agent identity: %v", err)
	}

	clientTLS := mtls.ClientTLSConfig(agentID, serverFP)
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(clientTLS)),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	ec := breakwaterv1.NewEnrollmentServiceClient(conn)
	resp, err := ec.Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname:     "fake-linux-01",
			Os:           "linux",
			OsVersion:    "test",
			AgentVersion: "0.0.1-dev",
			Arch:         "amd64",
		},
		ClientCertPem: agentID.CertPEM, // matches connection
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if resp.MachineId == "" || len(resp.HashingKey) == 0 {
		t.Fatalf("bad enroll response: %+v", resp)
	}
	if resp.ServerCertFingerprint != serverFP {
		t.Fatalf("server fp mismatch")
	}
	t.Logf("enrolled machine_id=%s hashingKeyLen=%d", resp.MachineId, len(resp.HashingKey))

	// Audit: successful enroll produces a verifiable machine.enroll row.
	enrollRows, err := auditor.ListByAction(ctx, audit.ActionMachineEnroll)
	if err != nil {
		t.Fatalf("list enroll audits: %v", err)
	}
	if len(enrollRows) < 1 {
		t.Fatal("expected machine.enroll audit row after success")
	}
	foundSuccess := false
	for _, r := range enrollRows {
		if r.Target == resp.MachineId && r.Actor == agentID.Fingerprint() {
			var d map[string]any
			_ = json.Unmarshal([]byte(r.Detail), &d)
			if d["outcome"] == "success" {
				foundSuccess = true
			}
		}
	}
	if !foundSuccess {
		t.Fatalf("no success audit for machine %s; rows=%+v", resp.MachineId, enrollRows)
	}
	if err := auditor.VerifyChain(ctx); err != nil {
		t.Fatalf("audit chain after enroll: %v", err)
	}

	// Machine appears in catalog with connection cert FP.
	m, err := db.MachineByID(ctx, resp.MachineId)
	if err != nil || m == nil {
		t.Fatalf("machine missing from catalog: %v", err)
	}
	if m.CertFP != agentID.Fingerprint() {
		t.Fatalf("catalog cert_fp %s != agent %s", m.CertFP, agentID.Fingerprint())
	}
	if m.Hostname != "fake-linux-01" {
		t.Fatalf("hostname: %s", m.Hostname)
	}

	// Stored hashing key + algorithm match vault format / enroll response (R2-5).
	storedHK, storedAlgo, err := ks.GetHashingKey(ctx, resp.MachineId)
	if err != nil {
		t.Fatalf("GetHashingKey: %v", err)
	}
	if string(storedHK) != string(resp.HashingKey) {
		t.Fatal("stored hashing key != enroll response")
	}
	if resp.HashingAlgorithm == "" {
		t.Fatal("enroll response missing hashing_algorithm")
	}
	if storedAlgo != resp.HashingAlgorithm {
		t.Fatalf("stored algo %q != response %q", storedAlgo, resp.HashingAlgorithm)
	}

	// Post-enroll: known cert can call DataService.CheckContents (pin probe).
	dc := breakwaterv1.NewDataServiceClient(conn)
	_, err = dc.CheckContents(ctx, &breakwaterv1.CheckContentsRequest{JobId: "probe"})
	if err != nil {
		t.Fatalf("CheckContents with enrolled cert: %v", err)
	}
	t.Logf("post-enroll probe ok machine=%s", resp.MachineId)

	// --- Wrong-cert client rejected ---
	wrongID, err := mtls.GenerateAgentIdentity("attacker", 24*time.Hour)
	if err != nil {
		t.Fatalf("wrong identity: %v", err)
	}
	wrongTLS := mtls.ClientTLSConfig(wrongID, serverFP)
	wrongConn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(credentials.NewTLS(wrongTLS)),
	)
	if err != nil {
		t.Fatalf("wrong dial: %v", err)
	}
	defer wrongConn.Close()

	_, err = breakwaterv1.NewDataServiceClient(wrongConn).CheckContents(ctx, &breakwaterv1.CheckContentsRequest{JobId: "should-fail"})
	if err == nil {
		t.Fatal("expected wrong-cert client to be rejected")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.PermissionDenied {
		t.Fatalf("wrong-cert want PermissionDenied, got %v", err)
	}
	t.Logf("wrong-cert rejected as expected: %v", err)

	// Wrong-cert rejection is audited (auth.fail).
	authFails, err := auditor.ListByAction(ctx, audit.ActionAuthFail)
	if err != nil {
		t.Fatalf("list auth.fail: %v", err)
	}
	if len(authFails) < 1 {
		t.Fatal("expected auth.fail audit row for wrong-cert rejection")
	}
	if err := auditor.VerifyChain(ctx); err != nil {
		t.Fatalf("audit chain after wrong-cert: %v", err)
	}
	t.Logf("wrong-cert audit rows=%d chain OK", len(authFails))

	// Wrong server fingerprint on client side rejected.
	badPinTLS := mtls.ClientTLSConfig(agentID, "0000000000000000000000000000000000000000000000000000000000000000")
	badConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(badPinTLS)))
	if err != nil {
		t.Fatalf("bad pin dial setup: %v", err)
	}
	defer badConn.Close()
	_, err = breakwaterv1.NewDataServiceClient(badConn).CheckContents(ctx, &breakwaterv1.CheckContentsRequest{JobId: "x"})
	if err == nil {
		t.Fatal("expected server cert pin mismatch to fail")
	}
	t.Logf("bad server pin rejected as expected: %v", err)

	// Plaintext (no TLS) must fail.
	plain, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("plain dial: %v", err)
	}
	defer plain.Close()
	_, err = breakwaterv1.NewEnrollmentServiceClient(plain).Enroll(ctx, &breakwaterv1.EnrollRequest{Token: "x"})
	if err == nil {
		t.Fatal("expected plaintext to fail")
	}
	t.Logf("plaintext rejected as expected: %v", err)

	// Token reuse fails (same connection cert already enrolled).
	_, err = ec.Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "reuse",
			Os:       "linux",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expected token reuse to fail")
	}
	t.Logf("token reuse rejected: %v", err)

	t.Log("M1 DEMO PASSED: fake client enrolls over protobuf; wrong-cert client rejected; audit chain OK")
}

// TestEnroll_BodyCertMismatch rejects when request-body PEM ≠ TLS peer (B2).
func TestEnroll_BodyCertMismatch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	db, err := catalog.Open(filepath.Join(tmp, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "keys", "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := mtls.GenerateServerIdentity("bw", []string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverFP := serverID.Fingerprint()
	vm := vault.NewManager(filepath.Join(tmp, "repos"), filepath.Join(tmp, "data"))
	defer vm.CloseAll(ctx)

	auditor := audit.NewWriter(db)
	svc := &enroll.Service{
		DB: db, Keystore: ks, Vaults: &realVault{m: vm}, ServerFP: serverFP,
		Log: slog.Default(),
	}
	gw := agentgw.New(serverID, svc, slog.Default())
	gw.Auditor = auditor
	addr, err := gw.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer gw.GracefulStop()

	rawTok, secret, err := enroll.Mint(addr, serverFP)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEnrollToken(ctx, "tok-mismatch", secret, "t", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	connID, err := mtls.GenerateAgentIdentity("conn", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	bodyID, err := mtls.GenerateAgentIdentity("body", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if connID.Fingerprint() == bodyID.Fingerprint() {
		t.Fatal("expected different certs")
	}

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(connID, serverFP))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	ec := breakwaterv1.NewEnrollmentServiceClient(conn)
	_, err = ec.Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "mismatch-host",
			Os:       "linux",
		},
		ClientCertPem: bodyID.CertPEM, // deliberately different from TLS peer
	})
	if err == nil {
		t.Fatal("expected body≠connection cert rejection")
	}
	t.Logf("body-cert mismatch rejected: %v", err)

	// Rejected enroll is audited with outcome=rejected.
	rows, err := auditor.ListByAction(ctx, audit.ActionMachineEnroll)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 1 {
		t.Fatal("expected machine.enroll audit for rejected attempt")
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(rows[0].Detail), &d); err != nil {
		t.Fatal(err)
	}
	if d["outcome"] != "rejected" {
		t.Fatalf("detail outcome=%v want rejected", d["outcome"])
	}
	if err := auditor.VerifyChain(ctx); err != nil {
		t.Fatalf("audit chain: %v", err)
	}

	// Original token still works with matching PEM (mismatch fails before consume).
	resp, err := ec.Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "ok-host",
			Os:       "linux",
		},
		ClientCertPem: connID.CertPEM,
	})
	if err != nil {
		t.Fatalf("matching enroll after mismatch should succeed: %v", err)
	}
	if resp.MachineId == "" {
		t.Fatal("empty machine id")
	}
	t.Logf("matching enroll after mismatch OK: %s", resp.MachineId)
}

type realVault struct {
	m *vault.Manager
}

func (r *realVault) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	v, err := r.m.Create(ctx, repoID, password)
	if err != nil {
		return nil, "", err
	}
	return v.HashingKey(ctx)
}
