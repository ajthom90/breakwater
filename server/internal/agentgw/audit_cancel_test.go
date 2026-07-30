package agentgw_test

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"github.com/ajthom90/breakwater/server/internal/agentgw"
	"github.com/ajthom90/breakwater/server/internal/audit"
	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
)

// cancelThenFailVault cancels the request context then fails Create (S1-F1 / R3-3).
type cancelThenFailVault struct {
	cancel context.CancelFunc
	repoID string
}

func (v *cancelThenFailVault) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	v.repoID = repoID
	if v.cancel != nil {
		v.cancel()
	}
	if ctx.Err() == nil {
		<-ctx.Done()
	}
	return nil, "", fmt.Errorf("injected vault failure after ctx cancel")
}

// TestEnroll_AuditDespiteCanceledContext is S1-F1: when Vaults.Create cancels
// the request context and then errors, the rejected machine.enroll audit row
// must still land and the chain must verify.
//
// Red-first on bc65f8a: auditEnroll uses the dead request ctx → Append no-ops
// → zero machine.enroll rows.
func TestEnroll_AuditDespiteCanceledContext(t *testing.T) {
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
	auditor := audit.NewWriter(db)

	// Client cancel is invoked from Vaults.Create so the RPC (and server
	// request) context is done by the time auditEnroll runs.
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	failing := &cancelThenFailVault{cancel: clientCancel}

	svc := &enroll.Service{
		DB: db, Keystore: ks, Vaults: failing, ServerFP: serverFP,
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
	if err := db.InsertEnrollToken(context.Background(), "tok-audit-cancel", secret, "t", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	agentID, err := mtls.GenerateAgentIdentity("audit-cancel", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(mtls.ClientTLSConfig(agentID, serverFP))))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	_, err = breakwaterv1.NewEnrollmentServiceClient(conn).Enroll(clientCtx, &breakwaterv1.EnrollRequest{
		Token: rawTok,
		AgentInfo: &breakwaterv1.AgentInfo{
			Hostname: "h-audit-cancel",
			Os:       "linux",
		},
		ClientCertPem: agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expected enroll to fail")
	}
	t.Logf("enroll failed as expected: %v", err)

	// Client cancel returns before the server finishes Enroll/auditEnroll —
	// poll briefly for the audit row (WithoutCancel lets the append complete).
	rows := waitAuditRows(t, auditor, audit.ActionMachineEnroll, 1, 2*time.Second)
	if err := auditor.VerifyChain(context.Background()); err != nil {
		t.Fatalf("audit chain: %v", err)
	}
	t.Logf("machine.enroll audit landed despite canceled ctx: id=%s", rows[0].ID)
}

// waitAuditRows polls until at least n rows for action exist, or fails.
func waitAuditRows(t *testing.T, w *audit.Writer, action string, n int, timeout time.Duration) []audit.Record {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []audit.Record
	for time.Now().Before(deadline) {
		rows, err := w.ListByAction(context.Background(), action)
		if err != nil {
			t.Fatal(err)
		}
		last = rows
		if len(rows) >= n {
			return rows
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected ≥%d audit rows for %s within %s; got %d (S1-F1)", n, action, timeout, len(last))
	return nil
}
