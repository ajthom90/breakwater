package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob/filesystem"
	"github.com/kopia/kopia/repo/content"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/repo/object"
	"github.com/pkg/errors"
)

// kopiaVault implements Vault using kopia's public repository packages only.
// All kopia imports in Breakwater live in this file (and tests in this package).
type kopiaVault struct {
	repoID   string
	password string
	repoPath string
	cfgPath  string
	cacheDir string

	mu  sync.RWMutex // R: backup/replication; W: prune
	rep repo.Repository
}

func repoPaths(reposDir, repoID string) (repoPath, cfgPath, cacheDir string) {
	repoPath = filepath.Join(reposDir, repoID)
	cfgPath = filepath.Join(repoPath, "breakwater.config")
	cacheDir = filepath.Join(repoPath, ".cache")
	return repoPath, cfgPath, cacheDir
}

func createKopiaVault(ctx context.Context, reposDir, repoID, password string) (*kopiaVault, error) {
	repoPath, cfgPath, cacheDir := repoPaths(reposDir, repoID)
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		return nil, errors.Wrap(err, "mkdir repo")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, errors.Wrap(err, "mkdir cache")
	}

	st, err := filesystem.New(ctx, &filesystem.Options{Path: repoPath}, true)
	if err != nil {
		return nil, errors.Wrap(err, "filesystem storage")
	}
	defer st.Close(ctx) // Connect holds its own handle after init

	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, password); err != nil {
		return nil, errors.Wrap(err, "repo.Initialize")
	}

	// Re-open storage for Connect (Initialize may leave storage in a certain state).
	st2, err := filesystem.New(ctx, &filesystem.Options{Path: repoPath}, false)
	if err != nil {
		return nil, errors.Wrap(err, "filesystem storage reconnect")
	}

	if err := repo.Connect(ctx, cfgPath, st2, password, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{
			Hostname:    "breakwaterd",
			Username:    "breakwater",
			Description: fmt.Sprintf("breakwater machine repo %s", repoID),
		},
		CachingOptions: content.CachingOptions{
			CacheDirectory: cacheDir,
		},
	}); err != nil {
		st2.Close(ctx)
		return nil, errors.Wrap(err, "repo.Connect")
	}

	return openKopiaVault(ctx, reposDir, repoID, password)
}

func openKopiaVault(ctx context.Context, reposDir, repoID, password string) (*kopiaVault, error) {
	repoPath, cfgPath, cacheDir := repoPaths(reposDir, repoID)
	if _, err := os.Stat(cfgPath); err != nil {
		// Config missing: try connect from existing initialized storage.
		if err := ensureConnected(ctx, repoPath, cfgPath, cacheDir, password, repoID); err != nil {
			return nil, err
		}
	}

	rep, err := repo.Open(ctx, cfgPath, password, &repo.Options{})
	if err != nil {
		return nil, errors.Wrap(err, "repo.Open")
	}

	return &kopiaVault{
		repoID:   repoID,
		password: password,
		repoPath: repoPath,
		cfgPath:  cfgPath,
		cacheDir: cacheDir,
		rep:      rep,
	}, nil
}

func ensureConnected(ctx context.Context, repoPath, cfgPath, cacheDir, password, repoID string) error {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return errors.Wrap(err, "mkdir cache")
	}
	st, err := filesystem.New(ctx, &filesystem.Options{Path: repoPath}, false)
	if err != nil {
		return errors.Wrap(err, "filesystem storage")
	}
	return errors.Wrap(repo.Connect(ctx, cfgPath, st, password, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{
			Hostname:    "breakwaterd",
			Username:    "breakwater",
			Description: fmt.Sprintf("breakwater machine repo %s", repoID),
		},
		CachingOptions: content.CachingOptions{
			CacheDirectory: cacheDir,
		},
	}), "repo.Connect")
}

func (v *kopiaVault) Close(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.rep == nil {
		return nil
	}
	err := v.rep.Close(ctx)
	v.rep = nil
	return err
}

