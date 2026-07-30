// Package backup implements portable plain-directory file backup.
//
// Placement: pkg/backup (not agent/internal) so server integration tests and the
// fake agent can share the library; the Windows agent (stage 4) reuses it and
// keeps VSS/BackupRead specifics in agent/internal. Pure os.File I/O — no
// Windows APIs here.
//
// Pipeline (PLAN M2): walk bottom-up → CDC split (pkg/contentid) → have/want
// CheckContents → PutContents misses → PutObjectFromContents (content_ids) →
// per-dir TreeObject → CommitSnapshot.
//
// Error policy (M2-S3 decision, fail-loud):
//   - I/O errors reading a file/dir abort the whole job (no partial success).
//   - Symlinks are stored as format.EntrySymlink with the target in ReparseData.
//   - Unsupported types (devices, sockets, …) are recorded in Stats.Skipped and
//     the job continues — skips are always visible, never silent (S3-F5).
//
// A backup never reports success while silently omitting data.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ajthom90/breakwater/pkg/contentid"
	"github.com/ajthom90/breakwater/pkg/format"
	breakwaterv1 "github.com/ajthom90/breakwater/pkg/proto/breakwater/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ProgressFunc reports backup progress (bytes done/total, phase).
type ProgressFunc func(bytesDone, bytesTotal int64, phase, message string)

// SkipRecord describes an entry deliberately not backed up as file content.
type SkipRecord struct {
	Path   string
	Reason string // e.g. "device", "socket", "named_pipe"
}

// Stats are returned after a successful backup.
type Stats struct {
	BytesRead     int64
	BytesUploaded int64 // payload bytes sent via PutContents (misses only)
	BytesStored   int64 // same as uploaded for agent accounting
	Files         int
	Dirs          int
	Symlinks      int
	Skipped       []SkipRecord // always visible when non-empty (S3-F5)
	SnapshotID    string
	ManifestRef   string
	RootObjectID  string
}

// Client is the DataService surface used by the pipeline (real gRPC or test double).
type Client interface {
	CheckContents(ctx context.Context, jobID string, ids []string) (present []bool, err error)
	PutContent(ctx context.Context, jobID, contentID string, data []byte) (serverID string, err error)
	// PutObjectFromContents materializes a file object from content IDs already stored.
	PutObjectFromContents(ctx context.Context, jobID string, contentIDs []string) (objectID string, err error)
	PutTreeObject(ctx context.Context, jobID string, treeJSON []byte) (objectID string, err error)
	CommitSnapshot(ctx context.Context, req *breakwaterv1.CommitSnapshotRequest) (snapshotID, manifestRef string, err error)
}

// Options configure a directory backup.
type Options struct {
	Source   string // absolute or relative directory path
	JobID    string
	Hasher   *contentid.Hasher
	Client   Client
	Progress ProgressFunc
	// Now defaults to time.Now.
	Now func() time.Time
}

// Run backs up opts.Source into the vault via DataService and commits a snapshot.
func Run(ctx context.Context, opts Options) (*Stats, error) {
	if opts.Source == "" || opts.JobID == "" || opts.Hasher == nil || opts.Client == nil {
		return nil, fmt.Errorf("backup: Source, JobID, Hasher, and Client required")
	}
	root, err := filepath.Abs(opts.Source)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("backup: source is not a directory: %s", root)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	started := now()

	sp, err := contentid.NewSplitter(contentid.SplitterDynamic4M)
	if err != nil {
		return nil, err
	}
	defer sp.Close()

	// Pre-scan total bytes for progress (regular files only; symlinks have no payload).
	var totalBytes int64
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			totalBytes += info.Size()
		}
		return nil
	})

	stats := &Stats{}
	var bytesDone int64
	report := func(phase, msg string) {
		if opts.Progress != nil {
			opts.Progress(bytesDone, totalBytes, phase, msg)
		}
	}
	report("scan", "starting backup")

	// Bottom-up: process directories depth-first post-order.
	rootOID, err := backupDir(ctx, opts, sp, root, root, stats, &bytesDone, totalBytes, report)
	if err != nil {
		return stats, err
	}
	stats.RootObjectID = rootOID
	finished := now()

	snapID, manRef, err := opts.Client.CommitSnapshot(ctx, &breakwaterv1.CommitSnapshotRequest{
		JobId:        opts.JobID,
		Kind:         breakwaterv1.SnapshotKind_SNAPSHOT_KIND_FILE,
		RootObjectId: rootOID,
		Source:       root,
		StartedAt:    timestamppb.New(started),
		FinishedAt:   timestamppb.New(finished),
		BytesRead:    stats.BytesRead,
		BytesStored:  stats.BytesUploaded,
	})
	if err != nil {
		return stats, fmt.Errorf("CommitSnapshot: %w", err)
	}
	stats.SnapshotID = snapID
	stats.ManifestRef = manRef
	stats.BytesStored = stats.BytesUploaded
	report("done", "snapshot "+snapID)
	return stats, nil
}

