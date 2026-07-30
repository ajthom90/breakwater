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
)

// noopVault satisfies enroll.VaultCreator without touching disk repos.
type noopVault struct{}

func (noopVault) Create(context.Context, string, string) error { return nil }

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

	enrollSvc := &enroll.Service{
		DB:            db,
		Keystore:      ks,
		Vaults:        noopVault{},
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
	_ = rawTok

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
		ClientCertPEM: agentID.CertPEM,
	})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if resp.MachineID == "" || len(resp.HashingKey) != 32 {
		t.Fatalf("bad enroll response: %+v", resp)
	}
	if resp.ServerCertFingerprint != serverFP {
		t.Fatalf("server fp mismatch")
	}
	t.Logf("enrolled machine_id=%s", resp.MachineID)

	// Machine appears in catalog.
	m, err := db.MachineByID(ctx, resp.MachineID)
	if err != nil || m == nil {
		t.Fatalf("machine missing from catalog: %v", err)
	}
	if m.Hostname != "fake-linux-01" {
		t.Fatalf("hostname: %s", m.Hostname)
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
	_, err = grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(badPinTLS)))
	if err != nil {
		// NewClient is lazy; force a call.
	}
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

	// Token reuse fails.
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

type echoImpl struct{}

func (echoImpl) Echo(ctx context.Context, req *agentgw.EchoRequest) (*agentgw.EchoResponse, error) {
	pi, _ := agentgw.PeerFromContext(ctx)
	return &agentgw.EchoResponse{Message: req.Message, MachineID: pi.MachineID}, nil
}
