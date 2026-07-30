package keystore_test

import (
	"context"
	"errors"
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

	// Before SetHashingKey, GetHashingKey must return ErrHashingKeyNotSet (R2-6).
	_, _, err = ks.GetHashingKey(ctx, "repo1")
	if !errors.Is(err, keystore.ErrHashingKeyNotSet) {
		t.Fatalf("expected ErrHashingKeyNotSet, got %v", err)
	}

	// Hashing key is vault-sourced; store and retrieve with algorithm (R2-5).
	hk := []byte("this-is-a-fake-hmac-secret-32b!!")
	algo := "BLAKE2B-256-128"
	if err := ks.SetHashingKey(ctx, "repo1", hk, algo); err != nil {
		t.Fatal(err)
	}
	gotHK, gotAlgo, err := ks.GetHashingKey(ctx, "repo1")
	if err != nil || string(gotHK) != string(hk) || gotAlgo != algo {
		t.Fatalf("hashing key: %v %q algo=%q", err, gotHK, gotAlgo)
	}
}

func TestGetHashingKey_EmptyIsNotSet(t *testing.T) {
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
	if _, err := ks.CreateRepoPassword(ctx, "empty-hk"); err != nil {
		t.Fatal(err)
	}
	_, _, err = ks.GetHashingKey(ctx, "empty-hk")
	if !errors.Is(err, keystore.ErrHashingKeyNotSet) {
		t.Fatalf("want ErrHashingKeyNotSet, got %v", err)
	}
}
