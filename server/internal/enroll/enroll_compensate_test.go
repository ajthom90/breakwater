package enroll_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/enroll"
	"github.com/ajthom90/breakwater/server/internal/keystore"
	"github.com/ajthom90/breakwater/server/internal/mtls"
)

// failingVault always errors on Create — injects post-consume failure (R2-9).
type failingVault struct{}

func (failingVault) Create(ctx context.Context, repoID, password string) ([]byte, string, error) {
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

	svc := &enroll.Service{
		DB:       db,
		Keystore: ks,
		Vaults:   failingVault{},
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
	if !errors.Is(err, err) { // just ensure we got an error
		t.Fatal(err)
	}
	t.Logf("enroll failed as expected: %v", err)

	// Token must be reusable after compensation.
	// Keystore row for the attempted machine should be gone — we don't know machineID
	// easily, but re-enroll with a working vault proves the token was released.
	okVault := &stubVault{key: []byte("0123456789abcdef0123456789abcdef"), algo: "BLAKE2B-256-128"}
	svc.Vaults = okVault
	// Also need a fresh keystore Create not to conflict — DeleteRepo should have run.
	// Use same token.
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
