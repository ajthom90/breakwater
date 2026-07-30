package vault

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/content"
)

// TestMarkTreeObject_UndecodableRootFailsClosed is R3-1 defense-in-depth:
// markTreeObject must always fail on decode error (no leaf heuristic).
// Also plants an unchecked snapshot record so Prune itself fails closed.
func TestMarkTreeObject_UndecodableRootFailsClosed(t *testing.T) {
	ctx := context.Background()
	mgr := NewManager(t.TempDir())
	defer mgr.CloseAll(ctx)

	viface, err := mgr.Create(ctx, "mark-fc", "pw")
	if err != nil {
		t.Fatal(err)
	}
	v := viface.(*kopiaVault)

	flat := []byte("NOT-A-TREE-OBJECT-raw-bytes-payload")
	oid, err := v.WriteObject(ctx, SplitterFixed4M, bytes.NewReader(flat))
	if err != nil {
		t.Fatal(err)
	}

	live := make(map[content.ID]struct{})
	err = markSnapshotContents(ctx, v.rep, live, KindFileSnapshot, oid, "test-manifest")
	if err == nil {
		t.Fatal("markTreeObject must fail closed on undecodable root")
	}
	t.Logf("mark failed closed: %v", err)

	if err := plantFileSnapshotUnchecked(ctx, v, oid); err != nil {
		t.Fatal(err)
	}
	if err := v.Prune(ctx, WithMinContentAge(0)); err == nil {
		t.Fatal("Prune must fail closed when a live file snapshot has undecodable TreeObject root")
	} else {
		t.Logf("Prune failed closed on planted bad root: %v", err)
	}
}

// plantFileSnapshotUnchecked writes a bw-file-snapshot manifest without root
// format validation (bypasses PutSnapshotRecord) for defense-in-depth tests.
func plantFileSnapshotUnchecked(ctx context.Context, v *kopiaVault, root ObjectID) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return err
	}
	rec := SnapshotRecord{
		Kind:         KindFileSnapshot,
		MachineID:    "mark-fc",
		Timestamp:    time.Now().UTC(),
		RootObjectID: root,
		Source:       "/planted-bad",
	}
	labels := map[string]string{
		"type":    string(KindFileSnapshot),
		"machine": rec.MachineID,
		"source":  rec.Source,
	}
	return repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "plant-bad-snap"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			_, err := w.PutManifest(ctx, labels, rec)
			return err
		})
}
