package keystore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/keystore"
)

func TestCreateAndGetSecrets(t *testing.T) {
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
	pw, hk, err := ks.CreateRepoSecrets(ctx, "repo1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) == 0 || len(hk) != 32 {
		t.Fatalf("pw/hk bad")
	}
	got, err := ks.GetRepoPassword(ctx, "repo1")
	if err != nil || got != pw {
		t.Fatalf("password: %v %s", err, got)
	}
	gotHK, err := ks.GetHashingKey(ctx, "repo1")
	if err != nil || string(gotHK) != string(hk) {
		t.Fatalf("hashing key")
	}
}
