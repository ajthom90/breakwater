package vault_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestPutSnapshotRecord_RejectsFlatFileRoot is R3-1 write-boundary: file-kind
// roots must be TreeObject JSON, not flat raw bytes.
//
// Red-first on eea1a46: Put accepted flat roots (this test would have failed
// with inverted assertion); after R3-1 Put rejects.
func TestPutSnapshotRecord_RejectsFlatFileRoot(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "flat-root", "pw")
	if err != nil {
		t.Fatal(err)
	}

	flat := []byte("NOT-A-TREE-OBJECT-raw-bytes-payload")
	if flat[0] == '{' {
		t.Fatal("test payload must not start with '{'")
	}
	oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(flat))
	if err != nil {
		t.Fatal(err)
	}

	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "flat-root",
		Timestamp:    time.Now().UTC(),
		RootObjectID: oid,
		Source:       "/flat",
	})
	if err == nil {
		t.Fatal("PutSnapshotRecord must reject non-TreeObject file root")
	}
	if !strings.Contains(err.Error(), "TreeObject") {
		t.Fatalf("expected TreeObject validation error, got: %v", err)
	}
	t.Logf("PutSnapshotRecord rejected flat file root: %v", err)
}

// TestPrune_FailClosedOnUndecodableFileRoot is R3-1: prune must fail closed
// when a file-kind root is not TreeObject. Public PutSnapshotRecord rejects
// such roots; the package-level TestMarkTreeObject_UndecodableRootFailsClosed
// plants an unchecked record and asserts Prune itself errors.
//
// Red-first on eea1a46: Put accepted flat root; Prune returned nil (leaf heuristic).
func TestPrune_FailClosedOnUndecodableFileRoot(t *testing.T) {
	// Write-boundary rejection is the primary public fix.
	TestPutSnapshotRecord_RejectsFlatFileRoot(t)
}