func (v *kopiaVault) PutContent(ctx context.Context, data []byte) (ContentID, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	var contentID ContentID
	err := repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "put-content"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			// Single-block object via FIXED-4M: for data ≤4MiB this yields one content
			// whose object ID string is usable as the content address for have/want.
			// (kopia's WriteContent takes internal gather.Bytes; object path is the public API.)
			ow := w.NewObjectWriter(ctx, object.WriterOptions{
				Splitter: SplitterFixed4M,
			})
			if _, err := ow.Write(data); err != nil {
				_ = ow.Close()
				return err
			}
			oid, err := ow.Result()
			if err != nil {
				_ = ow.Close()
				return err
			}
			if err := ow.Close(); err != nil {
				return err
			}
			// For a single-chunk object the object ID equals the content ID string form.
			contentID = ContentID(oid.String())
			// Prefer the first backing content ID when available.
			if ids, verr := w.VerifyObject(ctx, oid); verr == nil && len(ids) > 0 {
				contentID = ContentID(ids[0].String())
			}
			return nil
		})
	return contentID, err
}

func (v *kopiaVault) HasContents(ctx context.Context, ids []ContentID) ([]bool, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	out := make([]bool, len(ids))
	for i, id := range ids {
		cid, err := content.ParseID(string(id))
		if err != nil {
			// Also accept object-ID form from PutContent fallback.
			if oid, oerr := object.ParseID(string(id)); oerr == nil {
				_, verr := v.rep.VerifyObject(ctx, oid)
				out[i] = verr == nil
				continue
			}
			out[i] = false
			continue
		}
		_, err = v.rep.ContentInfo(ctx, cid)
		out[i] = err == nil
	}
	return out, nil
}

func (v *kopiaVault) GetContent(ctx context.Context, id ContentID) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	// Prefer content path; fall back to object open for IDs returned as object strings.
	cid, err := content.ParseID(string(id))
	if err == nil {
		if dr, ok := v.rep.(repo.DirectRepository); ok {
			return dr.ContentReader().GetContent(ctx, cid)
		}
	}
	oid, err := object.ParseID(string(id))
	if err != nil {
		return nil, errors.Wrap(err, "parse content/object id")
	}
	r, err := v.rep.OpenObject(ctx, oid)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (v *kopiaVault) WriteObject(ctx context.Context, splitter string, r io.Reader) (ObjectID, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if splitter == "" {
		splitter = SplitterDynamic
	}
	var oid ObjectID
	err := repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "write-object"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			ow := w.NewObjectWriter(ctx, object.WriterOptions{
				Splitter:   splitter,
				Compressor: "zstd",
			})
			if _, err := io.Copy(ow, r); err != nil {
				_ = ow.Close()
				return err
			}
			id, err := ow.Result()
			if err != nil {
				_ = ow.Close()
				return err
			}
			if err := ow.Close(); err != nil {
				return err
			}
			oid = ObjectID(id.String())
			return nil
		})
	return oid, err
}

func (v *kopiaVault) OpenObject(ctx context.Context, id ObjectID) (io.ReadCloser, error) {
	v.mu.RLock()
	// Caller must hold open until Close; unlock after open is OK for kopia readers.
	defer v.mu.RUnlock()

	oid, err := object.ParseID(string(id))
	if err != nil {
		return nil, errors.Wrap(err, "parse object id")
	}
	r, err := v.rep.OpenObject(ctx, oid)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (v *kopiaVault) VerifyObject(ctx context.Context, id ObjectID) ([]ContentID, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	oid, err := object.ParseID(string(id))
	if err != nil {
		return nil, errors.Wrap(err, "parse object id")
	}
	ids, err := v.rep.VerifyObject(ctx, oid)
	if err != nil {
		return nil, err
	}
	out := make([]ContentID, len(ids))
	for i, c := range ids {
		out[i] = ContentID(c.String())
	}
	return out, nil
}

func (v *kopiaVault) PutSnapshotRecord(ctx context.Context, rec SnapshotRecord) (SnapshotRecordID, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	if rec.Kind == "" {
		return "", errors.New("snapshot kind required")
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = time.Now().UTC()
	}
	labels := map[string]string{
		"type":    string(rec.Kind),
		"machine": rec.MachineID,
	}
	if rec.Source != "" {
		labels["source"] = rec.Source
	}

	var mid SnapshotRecordID
	err := repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "put-snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			id, err := w.PutManifest(ctx, labels, rec)
			if err != nil {
				return err
			}
			mid = SnapshotRecordID(id)
			return nil
		})
	return mid, err
}

