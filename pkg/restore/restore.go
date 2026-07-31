// Package restore implements portable snapshot restore (tree walk → files on disk).
//
// Used by the agent (JOB_TYPE_RESTORE) and bwctl. Pure portable I/O — no Windows
// BackupWrite here. ACL/ADS restore via BackupWrite is Windows-runtime work and
// must not be claimed until proven (see PROGRESS untested-on-Windows).
//
// Conflict policy is explicit: overwrite | rename | skip (tested).
// Unsupported entry types produce a visible SkipRecord — never a silent omission
// (S3-F5 lesson).
package restore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ajthom90/breakwater/pkg/format"
)

// ConflictPolicy controls what happens when a path already exists at the target.
type ConflictPolicy string

const (
	// ConflictOverwrite replaces existing files/symlinks (dirs are reused).
	ConflictOverwrite ConflictPolicy = "overwrite"
	// ConflictRename writes to path + ".restored" / ".restored.N" if needed.
	ConflictRename ConflictPolicy = "rename"
	// ConflictSkip leaves the existing path and records a skip.
	ConflictSkip ConflictPolicy = "skip"
)

// ValidConflict reports whether p is a supported policy.
func ValidConflict(p ConflictPolicy) bool {
	switch p {
	case ConflictOverwrite, ConflictRename, ConflictSkip:
		return true
	default:
		return false
	}
}

// SkipRecord describes a path that was not restored as intended.
type SkipRecord struct {
	Path   string
	Reason string
}

// Stats summarize a restore run.
type Stats struct {
	Files    int
	Dirs     int
	Symlinks int
	Bytes    int64
	Skipped  []SkipRecord
	// Errors are hard failures that aborted the walk (also returned as error).
	// Soft skips go to Skipped only.
}

// ProgressFunc reports restore progress.
type ProgressFunc func(bytesDone int64, phase, message string)

// ObjectReader loads snapshot objects (tree JSON or file bytes) by object ID.
// Implemented over RestoreService.GetObject (gRPC) or a local vault adapter.
type ObjectReader interface {
	// OpenObject streams object bytes. Caller must Close the reader.
	OpenObject(ctx context.Context, objectID string) (io.ReadCloser, error)
}

// Options configure a restore run.
type Options struct {
	// RootObjectID is the snapshot root TreeObject.
	RootObjectID string
	// TargetRoot is the directory to restore into (created if missing).
	TargetRoot string
	// Conflict policy (required).
	Conflict ConflictPolicy
	// Reader loads objects from the authorized restore path.
	Reader ObjectReader
	// Progress is optional.
	Progress ProgressFunc
	// PlatformRestore is optional Windows ACL/ADS hook (nil on portable).
	// When non-nil it is called after writing a regular file with the tree entry.
	// Untested on Windows until VM evidence exists — stub only.
	PlatformRestore func(path string, ent format.TreeEntry) error
}

// Run restores the snapshot tree at opts.RootObjectID into opts.TargetRoot.
func Run(ctx context.Context, opts Options) (*Stats, error) {
	if opts.RootObjectID == "" || opts.TargetRoot == "" || opts.Reader == nil {
		return nil, fmt.Errorf("restore: RootObjectID, TargetRoot, and Reader required")
	}
	if !ValidConflict(opts.Conflict) {
		return nil, fmt.Errorf("restore: invalid conflict policy %q (want overwrite|rename|skip)", opts.Conflict)
	}
	if err := os.MkdirAll(opts.TargetRoot, 0o755); err != nil {
		return nil, err
	}
	stats := &Stats{}
	if err := restoreDir(ctx, opts, opts.RootObjectID, opts.TargetRoot, stats); err != nil {
		return stats, err
	}
	return stats, nil
}

func report(opts Options, bytes int64, phase, msg string) {
	if opts.Progress != nil {
		opts.Progress(bytes, phase, msg)
	}
}

func restoreDir(ctx context.Context, opts Options, oid, dir string, stats *Stats) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := readAll(ctx, opts.Reader, oid)
	if err != nil {
		return fmt.Errorf("open tree %s: %w", oid, err)
	}
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("decode tree %s: %w", oid, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stats.Dirs++

	for _, ent := range tree.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := ent.Name
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
			stats.Skipped = append(stats.Skipped, SkipRecord{Path: filepath.Join(dir, name), Reason: "invalid_name"})
			continue
		}
		path := filepath.Join(dir, name)

		switch ent.Type {
		case format.EntryDir:
			if ent.ObjectID == "" {
				stats.Skipped = append(stats.Skipped, SkipRecord{Path: path, Reason: "dir_missing_oid"})
				continue
			}
			if err := restoreDir(ctx, opts, ent.ObjectID, path, stats); err != nil {
				return err
			}

		case format.EntryFile:
			if err := restoreFile(ctx, opts, ent, path, stats); err != nil {
				return err
			}

		case format.EntrySymlink:
			if err := restoreSymlink(opts, ent, path, stats); err != nil {
				// Symlink failure on this platform → visible skip, not silent.
				stats.Skipped = append(stats.Skipped, SkipRecord{Path: path, Reason: "symlink: " + err.Error()})
			}

		case format.EntryReparse:
			// Portable: reparse points are Windows-specific; record skip.
			stats.Skipped = append(stats.Skipped, SkipRecord{Path: path, Reason: "reparse_not_supported_portable"})
			report(opts, stats.Bytes, "skip", path+" (reparse)")

		default:
			stats.Skipped = append(stats.Skipped, SkipRecord{
				Path: path, Reason: "unsupported_entry_type:" + string(ent.Type),
			})
			report(opts, stats.Bytes, "skip", path)
		}
	}
	return nil
}

