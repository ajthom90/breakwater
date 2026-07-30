package golden_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ajthom90/breakwater/tools/golden"
)

func TestGenerate_Portable(t *testing.T) {
	root := t.TempDir()
	res, err := golden.Generate(golden.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Created) < 5 {
		t.Fatalf("created=%v too few", res.Created)
	}
	// Multi-GB must be explicit skip when LargeFiles=false.
	var sawGBSkip bool
	for _, s := range res.Skipped {
		if s.Fixture == golden.FixMultiGB {
			sawGBSkip = true
			if s.Reason == "" {
				t.Fatal("multi-gb skip must have reason")
			}
		}
	}
	if !sawGBSkip {
		t.Fatal("expected multi-gb skip record")
	}
	// Windows fixtures skipped with reason on non-Windows.
	if runtime.GOOS != "windows" {
		winSkips := 0
		for _, s := range res.Skipped {
			switch s.Fixture {
			case golden.FixADS, golden.FixACLSystemOnly, golden.FixSparse,
				golden.FixLongPath, golden.FixJunctionLoop, golden.FixDenyShareLocked:
				winSkips++
				if s.Reason == "" {
					t.Fatalf("silent skip for %s", s.Fixture)
				}
			}
		}
		if winSkips < 6 {
			t.Fatalf("expected 6 windows-only skips, got %d: %+v", winSkips, res.Skipped)
		}
	}
	// Round-trip compare against itself.
	cmp, err := golden.Compare(root, root, golden.CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !cmp.Equal() {
		t.Fatalf("self-compare failed: %+v", cmp.Diffs)
	}
	t.Logf("created=%v skipped=%d matched=%d", res.Created, len(res.Skipped), cmp.MatchedFiles)
}

func TestCompare_DetectsContentDiff(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "f.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "f.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmp, err := golden.Compare(a, b, golden.CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Equal() {
		t.Fatal("expected inequality")
	}
	if len(cmp.Diffs) < 1 || cmp.Diffs[0].Field != "content" {
		t.Fatalf("diffs=%+v", cmp.Diffs)
	}
}

func TestCompare_DetectsMissing(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "only-a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmp, err := golden.Compare(a, b, golden.CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Equal() {
		t.Fatal("expected missing")
	}
}

func TestNoSilentWindowsSkip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows fixtures run for real")
	}
	res, err := golden.Generate(golden.Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res.Skipped {
		if s.Reason == "" {
			t.Fatalf("silent skip: %+v", s)
		}
	}
}
