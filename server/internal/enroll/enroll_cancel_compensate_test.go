package enroll_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
)

// cancelThenFailVault cancels the request context then fails Create (R3-3).
// Models: agent RPC deadline expires during slow vault create.
type cancelThenFailVault struct {
	cancel context.CancelFunc
	repoID string
}

func (v *cancelThenFailVault) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	v.repoID = repoID
	if v.cancel != nil {
		v.cancel()
	}
	// Ensure parent ctx is done before we return (compensation defer runs next).
	if ctx.Err() == nil {
		// cancel is async w.r.t. this ctx if it is the same; force wait.
		<-ctx.Done()
	}
	return nil, "", fmt.Errorf("injected vault failure after ctx cancel")
}

// TestEnroll_CompensateDespiteCanceledContext is R3-3: when Vaults.Create
// cancels the request context and then errors, compensation must still release
// the token and delete the keystore row.
//
// Against eea1a46 this FAILS: compensation uses the dead request ctx, so
// ReleaseEnrollToken/DeleteRepo no-op and the token stays burned.
func TestEnroll_CompensateDespiteCanceledContext(t *testing.T) {
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

	rawTok, secret, err := enroll.Mint("127.0.0.1:9443", serverFP)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEnrollToken(context.Background(), "tok-cancel", secret, "admin", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	agentID, err := mtls.GenerateAgentIdentity("agent-cancel", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	failing := &cancelThenFailVault{cancel: cancel}

	svc := &enroll.Service{
		DB:       db,
		Keystore: ks,
		Vaults:   failing,
		ServerFP: serverFP,
	}

	_, err = svc.Enroll(ctx, enroll.EnrollRequest{
		Token:            rawTok,
		Hostname:         "h-cancel",
		OS:               "linux",
		ConnectionCertFP: agentID.Fingerprint(),
		ClientCertPEM:    agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expected enroll to fail")
	}
	t.Logf("enroll failed as expected: %v", err)

	if failing.repoID == "" {
		t.Fatal("vault Create never ran")
	}

	// R3-7: keystore row for the failed attempt must be gone.
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM keystore WHERE repo_id = ?`, failing.repoID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("keystore row for failed enroll still present (repo_id=%s count=%d) — compensation no-op on canceled ctx", failing.repoID, n)
	}

	// Token must be reusable with a live context.
	okVault := &stubVault{key: []byte("0123456789abcdef0123456789abcdef"), algo: "BLAKE2B-256-128"}
	svc.Vaults = okVault
	resp, err := svc.Enroll(context.Background(), enroll.EnrollRequest{
		Token:            rawTok,
		Hostname:         "h-retry",
		OS:               "linux",
		ConnectionCertFP: agentID.Fingerprint(),
		ClientCertPEM:    agentID.CertPEM,
	})
	if err != nil {
		t.Fatalf("token should be reusable after canceled-ctx compensate: %v", err)
	}
	if resp.MachineID == "" {
		t.Fatal("empty machine id on retry")
	}
	t.Logf("retry after canceled-ctx compensate OK: machine=%s", resp.MachineID)
}
