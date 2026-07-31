package restore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ajthom90/breakwater/pkg/format"
	"github.com/ajthom90/breakwater/pkg/restore"
)

// memReader is an in-memory ObjectReader for unit tests.
type memReader map[string][]byte

func (m memReader) OpenObject(_ context.Context, id string) (io.ReadCloser, error) {
	b, ok := m[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func mustTree(t *testing.T, entries ...format.TreeEntry) (oid string, raw []byte) {
	t.Helper()
	tree := format.TreeObject{Version: format.FormatVersion, Entries: entries}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	return "tree-" + entries[0].Name, raw
}

func TestConflictPolicies(t *testing.T) {
	ctx := context.Background()
	fileOID := "file-hello"
	fileData := []byte("restored-content\n")
	rootOID := "root"
	tree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: "hello.txt", Type: format.EntryFile, Size: int64(len(fileData)), ObjectID: fileOID},
		},
	}
	raw, _ := json.Marshal(tree)
	rdr := memReader{rootOID: raw, fileOID: fileData}

	t.Run("overwrite", func(t *testing.T) {
		dest := t.TempDir()
		if err := os.WriteFile(filepath.Join(dest, "hello.txt"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		stats, err := restore.Run(ctx, restore.Options{
			RootObjectID: rootOID, TargetRoot: dest, Conflict: restore.ConflictOverwrite, Reader: rdr,
		})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dest, "hello.txt"))
		if string(got) != string(fileData) {
			t.Fatalf("overwrite: got %q", got)
		}
		if stats.Files != 1 {
			t.Fatalf("files=%d", stats.Files)
		}
	})

	t.Run("skip", func(t *testing.T) {
		dest := t.TempDir()
		if err := os.WriteFile(filepath.Join(dest, "hello.txt"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		stats, err := restore.Run(ctx, restore.Options{
			RootObjectID: rootOID, TargetRoot: dest, Conflict: restore.ConflictSkip, Reader: rdr,
		})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(dest, "hello.txt"))
		if string(got) != "old" {
			t.Fatalf("skip must leave old content, got %q", got)
		}
		if len(stats.Skipped) < 1 {
			t.Fatal("skip must record a SkipRecord")
		}
	})

	t.Run("rename", func(t *testing.T) {
		dest := t.TempDir()
		if err := os.WriteFile(filepath.Join(dest, "hello.txt"), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		stats, err := restore.Run(ctx, restore.Options{
			RootObjectID: rootOID, TargetRoot: dest, Conflict: restore.ConflictRename, Reader: rdr,
		})
		if err != nil {
			t.Fatal(err)
		}
		old, _ := os.ReadFile(filepath.Join(dest, "hello.txt"))
		if string(old) != "old" {
			t.Fatalf("original must remain: %q", old)
		}
		renamed, err := os.ReadFile(filepath.Join(dest, "hello.txt.restored"))
		if err != nil {
			t.Fatal(err)
		}
		if string(renamed) != string(fileData) {
			t.Fatalf("renamed content: %q", renamed)
		}
		if stats.Files != 1 {
			t.Fatalf("files=%d", stats.Files)
		}
	})
}

func TestSymlinkAndUnsupportedSkip(t *testing.T) {
	ctx := context.Background()
	rootOID := "root"
	tree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: "link", Type: format.EntrySymlink, ReparseData: "target-path"},
			{Name: "rp", Type: format.EntryReparse, ReparseData: "x"},
			{Name: "weird", Type: format.EntryType("device")},
		},
	}
	raw, _ := json.Marshal(tree)
	rdr := memReader{rootOID: raw}
	dest := t.TempDir()
	stats, err := restore.Run(ctx, restore.Options{
		RootObjectID: rootOID, TargetRoot: dest, Conflict: restore.ConflictOverwrite, Reader: rdr,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Symlink created (or skipped with reason on platforms that forbid it).
	if stats.Symlinks == 0 {
		found := false
		for _, s := range stats.Skipped {
			if s.Path != "" && (strings.Contains(s.Reason, "symlink") || filepath.Base(s.Path) == "link") {
				found = true
			}
		}
		if !found {
			t.Fatalf("symlink neither created nor skipped visibly: %+v", stats.Skipped)
		}
	} else {
		got, err := os.Readlink(filepath.Join(dest, "link"))
		if err != nil {
			t.Fatal(err)
		}
		if got != "target-path" {
			t.Fatalf("symlink target %q", got)
		}
	}
	// Reparse + unknown type must be visible skips — never silent.
	var reasons []string
	for _, s := range stats.Skipped {
		if s.Reason == "" {
			t.Fatalf("silent skip: %+v", s)
		}
		reasons = append(reasons, s.Reason)
	}
	if len(reasons) < 2 {
		t.Fatalf("expected skips for reparse+unsupported, got %v", reasons)
	}
}

func TestMissingFileOIDIsError(t *testing.T) {
	ctx := context.Background()
	rootOID := "root"
	tree := format.TreeObject{
		Version: format.FormatVersion,
		Entries: []format.TreeEntry{
			{Name: "x.bin", Type: format.EntryFile, Size: 10, ObjectID: ""},
		},
	}
	raw, _ := json.Marshal(tree)
	rdr := memReader{rootOID: raw}
	_, err := restore.Run(ctx, restore.Options{
		RootObjectID: rootOID, TargetRoot: t.TempDir(), Conflict: restore.ConflictOverwrite, Reader: rdr,
	})
	if err == nil {
		t.Fatal("missing oid for non-empty file must error (no silent omission)")
	}
}
