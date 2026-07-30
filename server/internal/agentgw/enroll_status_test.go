package agentgw_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
)

// failingVaultCreator injects an internal vault failure (R3-8).
type failingVaultCreator struct{}

func (failingVaultCreator) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	return nil, "", fmt.Errorf("create vault: repo.Initialize: /repos/%s disk full", repoID)
}

func TestEnroll_gRPCStatusCodes(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	db, err := catalog.Open(filepath.Join(tmp, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ks, err := keystore.OpenOrCreate(db, filepath.Join(tmp, "master.key"))
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := mtls.GenerateServerIdentity("bw", []string{"127.0.0.1"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverFP := serverID.Fingerprint()

	svc := &enroll.Service{
		DB:       db,
		Keystore: ks,
		Vaults:   failingVaultCreator{},
		ServerFP: serverFP,
		Log:      slog.Default(),
	}
	gw := agentgw.New(serverID, svc, slog.Default())
	addr, err := gw.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer gw.GracefulStop()

	agentID, err := mtls.GenerateAgentIdentity("status-agent", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(agentID, serverFP))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ec := breakwaterv1.NewEnrollmentServiceClient(conn)

	// Bad token → InvalidArgument (not Internal, no path leak).
	_, err = ec.Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: "BW1:x:y:not-a-real-token",
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "h",
			Os:       "linux",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expected bad token error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	if st.Code() != codes.InvalidArgument && st.Code() != codes.PermissionDenied {
		t.Fatalf("bad token code=%v want InvalidArgument/PermissionDenied msg=%q", st.Code(), st.Message())
	}
	if containsAny(st.Message(), "/repos/", "disk full", "repo.Initialize") {
		t.Fatalf("client-visible message leaked internal detail: %q", st.Message())
	}
	t.Logf("bad token → %v %q", st.Code(), st.Message())

	// Valid token + internal vault failure → Internal "enrollment failed" only.
	rawTok, secret, err := enroll.Mint(addr, serverFP)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEnrollToken(ctx, "tok-status", secret, "t", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	_, err = ec.Enroll(ctx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "h",
			Os:       "linux",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expected internal enroll failure")
	}
	st, ok = status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Fatalf("internal failure code=%v want Internal msg=%q", st.Code(), st.Message())
	}
	if st.Message() != "enrollment failed" {
		t.Fatalf("client message must be exactly %q, got %q", "enrollment failed", st.Message())
	}
	if containsAny(st.Message(), "/repos/", "disk full", "Initialize") {
		t.Fatalf("leaked internal path: %q", st.Message())
	}
	t.Logf("internal failure → %v %q", st.Code(), st.Message())
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
