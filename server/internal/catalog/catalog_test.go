package catalog_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
)

func TestMigrateAndMachineRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	m := catalog.Machine{
		ID:           "01TESTMACHINE00000000000001",
		CertFP:       "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899",
		Hostname:     "win-dc01",
		OSInfo:       "windows/amd64",
		AgentVersion: "0.0.1-dev",
		Status:       "enrolled",
		RepoID:       "01TESTMACHINE00000000000001",
	}
	if err := db.InsertMachine(ctx, m); err != nil {
		t.Fatalf("InsertMachine: %v", err)
	}

	got, err := db.MachineByCertFP(ctx, m.CertFP)
	if err != nil || got == nil {
		t.Fatalf("MachineByCertFP: %v %#v", err, got)
	}
	if got.Hostname != "win-dc01" {
		t.Fatalf("hostname: %s", got.Hostname)
	}

	// Default policy seeded
	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM policies WHERE is_default = 1`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("default policy: n=%d err=%v", n, err)
	}
}

func TestEnrollTokenSingleUse(t *testing.T) {
	ctx := context.Background()
	db, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	secret := "super-secret-token-value"
	exp := time.Now().UTC().Add(24 * time.Hour)
	if err := db.InsertEnrollToken(ctx, "tok1", secret, "admin", exp); err != nil {
		t.Fatalf("InsertEnrollToken: %v", err)
	}

	id, err := db.ConsumeEnrollToken(ctx, secret, "machine-1")
	if err != nil || id != "tok1" {
		t.Fatalf("Consume: id=%s err=%v", id, err)
	}
	_, err = db.ConsumeEnrollToken(ctx, secret, "machine-2")
	if err == nil {
		t.Fatal("expected reuse to fail")
	}
}
