package vault_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestConfigCacheUnderDataDir asserts M4 layout: config and cache live under
// dataDir, not inside the repository blob path.
func TestConfigCacheUnderDataDir(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	reposDir := filepath.Join(tmp, "repos")
	dataDir := filepath.Join(tmp, "data")

	mgr := vault.NewManager(reposDir, dataDir)
	defer mgr.CloseAll(ctx)

	repoID := "cfg-layout-01"
	v, err := mgr.Create(ctx, repoID, "pw-layout")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(ctx); err != nil {
		t.Fatal(err)
	}
	_ = mgr.Close(ctx, repoID)

	cfg := filepath.Join(dataDir, "kopia-config", repoID+".config")
	cache := filepath.Join(dataDir, "cache", repoID)
	legacyCfg := filepath.Join(reposDir, repoID, "breakwater.config")
	legacyCache := filepath.Join(reposDir, repoID, ".cache")

	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("expected config at %s: %v", cfg, err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("expected cache dir at %s: %v", cache, err)
	}
	if _, err := os.Stat(legacyCfg); err == nil {
		t.Fatalf("config must not live under repo path: %s", legacyCfg)
	}
	if _, err := os.Stat(legacyCache); err == nil {
		t.Fatalf("cache must not live under repo path: %s", legacyCache)
	}
	t.Logf("layout OK: cfg=%s cache=%s", cfg, cache)
}

// TestMigrateLegacyConfigCache opens an M1-era repo (config/cache inside the
// repo path) with the M4 manager and verifies transparent migration.
func TestMigrateLegacyConfigCache(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	reposDir := filepath.Join(tmp, "repos")
	// Create with dataDir == reposDir so config lands under reposDir/kopia-config,
	// then relocate into the M1 in-repo layout by hand.
	createMgr := vault.NewManager(reposDir, reposDir)
	repoID := "legacy-mig-01"
	password := "pw-legacy-migrate"

	v, err := createMgr.Create(ctx, repoID, password)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("legacy-migrate-payload")
	oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := createMgr.CloseAll(ctx); err != nil {
		t.Fatal(err)
	}

	newCfg := filepath.Join(reposDir, "kopia-config", repoID+".config")
	newCache := filepath.Join(reposDir, "cache", repoID)
	repoPath := filepath.Join(reposDir, repoID)
	legacyCfg := filepath.Join(repoPath, "breakwater.config")
	legacyCache := filepath.Join(repoPath, ".cache")

	if err := os.Rename(newCfg, legacyCfg); err != nil {
		t.Fatalf("plant legacy config: %v", err)
	}
	if err := os.Rename(newCache, legacyCache); err != nil {
		t.Fatalf("plant legacy cache: %v", err)
	}
	_ = os.Remove(filepath.Join(reposDir, "kopia-config"))

	dataDir := filepath.Join(tmp, "data-migrated")
	openMgr := vault.NewManager(reposDir, dataDir)
	defer openMgr.CloseAll(ctx)

	v2, err := openMgr.Open(ctx, repoID, password)
	if err != nil {
		t.Fatalf("open after migrate: %v", err)
	}
	r, err := v2.OpenObject(ctx, oid)
	if err != nil {
		t.Fatalf("read object after migrate: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch after migrate: got %q", got)
	}

	migratedCfg := filepath.Join(dataDir, "kopia-config", repoID+".config")
	if _, err := os.Stat(migratedCfg); err != nil {
		t.Fatalf("expected migrated config at %s: %v", migratedCfg, err)
	}
	if _, err := os.Stat(legacyCfg); err == nil {
		t.Fatalf("legacy config should be removed after migrate: %s", legacyCfg)
	}
	t.Logf("migrated OK: cfg=%s payload intact", migratedCfg)
}
