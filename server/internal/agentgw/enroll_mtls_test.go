package agentgw_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestM1_EnrollmentAndWrongCertRejection is the M1 demo gate:
// fake Linux client enrolls; wrong-cert client is rejected.
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

	vm := vault.NewManager(filepath.Join(tmp, "repos"))
	defer vm.CloseAll(ctx)

	enrollSvc := &enroll.Service{
		DB:            db,
		Keystore:      ks,
		Vaults:        &realVault{m: vm},
		ServerFP:      serverFP,
		DefaultPolicy: "01DEFAULTPOLICY000000000000",
		Log:           slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}

	gw := agentgw.New(serverID, enrollSvc, enrollSvc.Log)
	gw.TestEcho = echoImpl{}
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

	// --- Fake Linux client: correct enrollment ---
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

	ec := agentgw.NewEnrollmentClient(conn)
	resp, err := ec.Enroll(ctx, &agentgw.EnrollRequest{
		Token:         rawTok,
		Hostname:      "fake-linux-01",
		OS:            "linux",
		OSVersion:     "test",
		AgentVersion:  "0.0.1-dev",
		Arch:          "amd64",
		ClientCertPEM: agentID.CertPEM, // matches connection
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if resp.MachineID == "" || len(resp.HashingKey) == 0 {
		t.Fatalf("bad enroll response: %+v", resp)
	}
	if resp.ServerCertFingerprint != serverFP {
		t.Fatalf("server fp mismatch")
	}
	t.Logf("enrolled machine_id=%s hashingKeyLen=%d", resp.MachineID, len(resp.HashingKey))

	// Machine appears in catalog with connection cert FP.
	m, err := db.MachineByID(ctx, resp.MachineID)
	if err != nil || m == nil {
		t.Fatalf("machine missing from catalog: %v", err)
	}
	if m.CertFP != agentID.Fingerprint() {
		t.Fatalf("catalog cert_fp %s != agent %s", m.CertFP, agentID.Fingerprint())
	}
	if m.Hostname != "fake-linux-01" {
		t.Fatalf("hostname: %s", m.Hostname)
	}

	// Stored hashing key matches vault format key.
	storedHK, err := ks.GetHashingKey(ctx, resp.MachineID)
	if err != nil {
		t.Fatalf("GetHashingKey: %v", err)
	}
	if string(storedHK) != string(resp.HashingKey) {
		t.Fatal("stored hashing key != enroll response")
	}

	// Post-enroll: known cert can call Echo.
	echo := agentgw.NewEchoClient(conn)
	er, err := echo.Echo(ctx, &agentgw.EchoRequest{Message: "ping"})
	if err != nil {
		t.Fatalf("echo with enrolled cert: %v", err)
	}
	if er.MachineID != resp.MachineID {
		t.Fatalf("echo machine id: %s want %s", er.MachineID, resp.MachineID)
	}
	t.Logf("echo ok machine=%s", er.MachineID)

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

	wrongEcho := agentgw.NewEchoClient(wrongConn)
	_, err = wrongEcho.Echo(ctx, &agentgw.EchoRequest{Message: "should-fail"})
	if err == nil {
		t.Fatal("expected wrong-cert client to be rejected")
	}
	t.Logf("wrong-cert rejected as expected: %v", err)

	// Wrong server fingerprint on client side rejected.
	badPinTLS := mtls.ClientTLSConfig(agentID, "0000000000000000000000000000000000000000000000000000000000000000")
	badConn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(badPinTLS)))
	if err != nil {
		t.Fatalf("bad pin dial setup: %v", err)
	}
	defer badConn.Close()
	_, err = agentgw.NewEchoClient(badConn).Echo(ctx, &agentgw.EchoRequest{Message: "x"})
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
	_, err = agentgw.NewEnrollmentClient(plain).Enroll(ctx, &agentgw.EnrollRequest{Token: "x"})
	if err == nil {
		t.Fatal("expected plaintext to fail")
	}
	t.Logf("plaintext rejected as expected: %v", err)

	// Token reuse fails (same connection cert already enrolled).
	_, err = ec.Enroll(ctx, &agentgw.EnrollRequest{
		Token:         rawTok,
		Hostname:      "reuse",
		OS:            "linux",
		ClientCertPEM: agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expected token reuse to fail")
	}
	t.Logf("token reuse rejected: %v", err)

	t.Log("M1 DEMO PASSED: fake client enrolls; wrong-cert client rejected")
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
	vm := vault.NewManager(filepath.Join(tmp, "repos"))
	defer vm.CloseAll(ctx)

	svc := &enroll.Service{
		DB: db, Keystore: ks, Vaults: &realVault{m: vm}, ServerFP: serverFP,
		Log: slog.Default(),
	}
	gw := agentgw.New(serverID, svc, slog.Default())
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

	_, err = agentgw.NewEnrollmentClient(conn).Enroll(ctx, &agentgw.EnrollRequest{
		Token:         rawTok,
		Hostname:      "mismatch-host",
		OS:            "linux",
		ClientCertPEM: bodyID.CertPEM, // deliberately different from TLS peer
	})
	if err == nil {
		t.Fatal("expected body≠connection cert rejection")
	}
	t.Logf("body-cert mismatch rejected: %v", err)

	// Token must still be usable with matching cert (not burned on mismatch...
	// Current order: mismatch check is before token consume — good).
	// If token was burned, this would fail for wrong reason.
	_, secret2, err := enroll.Mint(addr, serverFP)
	if err != nil {
		t.Fatal(err)
	}
	// Re-parse: if first token still valid, use it; else use fresh.
	// Mismatch fails before ConsumeEnrollToken, so original token still good.
	rawTok2, secret2b, err := enroll.Mint(addr, serverFP)
	_ = secret2
	_ = secret2b
	_ = rawTok2

	// Original token still works with matching PEM.
	resp, err := agentgw.NewEnrollmentClient(conn).Enroll(ctx, &agentgw.EnrollRequest{
		Token:         rawTok,
		Hostname:      "ok-host",
		OS:            "linux",
		ClientCertPEM: connID.CertPEM,
	})
	if err != nil {
		t.Fatalf("matching enroll after mismatch should succeed: %v", err)
	}
	if resp.MachineID == "" {
		t.Fatal("empty machine id")
	}
	t.Logf("matching enroll after mismatch OK: %s", resp.MachineID)
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

type echoImpl struct{}

func (echoImpl) Echo(ctx context.Context, req *agentgw.EchoRequest) (*agentgw.EchoResponse, error) {
	pi, _ := agentgw.PeerFromContext(ctx)
	return &agentgw.EchoResponse{Message: req.Message, MachineID: pi.MachineID}, nil
}
