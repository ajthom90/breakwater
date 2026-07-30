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

// failingVault always errors on Create — injects post-consume failure (R2-9/R3-7).
type failingVault struct {
	repoID string
}

func (f *failingVault) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	f.repoID = repoID
	return nil, "", fmt.Errorf("injected vault create failure")
}

func TestEnroll_CompensateOnVaultFailure(t *testing.T) {
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

	rawTok, secret, err := enroll.Mint("127.0.0.1:9443", serverFP)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertEnrollToken(ctx, "tok-comp", secret, "admin", time.Now().UTC().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	agentID, err := mtls.GenerateAgentIdentity("agent", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	failing := &failingVault{}
	svc := &enroll.Service{
		DB:       db,
		Keystore: ks,
		Vaults:   failing,
		ServerFP: serverFP,
	}

	_, err = svc.Enroll(ctx, enroll.EnrollRequest{
		Token:            rawTok,
		Hostname:         "h",
		OS:               "linux",
		ConnectionCertFP: agentID.Fingerprint(),
		ClientCertPEM:    agentID.CertPEM,
	})
	if err == nil {
		t.Fatal("expected enroll to fail")
	}
	t.Logf("enroll failed as expected: %v", err)

	// R3-7: keystore row for the failed attempt must be gone.
	if failing.repoID == "" {
		t.Fatal("vault Create never ran")
	}
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM keystore WHERE repo_id = ?`, failing.repoID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("keystore row still present for failed enroll repo_id=%s", failing.repoID)
	}

	// Token must be reusable after compensation.
	okVault := &stubVault{key: []byte("0123456789abcdef0123456789abcdef"), algo: "BLAKE2B-256-128"}
	svc.Vaults = okVault
	resp, err := svc.Enroll(ctx, enroll.EnrollRequest{
		Token:            rawTok,
		Hostname:         "h2",
		OS:               "linux",
		ConnectionCertFP: agentID.Fingerprint(),
		ClientCertPEM:    agentID.CertPEM,
	})
	if err != nil {
		t.Fatalf("token should be reusable after compensate: %v", err)
	}
	if resp.MachineID == "" || len(resp.HashingKey) == 0 {
		t.Fatalf("bad success response: %+v", resp)
	}
	t.Logf("retry after compensate OK: machine=%s", resp.MachineID)
}

type stubVault struct {
	key  []byte
	algo string
}

func (s *stubVault) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
	return s.key, s.algo, nil
}
