// Package golden fabricates adversarial filesystem fixtures and compares
// restored trees against the original.
//
// Built in M2, reused forever by every restore assertion (PLAN §Verification).
//
// Fixtures (PLAN-binding):
//
//	portable (Linux/macOS/Windows CI):
//	  - empty (0-byte) files
//	  - multi-MB files (multi-GB is opt-in via Options.LargeFiles)
//	  - unicode names
//	  - nested deep paths (approaching OS limits portably)
//	  - symlink to file + directory
//	  - hardlinks (where supported)
//	  - empty directories
//
//	Windows-only (skip-with-record on non-Windows — never silent):
//	  - SYSTEM-only ACLs
//	  - Alternate Data Streams (ADS)
//	  - sparse files
//	  - >260-char paths (\\?\ extended)
//	  - junction / symlink loops
//	  - deny-share-locked files
//
// Every skip is explicit and reported in Result.Skipped.
package golden

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// FixtureID names a fabricated case.
type FixtureID string

const (
	FixEmptyFile       FixtureID = "empty-file"
	FixSmallText       FixtureID = "small-text"
	FixMultiMB         FixtureID = "multi-mb"
	FixMultiGB         FixtureID = "multi-gb"
	FixUnicodeNames    FixtureID = "unicode-names"
	FixDeepPath        FixtureID = "deep-path"
	FixEmptyDir        FixtureID = "empty-dir"
	FixSymlinkFile     FixtureID = "symlink-file"
	FixSymlinkDir      FixtureID = "symlink-dir"
	FixHardlink        FixtureID = "hardlink"
	FixLongPath        FixtureID = "long-path-gt260" // Windows extended path
	FixACLSystemOnly   FixtureID = "acl-system-only"
	FixADS             FixtureID = "ads"
	FixSparse          FixtureID = "sparse"
	FixJunctionLoop    FixtureID = "junction-symlink-loop"
	FixDenyShareLocked FixtureID = "deny-share-locked"
)

// Skip records an intentionally omitted Windows-only (or unsupported) fixture.
type Skip struct {
	Fixture FixtureID
	Reason  string
}

// Options configure generation.
type Options struct {
	// Root directory to populate (created if missing).
	Root string
	// LargeFiles enables multi-GB fixture (default false — too heavy for CI).
	LargeFiles bool
	// MultiMBSize defaults to 12 MiB (forces multi-chunk CDC).
	MultiMBSize int64
	// IncludeWindows attempts Windows-only fixtures (default: true on Windows).
	// On non-Windows they are always skipped with a record.
	IncludeWindows bool
}

// Result describes what was generated.
type Result struct {
	Root    string
	Created []FixtureID
	Skipped []Skip
	// Paths of interest for tests (relative to Root).
	Paths map[FixtureID][]string
}

