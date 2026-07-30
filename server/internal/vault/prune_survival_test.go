package vault_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// pruneForTest runs prune with zero min-age so young test data is reclaimable.
// Production Prune(ctx) keeps the 24h default (R2-2).
func pruneForTest(ctx context.Context, v vault.Vault) error {
	return v.Prune(ctx, vault.WithMinContentAge(0))
}

// wrapFilePayloadRoot stores a one-entry TreeObject whose sole file entry points
// at payloadOID and returns the tree root object ID (R3-1: file snapshots must
// have TreeObject roots, not flat raw-byte roots).
func wrapFilePayloadRoot(t *testing.T, ctx context.Context, v vault.Vault, name string, payloadOID vault.ObjectID, size int64) vault.ObjectID {
	t.Helper()
	tree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: name, Type: format.EntryFile, Size: size, ObjectID: string(payloadOID)},
		},
	}
	return writeJSONObject(t, ctx, v, tree)
}

// diskBytes walks the repo directory and sums regular file sizes (R2-13).
func diskBytes(t *testing.T, repoDir string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(repoDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoDir, err)
	}
	return total
}

func readObject(t *testing.T, ctx context.Context, v vault.Vault, oid vault.ObjectID) []byte {
	t.Helper()
	r, err := v.OpenObject(ctx, oid)
	if err != nil {
		t.Fatalf("OpenObject %s: %v", oid, err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read %s: %v", oid, err)
	}
	return b
}

func writeJSONObject(t *testing.T, ctx context.Context, v vault.Vault, val any) vault.ObjectID {
	t.Helper()
	raw, err := json.Marshal(val)
	if err != nil {
		t.Fatal(err)
	}
	oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("WriteObject json: %v", err)
	}
	return oid
}

// TestPruneSurvivesIndirectTreeReferences is R2-15(a): a live file snapshot whose
// root TreeObject references separate file objects and a child tree. After prune,
// every indirectly-referenced object must still open and checksum-verify.
//
// Against 755f417 (flat-only mark) this MUST FAIL — only the root JSON blob is
// marked live, so file/child contents are DeleteContent'd.
func TestPruneSurvivesIndirectTreeReferences(t *testing.T) {
	ctx := context.Background()
	reposDir := t.TempDir()
	mgr := vault.NewManager(reposDir)
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "tree-live", "pw-tree")
	if err != nil {
		t.Fatal(err)
	}

	file1Payload := bytes.Repeat([]byte("FILE1-LIVE-PAYLOAD-"), 64)
	file2Payload := bytes.Repeat([]byte("FILE2-LIVE-PAYLOAD-"), 64)
	file1OID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(file1Payload))
	if err != nil {
		t.Fatal(err)
	}
	file2OID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(file2Payload))
	if err != nil {
		t.Fatal(err)
	}
	file1Sum := sha256.Sum256(file1Payload)
	file2Sum := sha256.Sum256(file2Payload)

	// Child directory tree referencing file2.
	childTree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{
				Name:     "nested.txt",
				Type:     format.EntryFile,
				Size:     int64(len(file2Payload)),
				ObjectID: string(file2OID),
			},
		},
	}
	childOID := writeJSONObject(t, ctx, v, childTree)

	// Root tree referencing file1 + child dir.
	rootTree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{
				Name:     "root.txt",
				Type:     format.EntryFile,
				Size:     int64(len(file1Payload)),
				ObjectID: string(file1OID),
			},
			{
				Name:     "subdir",
				Type:     format.EntryDir,
				ObjectID: string(childOID),
			},
		},
	}
	rootOID := writeJSONObject(t, ctx, v, rootTree)

	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "tree-live",
		Timestamp:    time.Now().UTC(),
		RootObjectID: rootOID,
		Source:       "/data",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Forgotten snapshot with its own indirect tree (must be reclaimed — R2-15(c)).
	deadFile := bytes.Repeat([]byte("DEAD-INDIRECT-FILE-"), 32)
	deadFileOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(deadFile))
	if err != nil {
		t.Fatal(err)
	}
	deadTree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: "gone.txt", Type: format.EntryFile, Size: int64(len(deadFile)), ObjectID: string(deadFileOID)},
		},
	}
	deadRoot := writeJSONObject(t, ctx, v, deadTree)
	deadSnap, err := v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "tree-live",
		Timestamp:    time.Now().UTC().Add(-time.Hour),
		RootObjectID: deadRoot,
		Source:       "/dead",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.DeleteSnapshotRecord(ctx, deadSnap); err != nil {
		t.Fatal(err)
	}

	// Reclamation of young forgotten data: tests pass WithMinContentAge(0) once
	// the prune API supports it (R2-2). Until then, default Prune(ctx).
	if err := pruneForTest(ctx, v); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// Live indirect objects must survive.
	got1 := readObject(t, ctx, v, file1OID)
	if !bytes.Equal(got1, file1Payload) {
		t.Fatalf("file1 payload mismatch after prune")
	}
	if sum := sha256.Sum256(got1); sum != file1Sum {
		t.Fatalf("file1 checksum mismatch")
	}
	got2 := readObject(t, ctx, v, file2OID)
	if !bytes.Equal(got2, file2Payload) {
		t.Fatalf("file2 payload mismatch after prune")
	}
	if sum := sha256.Sum256(got2); sum != file2Sum {
		t.Fatalf("file2 checksum mismatch")
	}
	gotChild := readObject(t, ctx, v, childOID)
	var gotChildTree format.TreeObject
	if err := json.Unmarshal(gotChild, &gotChildTree); err != nil {
		t.Fatalf("child tree decode: %v", err)
	}
	if len(gotChildTree.Entries) != 1 || gotChildTree.Entries[0].ObjectID != string(file2OID) {
		t.Fatalf("child tree corrupted: %+v", gotChildTree)
	}
	gotRoot := readObject(t, ctx, v, rootOID)
	var gotRootTree format.TreeObject
	if err := json.Unmarshal(gotRoot, &gotRootTree); err != nil {
		t.Fatalf("root tree decode: %v", err)
	}
	if len(gotRootTree.Entries) != 2 {
		t.Fatalf("root tree entries: %d", len(gotRootTree.Entries))
	}

	// Forgotten indirect snapshot contents must be gone.
	if r, err := v.OpenObject(ctx, deadFileOID); err == nil {
		_, _ = io.Copy(io.Discard, r)
		_ = r.Close()
		t.Fatalf("forgotten indirect file object still readable after prune")
	}
	if r, err := v.OpenObject(ctx, deadRoot); err == nil {
		_, _ = io.Copy(io.Discard, r)
		_ = r.Close()
		t.Fatalf("forgotten indirect root still readable after prune")
	}
}

