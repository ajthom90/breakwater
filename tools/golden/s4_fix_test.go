package golden_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ajthom90/breakwater/tools/golden"
)

// TestS4F6_PlainExtraFileDetected permanently covers the extra-data path.
// Passes on 37e5fc3 (happy path works); kept so the path cannot regress silently.
func TestS4F6_PlainExtraFileDetected(t *testing.T) {
	orig, restored := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(orig, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "extra.txt"), []byte("extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmp, err := golden.Compare(orig, restored, golden.CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Equal() {
		t.Fatal("S4-F6: plain extra file in restored must make Equal() false")
	}
	found := false
	for _, d := range cmp.Diffs {
		if d.Field == "extra" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected Field=extra diff; got %+v", cmp.Diffs)
	}
}

// TestS4F6_ExtraBehindUnreadableDirMustNotEqual is the BLOCKER probe:
// an unreadable subtree in restored must not report Equal with zero diffs.
// Must FAIL on unmodified 37e5fc3 (walk error discarded → false equality).
func TestS4F6_ExtraBehindUnreadableDirMustNotEqual(t *testing.T) {
	if runtime.GOOS == "windows" {
		// chmod 000 is not a reliable unreadable-dir probe on Windows.
		t.Skip("chmod-000 unreadable-dir probe is unix-specific; Windows covered by CI SD/ACL paths")
	}
	orig, restored := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(orig, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(restored, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Extra data behind a directory the walker cannot enter.
	hidden := filepath.Join(restored, "hidden")
	if err := os.Mkdir(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "extra.bin"), []byte("secret extra"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) })

	cmp, err := golden.Compare(orig, restored, golden.CompareOptions{})
	// Either return a walk error OR report a non-equal result with a walk-error
	// (or extra) diff. Never Equal with empty diffs and nil error.
	if err == nil && cmp != nil && cmp.Equal() && len(cmp.Diffs) == 0 && len(cmp.SkippedChecks) == 0 {
		t.Fatalf("S4-F6 BLOCKER: Compare certified Equal=true diffs=0 skipped=0 err=<nil> while extra data sits behind unreadable dir %s", hidden)
	}
	if err == nil && cmp != nil && cmp.Equal() {
		// Equal() is true only when Diffs empty — already covered above.
		// If Equal somehow true with skips only, still fail: skips are not a walk error.
		t.Fatalf("S4-F6: Equal() true despite incomplete restored walk; diffs=%+v skipped=%+v", cmp.Diffs, cmp.SkippedChecks)
	}
	t.Logf("S4-F6 fixed path: err=%v equal=%v diffs=%+v", err, cmp != nil && cmp.Equal(), cmp)
}