// Generate fabricates the golden dataset under opts.Root.
func Generate(opts Options) (*Result, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("golden: Root required")
	}
	if opts.MultiMBSize <= 0 {
		opts.MultiMBSize = 12 << 20 // 12 MiB
	}
	if err := os.MkdirAll(opts.Root, 0o755); err != nil {
		return nil, err
	}
	res := &Result{
		Root:  opts.Root,
		Paths: make(map[FixtureID][]string),
	}

	// --- Portable fixtures ---
	if err := writeFile(opts.Root, "portable/empty.txt", nil); err != nil {
		return nil, err
	}
	res.Created = append(res.Created, FixEmptyFile)
	res.Paths[FixEmptyFile] = []string{"portable/empty.txt"}

	if err := writeFile(opts.Root, "portable/hello.txt", []byte("hello breakwater golden\n")); err != nil {
		return nil, err
	}
	res.Created = append(res.Created, FixSmallText)
	res.Paths[FixSmallText] = []string{"portable/hello.txt"}

	big := make([]byte, opts.MultiMBSize)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := writeFile(opts.Root, "portable/multi-mb.bin", big); err != nil {
		return nil, err
	}
	res.Created = append(res.Created, FixMultiMB)
	res.Paths[FixMultiMB] = []string{"portable/multi-mb.bin"}

	if opts.LargeFiles {
		// Multi-GB: write sparse-ish by seeking if possible; else sequential.
		// Cap at 2 GiB for sanity unless environment is production-scale.
		const gb = int64(2) << 30
		if err := writeLargeFile(filepath.Join(opts.Root, "portable/multi-gb.bin"), gb); err != nil {
			return nil, err
		}
		res.Created = append(res.Created, FixMultiGB)
		res.Paths[FixMultiGB] = []string{"portable/multi-gb.bin"}
	} else {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixMultiGB, Reason: "LargeFiles=false (opt-in; CI default)"})
	}

	// Unicode names (NFC + CJK + emoji where FS allows).
	uniDir := "portable/unicode-名前-📁"
	if err := os.MkdirAll(filepath.Join(opts.Root, uniDir), 0o755); err != nil {
		return nil, err
	}
	uniFile := filepath.Join(uniDir, "café-αβγ.txt")
	if err := writeFile(opts.Root, uniFile, []byte("unicode payload\n")); err != nil {
		return nil, err
	}
	res.Created = append(res.Created, FixUnicodeNames)
	res.Paths[FixUnicodeNames] = []string{uniFile}

	// Deep nested path (portable depth, not full 260+).
	deep := "portable/deep"
	for i := 0; i < 20; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("d%02d", i))
	}
	if err := os.MkdirAll(filepath.Join(opts.Root, deep), 0o755); err != nil {
		return nil, err
	}
	deepFile := filepath.Join(deep, "leaf.txt")
	if err := writeFile(opts.Root, deepFile, []byte("deep\n")); err != nil {
		return nil, err
	}
	res.Created = append(res.Created, FixDeepPath)
	res.Paths[FixDeepPath] = []string{deepFile}

	if err := os.MkdirAll(filepath.Join(opts.Root, "portable/empty-dir"), 0o755); err != nil {
		return nil, err
	}
	res.Created = append(res.Created, FixEmptyDir)
	res.Paths[FixEmptyDir] = []string{"portable/empty-dir"}

	// Symlinks (may fail on Windows without privilege — then skip with record).
	if err := writeFile(opts.Root, "portable/link-target.txt", []byte("target\n")); err != nil {
		return nil, err
	}
	if err := os.Symlink("link-target.txt", filepath.Join(opts.Root, "portable/link-to-file")); err != nil {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixSymlinkFile, Reason: "symlink: " + err.Error()})
	} else {
		res.Created = append(res.Created, FixSymlinkFile)
		res.Paths[FixSymlinkFile] = []string{"portable/link-to-file"}
	}
	if err := os.MkdirAll(filepath.Join(opts.Root, "portable/real-dir"), 0o755); err != nil {
		return nil, err
	}
	if err := os.Symlink("real-dir", filepath.Join(opts.Root, "portable/link-to-dir")); err != nil {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixSymlinkDir, Reason: "symlink: " + err.Error()})
	} else {
		res.Created = append(res.Created, FixSymlinkDir)
		res.Paths[FixSymlinkDir] = []string{"portable/link-to-dir"}
	}

	// Hardlink
	if err := writeFile(opts.Root, "portable/hardlink-a.txt", []byte("hardlinked\n")); err != nil {
		return nil, err
	}
	if err := os.Link(
		filepath.Join(opts.Root, "portable/hardlink-a.txt"),
		filepath.Join(opts.Root, "portable/hardlink-b.txt"),
	); err != nil {
		res.Skipped = append(res.Skipped, Skip{Fixture: FixHardlink, Reason: "hardlink: " + err.Error()})
	} else {
		res.Created = append(res.Created, FixHardlink)
		res.Paths[FixHardlink] = []string{"portable/hardlink-a.txt", "portable/hardlink-b.txt"}
	}

	// --- Windows-only fixtures ---
	wantWin := opts.IncludeWindows || runtime.GOOS == "windows"
	if runtime.GOOS != "windows" {
		// Always skip-with-record (S3-F5 lesson: never silent).
		for _, id := range []FixtureID{
			FixLongPath, FixACLSystemOnly, FixADS, FixSparse, FixJunctionLoop, FixDenyShareLocked,
		} {
			res.Skipped = append(res.Skipped, Skip{
				Fixture: id,
				Reason:  fmt.Sprintf("windows-only (running on %s)", runtime.GOOS),
			})
		}
		_ = wantWin
		return res, nil
	}

	if err := generateWindows(opts.Root, res); err != nil {
		return nil, err
	}
	return res, nil
}

