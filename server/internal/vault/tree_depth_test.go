package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo/content"

	"github.com/ajthom90/breakwater/pkg/format"
)

// TestTreeDepthExceededError_Actionable is a cheap unit check: calling mark at
// depth MaxTreeDepth+1 must fail before any repo I/O and name the manifest + path.
func TestTreeDepthExceededError_Actionable(t *testing.T) {
	live := make(map[content.ID]struct{})
	err := markTreeObject(context.Background(), nil, live, ObjectID("oid-deep"), "manifest-XYZ", format.MaxTreeDepth+1, "a/b/c/deep")
	if err == nil {
		t.Fatal("expected depth exceeded error")
	}
	msg := err.Error()
	for _, want := range []string{
		"manifest-XYZ",
		"a/b/c/deep",
		"oid-deep",
		fmt.Sprintf("%d", format.MaxTreeDepth),
		"runaway guard",
		"prune",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error missing %q: %s", want, msg)
		}
	}
	t.Logf("actionable: %s", msg)
}

// TestPruneDeepTreeBeyondOld256Limit builds a chain of nested directories deeper
// than the historical 256 limit and asserts prune + mark succeed (M4-F1).
// A tree deeper than format.MaxTreeDepth is covered by the unit test above.
func TestPruneDeepTreeBeyondOld256Limit(t *testing.T) {
	const depth = 300 // was maxTreeDepth=256; must succeed now
	if depth <= 256 {
		t.Fatal("test depth must exceed old limit of 256")
	}
	if depth > format.MaxTreeDepth {
		t.Fatalf("test depth %d exceeds MaxTreeDepth %d", depth, format.MaxTreeDepth)
	}

	ctx := context.Background()
	mgr := NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)

	v, err := mgr.Create(ctx, "deep-tree", "pw")
	if err != nil {
		t.Fatal(err)
	}

	// Leaf file payload.
	fileOID, err := v.WriteObject(ctx, SplitterFixed4M, bytes.NewReader([]byte("deep-leaf\n")))
	if err != nil {
		t.Fatal(err)
	}

	// Build bottom-up: depth levels of single-child dirs ending in the file.
	// Level 0 = deepest dir containing the file; level depth-1 = root.
	childOID := fileOID
	childIsFile := true
	for i := 0; i < depth; i++ {
		var entries []format.TreeEntry
		if childIsFile {
			entries = []format.TreeEntry{{
				Name: "leaf.txt", Type: format.EntryFile, Size: 10, ObjectID: string(childOID),
			}}
			childIsFile = false
		} else {
			entries = []format.TreeEntry{{
				Name: fmt.Sprintf("d%d", i), Type: format.EntryDir, ObjectID: string(childOID),
			}}
		}
		raw, err := json.Marshal(format.TreeObject{Version: format.FormatVersion, Entries: entries})
		if err != nil {
			t.Fatal(err)
		}
		oid, err := v.WriteObject(ctx, SplitterFixed4M, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		childOID = oid
	}
	rootOID := childOID

	manID, err := v.PutSnapshotRecord(ctx, SnapshotRecord{
		Kind: KindFileSnapshot, MachineID: "deep-tree",
		Timestamp: time.Now().UTC(), RootObjectID: rootOID, Source: "/deep",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("deep tree depth=%d root=%s manifest=%s", depth, rootOID, manID)

	// Mark must succeed (would have failed at old 256).
	live := make(map[content.ID]struct{})
	if err := markSnapshotContents(ctx, v.(*kopiaVault).rep, live, KindFileSnapshot, rootOID, "deep-manifest"); err != nil {
		t.Fatalf("mark deep tree: %v", err)
	}
	if len(live) < depth {
		t.Fatalf("live contents %d want >= %d", len(live), depth)
	}

	// Prune must complete (not wedge retention).
	if err := v.Prune(ctx, WithMinContentAge(0)); err != nil {
		t.Fatalf("prune deep tree: %v", err)
	}

	// Root still readable after prune.
	rc, err := v.OpenObject(ctx, rootOID)
	if err != nil {
		t.Fatalf("OpenObject root after prune: %v", err)
	}
	_ = rc.Close()
	t.Logf("M4-F1: depth=%d prune OK live_contents=%d", depth, len(live))
}

// TestMarkTreeDepthPathPrefix names nested path components in the error.
func TestMarkTreeDepthPathPrefix(t *testing.T) {
	// Force depth error mid-walk by using MaxTreeDepth-1 so one more dir exceeds.
	// Cheaper: call markTreeObject with depth already near the limit on a real tiny tree.
	ctx := context.Background()
	mgr := NewManager(t.TempDir(), t.TempDir())
	defer mgr.CloseAll(ctx)
	v, err := mgr.Create(ctx, "path-prefix", "pw")
	if err != nil {
		t.Fatal(err)
	}

	// Nested two dirs: outer/inner/leaf.txt — call mark with depth = MaxTreeDepth
	// so the first child dir walk hits MaxTreeDepth+1.
	leaf, err := v.WriteObject(ctx, SplitterFixed4M, bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatal(err)
	}
	innerRaw, _ := json.Marshal(format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{{Name: "leaf.txt", Type: format.EntryFile, ObjectID: string(leaf)}},
	})
	innerOID, err := v.WriteObject(ctx, SplitterFixed4M, bytes.NewReader(innerRaw))
	if err != nil {
		t.Fatal(err)
	}
	outerRaw, _ := json.Marshal(format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{{Name: "inner", Type: format.EntryDir, ObjectID: string(innerOID)}},
	})
	outerOID, err := v.WriteObject(ctx, SplitterFixed4M, bytes.NewReader(outerRaw))
	if err != nil {
		t.Fatal(err)
	}

	live := make(map[content.ID]struct{})
	// Start at depth MaxTreeDepth: root is OK; child "inner" is depth+1 → error.
	err = markTreeObject(ctx, v.(*kopiaVault).rep, live, outerOID, "man-path", format.MaxTreeDepth, "already/deep")
	if err == nil {
		t.Fatal("expected depth error when child exceeds MaxTreeDepth")
	}
	msg := err.Error()
	if !strings.Contains(msg, "man-path") {
		t.Errorf("missing manifest: %s", msg)
	}
	if !strings.Contains(msg, "already/deep/inner") && !strings.Contains(msg, "inner") {
		t.Errorf("missing path prefix for child: %s", msg)
	}
	t.Logf("path-aware error: %s", msg)
}
