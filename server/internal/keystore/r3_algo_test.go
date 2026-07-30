package keystore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/keystore"
)

// TestGetHashingKey_EmptyAlgorithmIsError is R3-4: a real hashing key with an
// empty algorithm column (the 755f417→eea1a46 upgrade population) must return
// a distinct error, not (key, "", nil).
//
// Against eea1a46 this FAILS: only empty key is guarded.
func TestGetHashingKey_EmptyAlgorithmIsError(t *testing.T) {
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

	if _, err := ks.CreateRepoPassword(ctx, "migrated-repo"); err != nil {
		t.Fatal(err)
	}
	hk := []byte("real-hmac-secret-from-755f417!!!!")
	if err := ks.SetHashingKey(ctx, "migrated-repo", hk, "BLAKE2B-256-128"); err != nil {
		t.Fatal(err)
	}
	// Simulate migrateV1ToV2 DEFAULT '' left on upgraded rows.
	if _, err := db.SQL().Exec(`UPDATE keystore SET hashing_algorithm = '' WHERE repo_id = ?`, "migrated-repo"); err != nil {
		t.Fatal(err)
	}

	key, algo, err := ks.GetHashingKey(ctx, "migrated-repo")
	if err == nil {
		t.Fatalf("expected error for empty algorithm, got keyLen=%d algo=%q err=nil (R2-5 resurrected for upgrade population)", len(key), algo)
	}
	if !errors.Is(err, keystore.ErrHashingAlgorithmNotSet) {
		t.Fatalf("want ErrHashingAlgorithmNotSet, got %v", err)
	}
	t.Logf("GetHashingKey correctly rejected empty algorithm: %v", err)
}
