package vault_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/server/internal/vault"
)

// TestPutSnapshotRecord_RejectsCrossKindRoot is M2 / R3 addendum note 1:
// a TreeObject root labeled bw-image-snapshot (or ImageManifest under
// bw-file-snapshot) must be rejected at the write boundary.
//
// Red-first (loose json.Unmarshal): Put accepted the mislabeled root — see
// PROGRESS.md M2 stage-1 evidence. After DisallowUnknownFields: Put rejects.
func TestPutSnapshotRecord_RejectsCrossKindRoot(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "cross-kind", "pw")
	if err != nil {
		t.Fatal(err)
	}

	// Real TreeObject JSON (valid for file kind).
	tree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: "f", Type: format.EntryFile, ObjectID: "placeholder"},
		},
	}
	treeJSON, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	treeOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(treeJSON))
	if err != nil {
		t.Fatal(err)
	}

	// Store TreeObject under image kind — must fail with strict decode.
	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindImageSnapshot,
		MachineID:    "cross-kind",
		Timestamp:    time.Now().UTC(),
		RootObjectID: treeOID,
		Source:       "/cross",
	})
	if err == nil {
		t.Fatal("PutSnapshotRecord must reject TreeObject root under bw-image-snapshot (cross-kind)")
	}
	if !strings.Contains(err.Error(), "ImageManifest") && !strings.Contains(err.Error(), "unknown field") {
		t.Logf("reject error (acceptable): %v", err)
	}
	t.Logf("cross-kind TreeObject-as-image rejected: %v", err)

	// Real ImageManifest under file kind — must also fail.
	img := format.ImageManifest{
		Version:   format.FormatVersion,
		BlockSize: 4 << 20,
		Size:      4 << 20,
		Blocks:    []format.ImageBlockRef{{ContentID: "x", XXH64: 1}},
	}
	imgJSON, err := json.Marshal(img)
	if err != nil {
		t.Fatal(err)
	}
	imgOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(imgJSON))
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "cross-kind",
		Timestamp:    time.Now().UTC(),
		RootObjectID: imgOID,
		Source:       "/cross-file",
	})
	if err == nil {
		t.Fatal("PutSnapshotRecord must reject ImageManifest root under bw-file-snapshot (cross-kind)")
	}
	t.Logf("cross-kind ImageManifest-as-file rejected: %v", err)

	// Control: correct kind still accepted.
	// TreeObject needs a real file object id for mark, but write boundary only
	// checks decode — placeholder oid string is fine if present in JSON.
	// Use a real object as the entry target.
	fileOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader([]byte("file-bytes")))
	if err != nil {
		t.Fatal(err)
	}
	treeOK := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: "f", Type: format.EntryFile, ObjectID: string(fileOID)},
		},
	}
	treeOKJSON, _ := json.Marshal(treeOK)
	treeOKOID, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(treeOKJSON))
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "cross-kind",
		Timestamp:    time.Now().UTC(),
		RootObjectID: treeOKOID,
		Source:       "/ok",
	})
	if err != nil {
		t.Fatalf("correct kind must still be accepted: %v", err)
	}
}

// TestPutSnapshotRecord_RejectsTrailingGarbage is S1-F3: a valid TreeObject JSON
// prefix followed by trailing bytes must be rejected at the write boundary.
//
// Red-first on bc65f8a: strictJSONDecode only Decode()s one value and ignores
// trailing garbage → Put accepts. After EOF check: Put rejects.
func TestPutSnapshotRecord_RejectsTrailingGarbage(t *testing.T) {
	ctx := context.Background()
	mgr := vault.NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "trailing", "pw")
	if err != nil {
		t.Fatal(err)
	}

	// Valid TreeObject JSON + trailing garbage (json.Unmarshal would reject;
	// Decoder.Decode alone accepts).
	raw := append([]byte(`{"v":1,"entries":[]}`), []byte("TRAILING-GARBAGE-BYTES")...)
	oid, err := v.WriteObject(ctx, vault.SplitterFixed4M, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	_, err = v.PutSnapshotRecord(ctx, vault.SnapshotRecord{
		Kind:         vault.KindFileSnapshot,
		MachineID:    "trailing",
		Timestamp:    time.Now().UTC(),
		RootObjectID: oid,
		Source:       "/trailing",
	})
	if err == nil {
		t.Fatal("PutSnapshotRecord must reject TreeObject with trailing garbage after JSON value (S1-F3)")
	}
	t.Logf("trailing garbage rejected: %v", err)
}