func backupDir(ctx context.Context, opts Options, sp *contentid.Splitter, root, dir string,
	stats *Stats, bytesDone *int64, totalBytes int64, report func(string, string),
) (objectID string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err // fail-loud
	}
	// Stable order for deterministic trees.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var treeEntries []format.TreeEntry
	for _, ent := range entries {
		name := ent.Name()
		path := filepath.Join(dir, name)
		info, err := ent.Info() // Lstat — does not follow symlinks
		if err != nil {
			return "", fmt.Errorf("stat %s: %w", path, err)
		}

		// Symlinks first (ModeSymlink bit; IsDir/IsRegular false for links).
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", fmt.Errorf("readlink %s: %w", path, err)
			}
			treeEntries = append(treeEntries, format.TreeEntry{
				Name:        name,
				Type:        format.EntrySymlink,
				MtimeNS:     info.ModTime().UnixNano(),
				ReparseData: target,
			})
			stats.Symlinks++
			report("symlink", path)
			continue
		}

		if info.IsDir() {
			childOID, err := backupDir(ctx, opts, sp, root, path, stats, bytesDone, totalBytes, report)
			if err != nil {
				return "", err
			}
			te := format.TreeEntry{Name: name, Type: format.EntryDir, ObjectID: childOID, MtimeNS: info.ModTime().UnixNano()}
			treeEntries = append(treeEntries, te)
			stats.Dirs++
			continue
		}

		if info.Mode().IsRegular() {
			fileOID, n, uploaded, err := backupFile(ctx, opts, sp, path)
			if err != nil {
				return "", fmt.Errorf("file %s: %w", path, err)
			}
			*bytesDone += n
			stats.BytesRead += n
			stats.BytesUploaded += uploaded
			stats.Files++
			report("file", path)
			treeEntries = append(treeEntries, format.TreeEntry{
				Name:     name,
				Type:     format.EntryFile,
				Size:     info.Size(),
				MtimeNS:  info.ModTime().UnixNano(),
				ObjectID: fileOID,
			})
			continue
		}

		// Unsupported types: visible skip (S3-F5), never silent.
		reason := skipReason(info.Mode())
		stats.Skipped = append(stats.Skipped, SkipRecord{Path: path, Reason: reason})
		report("skip", path+" ("+reason+")")
	}

	tree := format.TreeObject{Version: format.FormatVersion, Entries: treeEntries}
	raw, err := json.Marshal(tree)
	if err != nil {
		return "", err
	}
	oid, err := opts.Client.PutTreeObject(ctx, opts.JobID, raw)
	if err != nil {
		return "", fmt.Errorf("PutTreeObject %s: %w", dir, err)
	}
	return oid, nil
}

func skipReason(m os.FileMode) string {
	switch m & os.ModeType {
	case os.ModeDevice:
		return "device"
	case os.ModeNamedPipe:
		return "named_pipe"
	case os.ModeSocket:
		return "socket"
	case os.ModeCharDevice:
		return "char_device"
	default:
		return "unsupported"
	}
}

