package vault_test

import (
	"context"
	"testing"

	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestManager_ReopenAfterClose is R2-10: closing a vault through the interface
// must not permanently poison the Manager cache.
func TestManager_ReopenAfterClose(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)

	v1, err := mgr.Create(ctx, "reopen-1", "pw")
	if err != nil {
		t.Fatal(err)
	}
	if err := v1.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Open must re-open, not return the closed instance forever.
	v2, err := mgr.Open(ctx, "reopen-1", "pw")
	if err != nil {
		t.Fatalf("Open after Close: %v", err)
	}
	if _, err := v2.Stats(ctx); err != nil {
		t.Fatalf("Stats on reopened vault: %v", err)
	}

	// Manager.Close should also work.
	if err := mgr.Close(ctx, "reopen-1"); err != nil {
		t.Fatal(err)
	}
	v3, err := mgr.Open(ctx, "reopen-1", "pw")
	if err != nil {
		t.Fatalf("Open after Manager.Close: %v", err)
	}
	if _, err := v3.Stats(ctx); err != nil {
		t.Fatalf("Stats after Manager.Close+Open: %v", err)
	}
}