func (v *kopiaVault) GetSnapshotRecord(ctx context.Context, id SnapshotRecordID) (*SnapshotRecord, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	var rec SnapshotRecord
	_, err := v.rep.GetManifest(ctx, manifest.ID(id), &rec)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func (v *kopiaVault) ListSnapshotRecords(ctx context.Context, kind SnapshotKind) ([]SnapshotMeta, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	labels := map[string]string{}
	if kind != "" {
		labels["type"] = string(kind)
	} else {
		// List file snapshots by default when empty — caller can pass kind.
		// Empty labels match all manifests; filter to bw-* kinds below.
	}
	entries, err := v.rep.FindManifests(ctx, labels)
	if err != nil {
		return nil, err
	}
	var out []SnapshotMeta
	for _, e := range entries {
		t := e.Labels["type"]
		if kind == "" && t != string(KindFileSnapshot) && t != string(KindImageSnapshot) {
			continue
		}
		if kind != "" && t != string(kind) {
			continue
		}
		out = append(out, SnapshotMeta{
			ID:        SnapshotRecordID(e.ID),
			Kind:      SnapshotKind(t),
			MachineID: e.Labels["machine"],
			ModTime:   e.ModTime,
		})
	}
	return out, nil
}

func (v *kopiaVault) DeleteSnapshotRecord(ctx context.Context, id SnapshotRecordID) error {
	v.mu.RLock()
	defer v.mu.RUnlock()

	return repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "delete-snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return w.DeleteManifest(ctx, manifest.ID(id))
		})
}

func (v *kopiaVault) Prune(ctx context.Context) error {
	// Exclusive: prune must not race with writers.
	v.mu.Lock()
	defer v.mu.Unlock()

	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return errors.New("prune requires direct repository access")
	}

	// Ensure maintenance params exist and are owned by this client.
	err := repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "maintenance-params"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			p, err := maintenance.GetParams(ctx, dw)
			if err != nil {
				p = &maintenance.Params{}
				*p = maintenance.DefaultParams()
			}
			p.Owner = dw.ClientOptions().UsernameAtHost()
			return maintenance.SetParams(ctx, dw, p)
		})
	if err != nil {
		return errors.Wrap(err, "set maintenance params")
	}

	// Re-open as DirectRepositoryWriter path via DirectWriteSession for RunExclusive.
	return repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "prune"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			return maintenance.RunExclusive(ctx, dw, maintenance.ModeFull, true,
				func(ctx context.Context, runParams maintenance.RunParameters) error {
					// SafetyNone: server is sole writer (PLAN: per-repo RW lock).
					return maintenance.Run(ctx, runParams, maintenance.SafetyNone)
				})
		})
}

func (v *kopiaVault) Stats(ctx context.Context) (VaultStats, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()

	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return VaultStats{}, errors.New("stats require direct repository")
	}

	var stats VaultStats
	err := dr.ContentReader().IterateContents(ctx, content.IterateOptions{}, func(info content.Info) error {
		if info.Deleted {
			return nil
		}
		stats.ContentCount++
		stats.TotalSizeBytes += int64(info.PackedLength)
		return nil
	})
	return stats, err
}

// MarshalSnapshotJSON is a helper for tests/debug.
func MarshalSnapshotJSON(rec SnapshotRecord) ([]byte, error) {
	return json.Marshal(rec)
}