func backupFile(ctx context.Context, opts Options, sp *contentid.Splitter, path string) (objectID string, bytesRead, uploaded int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return "", 0, 0, err
	}
	bytesRead = int64(len(data))

	chunks, ids, err := contentid.ChunkAndID(opts.Hasher, sp, data)
	if err != nil {
		return "", bytesRead, 0, err
	}

	// Have/want in batches of 4096.
	present := make([]bool, len(ids))
	const batch = 4096
	for i := 0; i < len(ids); i += batch {
		end := i + batch
		if end > len(ids) {
			end = len(ids)
		}
		p, err := opts.Client.CheckContents(ctx, opts.JobID, ids[i:end])
		if err != nil {
			return "", bytesRead, 0, fmt.Errorf("CheckContents: %w", err)
		}
		if len(p) != end-i {
			return "", bytesRead, 0, fmt.Errorf("CheckContents: bitmap length %d want %d", len(p), end-i)
		}
		copy(present[i:end], p)
	}

	for i, p := range present {
		if p {
			continue
		}
		if _, err := opts.Client.PutContent(ctx, opts.JobID, ids[i], chunks[i]); err != nil {
			return "", bytesRead, uploaded, fmt.Errorf("PutContent: %w", err)
		}
		uploaded += int64(len(chunks[i]))
	}

	oid, err := opts.Client.PutObjectFromContents(ctx, opts.JobID, ids)
	if err != nil {
		return "", bytesRead, uploaded, fmt.Errorf("PutObjectFromContents: %w", err)
	}
	return oid, bytesRead, uploaded, nil
}

// GRPCClient adapts DataServiceClient to backup.Client.
type GRPCClient struct {
	DS breakwaterv1.DataServiceClient
}

func (c *GRPCClient) CheckContents(ctx context.Context, jobID string, ids []string) ([]bool, error) {
	resp, err := c.DS.CheckContents(ctx, &breakwaterv1.CheckContentsRequest{
		JobId: jobID, ContentIds: ids,
	})
	if err != nil {
		return nil, err
	}
	return unpackBitmap(resp.GetPresentBitmap(), len(ids)), nil
}

func unpackBitmap(b []byte, n int) []bool {
	out := make([]bool, n)
	for i := 0; i < n; i++ {
		if i/8 < len(b) && b[i/8]&(1<<(i%8)) != 0 {
			out[i] = true
		}
	}
	return out
}

func (c *GRPCClient) PutContent(ctx context.Context, jobID, contentID string, data []byte) (string, error) {
	stream, err := c.DS.PutContents(ctx)
	if err != nil {
		return "", err
	}
	if err := stream.Send(&breakwaterv1.PutContentsRequest{
		JobId: jobID, ContentId: contentID, Data: data, Seq: 1,
	}); err != nil {
		return "", err
	}
	if err := stream.CloseSend(); err != nil {
		return "", err
	}
	resp, err := stream.Recv()
	if err != nil {
		return "", err
	}
	if !resp.GetAccepted() {
		return "", fmt.Errorf("PutContents rejected: %s", resp.GetErrorMessage())
	}
	return resp.GetContentId(), nil
}

func (c *GRPCClient) PutObjectFromContents(ctx context.Context, jobID string, contentIDs []string) (string, error) {
	// S3-F1: first-class content_ids field — no sentinel tree hack.
	resp, err := c.DS.PutTreeObject(ctx, &breakwaterv1.PutTreeObjectRequest{
		JobId: jobID, ContentIds: contentIDs,
	})
	if err != nil {
		return "", err
	}
	return resp.GetObjectId(), nil
}

func (c *GRPCClient) PutTreeObject(ctx context.Context, jobID string, treeJSON []byte) (string, error) {
	resp, err := c.DS.PutTreeObject(ctx, &breakwaterv1.PutTreeObjectRequest{
		JobId: jobID, TreeJson: treeJSON,
	})
	if err != nil {
		return "", err
	}
	return resp.GetObjectId(), nil
}

func (c *GRPCClient) CommitSnapshot(ctx context.Context, req *breakwaterv1.CommitSnapshotRequest) (string, string, error) {
	resp, err := c.DS.CommitSnapshot(ctx, req)
	if err != nil {
		return "", "", err
	}
	return resp.GetSnapshotId(), resp.GetManifestRef(), nil
}