func writeFile(root, rel string, data []byte) error {
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

func writeLargeFile(path string, size int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	// Write first + last 4KiB so content is non-zero at edges; hole in the middle
	// may or may not be sparse depending on FS — size is what matters.
	head := make([]byte, 4096)
	for i := range head {
		head[i] = 0xAB
	}
	if _, err := f.Write(head); err != nil {
		return err
	}
	if size > 8192 {
		if _, err := f.Seek(size-4096, 0); err != nil {
			return err
		}
		tail := make([]byte, 4096)
		for i := range tail {
			tail[i] = 0xCD
		}
		if _, err := f.Write(tail); err != nil {
			return err
		}
	}
	return f.Truncate(size)
}

// CompareOptions control equality checks.
type CompareOptions struct {
	// CompareTimestamps requires mtime equality within TimestampSkew.
	CompareTimestamps bool
	TimestampSkew     time.Duration
	// CompareACL/ADS only meaningful on Windows; on other OS they are no-ops
	// (reported in SkippedChecks, never silent failure).
	CompareACL bool
	CompareADS bool
}

// Diff describes one inequality.
type Diff struct {
	Path   string
	Field  string // content|missing|extra|mtime|acl|ads|type
	Detail string
}

// CompareResult is the full comparison report.
type CompareResult struct {
	Diffs         []Diff
	SkippedChecks []Skip // e.g. ACL compare on Linux
	MatchedFiles  int
}

// Equal reports whether byte (+ optional metadata) equality held.
func (r *CompareResult) Equal() bool { return r != nil && len(r.Diffs) == 0 }

// Compare asserts that restored mirrors original for portable content.
// Symlinks are compared as link targets (not followed). Hardlink content is
// compared as independent paths (byte equality of both names).
func Compare(original, restored string, opts CompareOptions) (*CompareResult, error) {
	if opts.TimestampSkew <= 0 {
		opts.TimestampSkew = time.Second
	}
	out := &CompareResult{}

	if runtime.GOOS != "windows" {
		if opts.CompareACL {
			out.SkippedChecks = append(out.SkippedChecks, Skip{Fixture: FixACLSystemOnly, Reason: "ACL compare not available on " + runtime.GOOS})
			opts.CompareACL = false
		}
		if opts.CompareADS {
			out.SkippedChecks = append(out.SkippedChecks, Skip{Fixture: FixADS, Reason: "ADS compare not available on " + runtime.GOOS})
			opts.CompareADS = false
		}
	}

	// Walk original; every path must exist in restored with equal content/type.
	err := filepath.WalkDir(original, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(original, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		other := filepath.Join(restored, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}

		// Symlink: compare target strings.
		if info.Mode()&os.ModeSymlink != 0 {
			want, err := os.Readlink(path)
			if err != nil {
				return err
			}
			got, err := os.Readlink(other)
			if err != nil {

				out.Diffs = append(out.Diffs, Diff{Path: rel, Field: "missing", Detail: "symlink: " + err.Error()})
				return nil
			}
			if got != want {

				out.Diffs = append(out.Diffs, Diff{Path: rel, Field: "type", Detail: fmt.Sprintf("symlink want %q got %q", want, got)})
			} else {
				out.MatchedFiles++
			}
			return nil
		}

		if d.IsDir() {
			st, err := os.Stat(other)
			if err != nil || !st.IsDir() {

				out.Diffs = append(out.Diffs, Diff{Path: rel, Field: "missing", Detail: "directory"})
			}
			return nil
		}

		if !info.Mode().IsRegular() {
			// Other special files: skip with record rather than silent.
			out.SkippedChecks = append(out.SkippedChecks, Skip{
				Fixture: FixtureID("special:" + rel),
				Reason:  "non-regular file type " + info.Mode().String(),
			})
			return nil
		}

		want, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		got, err := os.ReadFile(other)
		if err != nil {
			out.Diffs = append(out.Diffs, Diff{Path: rel, Field: "missing", Detail: err.Error()})
			return nil
		}
		if string(got) != string(want) {
			out.Diffs = append(out.Diffs, Diff{
				Path: rel, Field: "content",
				Detail: fmt.Sprintf("len want=%d got=%d", len(want), len(got)),
			})
			return nil
		}
		if opts.CompareTimestamps {
			oi, _ := os.Stat(path)
			ri, err := os.Stat(other)
			if err == nil && oi != nil {
				delta := oi.ModTime().Sub(ri.ModTime())
				if delta < 0 {
					delta = -delta
				}
				if delta > opts.TimestampSkew {

					out.Diffs = append(out.Diffs, Diff{
						Path: rel, Field: "mtime",
						Detail: fmt.Sprintf("skew %v > %v", delta, opts.TimestampSkew),
					})
				}
			}
		}
		if opts.CompareACL {
			if err := compareACL(path, other); err != nil {

				out.Diffs = append(out.Diffs, Diff{Path: rel, Field: "acl", Detail: err.Error()})
			}
		}
		if opts.CompareADS {
			if err := compareADS(path, other); err != nil {

				out.Diffs = append(out.Diffs, Diff{Path: rel, Field: "ads", Detail: err.Error()})
			}
		}
		out.MatchedFiles++
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Extra files in restored (not in original) — report as diffs.
	_ = filepath.WalkDir(restored, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(restored, path)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(original, rel)); os.IsNotExist(err) {
			out.Diffs = append(out.Diffs, Diff{Path: rel, Field: "extra", Detail: "present only in restored"})
		}
		return nil
	})

	return out, nil
}