// TestPruneSurvivesImageManifestBlocks is R2-15(b): live image snapshot with
// ImageManifest referencing block content IDs. After prune all blocks must remain readable.
// Against 755f417 this MUST FAIL — only the manifest JSON blob is marked.
func TestPruneSurvivesImageManifestBlocks(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "img-live", "pw-img")
	if err != nil {
		t.Fatal(err)
	}

	block1 := bytes.Repeat([]byte("IMG-BLOCK-1-"), 100)
	block2 := bytes.Repeat([]byte("IMG-BLOCK-2-"), 100)
	cid1, err := v.PutContent(ctx, block1)
	if err != nil {
		t.Fatal(err)
	}
	cid2, err := v.PutContent(ctx, block2)
	if err != nil {
		t.Fatal(err)
	}

	manifest := format.ImageManifest{
		Version:   format.FormatVersion,
		BlockSize: 4 << 20,
		Size:      int64(len(block1) + len(block2)),
		Blocks: []format.ImageBlockRef{
			{ContentID: string(cid1), XXH64: 1},
			{ContentID: string(cid2), XXH64: 2},
		},
	}
	manOID := writeJSONObject(t, ctx, v, manifest)

	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindImageSnapshot,
		MachineID:    "img-live",
		Timestamp:    time.Now().UTC(),
		RootObjectID: manOID,
		Source:       "\\\\.\\PhysicalDrive0",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Orphan content not referenced by any live manifest — should be reclaimed.
	orphan, err := v.PutContent(ctx, []byte("orphan-image-block-unique"))
	if err != nil {
		t.Fatal(err)
	}

	if err := pruneForTest(ctx, v); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	got1, err := v.GetContent(ctx, cid1)
	if err != nil {
		t.Fatalf("block1 missing after prune: %v", err)
	}
	if !bytes.Equal(got1, block1) {
		t.Fatal("block1 payload mismatch")
	}
	got2, err := v.GetContent(ctx, cid2)
	if err != nil {
		t.Fatalf("block2 missing after prune: %v", err)
	}
	if !bytes.Equal(got2, block2) {
		t.Fatal("block2 payload mismatch")
	}
	// Manifest object itself still readable.
	_ = readObject(t, ctx, v, manOID)

	has, err := v.HasContents(ctx, []vault.ContentID{orphan})
	if err != nil {
		t.Fatal(err)
	}
	if has[0] {
		t.Fatalf("orphan content %s still present after prune", orphan)
	}
}

// TestPruneMinAgeProtectsInFlightBackup is R2-2: contents uploaded before the
// snapshot record is committed must survive a concurrent prune under the default
// min-age guard (≥24h).
func TestPruneMinAgeProtectsInFlightBackup(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "inflight", "pw-inflight")
	if err != nil {
		t.Fatal(err)
	}

	payload := bytes.Repeat([]byte("IN-FLIGHT-BACKUP-CHUNK-"), 200)
	oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}

	// Prune BEFORE PutSnapshotRecord — simulates race with multi-RPC backup.
	// Default min-age (24h) must protect these young contents.
	if err := v.Prune(ctx); err != nil {
		t.Fatalf("Prune during in-flight backup: %v", err)
	}

	// Now commit the snapshot as the agent would (TreeObject root, R3-1).
	rootOID := wrapFilePayloadRoot(t, ctx, v, "chunk.bin", oid, int64(len(payload)))
	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "inflight",
		Timestamp:    time.Now().UTC(),
		RootObjectID: rootOID,
		Source:       "/inflight",
	})
	if err != nil {
		t.Fatal(err)
	}

	got := readObject(t, ctx, v, oid)
	if !bytes.Equal(got, payload) {
		t.Fatal("in-flight backup contents destroyed by prune (min-age guard failed)")
	}
}