func restoreFile(ctx context.Context, opts Options, ent format.TreeEntry, path string, stats *Stats) error {
	if ent.ObjectID == "" {
		// Empty file may have empty oid only if size 0 — still an error if size>0.
		if ent.Size > 0 {
			return fmt.Errorf("restore file %s: missing object id (size=%d)", path, ent.Size)
		}
		// Zero-size with no oid: write empty file after conflict resolve.
	}

	dest, skip, err := resolveConflict(path, opts.Conflict, false)
	if err != nil {
		return err
	}
	if skip {
		stats.Skipped = append(stats.Skipped, SkipRecord{Path: path, Reason: "conflict_skip"})
		report(opts, stats.Bytes, "skip", path+" (conflict)")
		return nil
	}

	var data []byte
	if ent.ObjectID != "" {
		data, err = readAll(ctx, opts.Reader, ent.ObjectID)
		if err != nil {
			return fmt.Errorf("read file object %s (%s): %w", path, ent.ObjectID, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	// Write via temp + rename for atomicity on the destination path.
	tmp := dest + ".bw-tmp-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}

	if ent.MtimeNS > 0 {
		mt := time.Unix(0, ent.MtimeNS)
		_ = os.Chtimes(dest, mt, mt)
	}

	// ADS/ACL via platform hook (Windows) — stub path only; untested.
	if opts.PlatformRestore != nil {
		if err := opts.PlatformRestore(dest, ent); err != nil {
			stats.Skipped = append(stats.Skipped, SkipRecord{Path: dest, Reason: "platform_restore: " + err.Error()})
		}
	} else if len(ent.ADS) > 0 || ent.SecurityDescriptor != "" {
		// Portable: record that Windows metadata was not applied.
		if len(ent.ADS) > 0 {
			stats.Skipped = append(stats.Skipped, SkipRecord{Path: dest, Reason: "ads_restore_untested_on_windows"})
		}
		if ent.SecurityDescriptor != "" {
			stats.Skipped = append(stats.Skipped, SkipRecord{Path: dest, Reason: "acl_restore_untested_on_windows"})
		}
	}

	stats.Files++
	stats.Bytes += int64(len(data))
	report(opts, stats.Bytes, "file", dest)
	return nil
}

func restoreSymlink(opts Options, ent format.TreeEntry, path string, stats *Stats) error {
	target := ent.ReparseData
	if target == "" {
		stats.Skipped = append(stats.Skipped, SkipRecord{Path: path, Reason: "symlink_empty_target"})
		return nil
	}
	dest, skip, err := resolveConflict(path, opts.Conflict, true)
	if err != nil {
		return err
	}
	if skip {
		stats.Skipped = append(stats.Skipped, SkipRecord{Path: path, Reason: "conflict_skip"})
		return nil
	}
	// Remove existing (file/symlink) so Symlink succeeds.
	_ = os.Remove(dest)
	if err := os.Symlink(target, dest); err != nil {
		return err
	}
	stats.Symlinks++
	report(opts, stats.Bytes, "symlink", dest)
	return nil
}

// resolveConflict returns the path to write, whether to skip, and error.
func resolveConflict(path string, policy ConflictPolicy, isSymlink bool) (dest string, skip bool, err error) {
	_, err = os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, false, nil
		}
		return "", false, err
	}
	// Exists.
	switch policy {
	case ConflictOverwrite:
		// Directories: leave in place for overwrite of children.
		st, err := os.Lstat(path)
		if err != nil {
			return "", false, err
		}
		if st.IsDir() && !isSymlink {
			return path, false, nil
		}
		if err := os.RemoveAll(path); err != nil {
			return "", false, err
		}
		return path, false, nil
	case ConflictSkip:
		return "", true, nil
	case ConflictRename:
		return uniqueRestoredName(path), false, nil
	default:
		return "", false, fmt.Errorf("unknown conflict policy %q", policy)
	}
}

func uniqueRestoredName(path string) string {
	cand := path + ".restored"
	if _, err := os.Lstat(cand); os.IsNotExist(err) {
		return cand
	}
	for i := 2; i < 10000; i++ {
		cand = fmt.Sprintf("%s.restored.%d", path, i)
		if _, err := os.Lstat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return fmt.Sprintf("%s.restored.%d", path, time.Now().UnixNano())
}

func readAll(ctx context.Context, r ObjectReader, oid string) ([]byte, error) {
	rc, err := r.OpenObject(ctx, oid)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Bound individual object reads (trees + files). Large files stream via ReadAll
	// from the gRPC stream implementation which already chunked.
	return io.ReadAll(rc)
}
