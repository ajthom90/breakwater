package keystore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/ajthom90/breakwater/server/internal/keystore"
)

func TestCreatePasswordAndSetHashingKey(t *testing.T) {
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
	pw, err := ks.CreateRepoPassword(ctx, "repo1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pw) == 0 {
		t.Fatal("empty password")
	}
	got, err := ks.GetRepoPassword(ctx, "repo1")
	if err != nil || got != pw {
		t.Fatalf("password: %v %s", err, got)
	}

	// Hashing key is vault-sourced; store and retrieve.
	hk := []byte("this-is-a-fake-hmac-secret-32b!!")
	if err := ks.SetHashingKey(ctx, "repo1", hk); err != nil {
		t.Fatal(err)
	}
	gotHK, err := ks.GetHashingKey(ctx, "repo1")
	if err != nil || string(gotHK) != string(hk) {
		t.Fatalf("hashing key: %v %q", err, gotHK)
	}
}
