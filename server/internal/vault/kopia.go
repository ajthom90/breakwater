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

	"github.com/ajthom90/breakwater/pkg/format"
)

// MaxPutContentBytes is the maximum payload for PutContent (one FIXED-4M block).
const MaxPutContentBytes = 4 << 20 // 4 MiB

// MaxMarkObjectBytes caps how much of a tree/image root the mark phase reads into
// memory (R3-2). Real TreeObject / ImageManifest JSON is far smaller; a root
// larger than this is treated as corrupt and fails the prune (fail closed).
// Without the cap, a flat multi-GiB root (pre-R3-1 test artifact) would OOM.
const MaxMarkObjectBytes = 16 << 20 // 16 MiB

var errVaultClosed = fmt.Errorf("vault is closed")

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
		return nil, fmt.Errorf("mkdir repo: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir cache: %w", err)
	}

	st, err := filesystem.New(ctx, &filesystem.Options{Path: repoPath}, true)
	if err != nil {
		return nil, fmt.Errorf("filesystem storage: %w", err)
	}
	defer st.Close(ctx) // Connect holds its own handle after init

	if err := repo.Initialize(ctx, st, &repo.NewRepositoryOptions{}, password); err != nil {
		return nil, fmt.Errorf("repo.Initialize: %w", err)
	}

	// Re-open storage for Connect (Initialize may leave storage in a certain state).
	st2, err := filesystem.New(ctx, &filesystem.Options{Path: repoPath}, false)
	if err != nil {
		return nil, fmt.Errorf("filesystem storage reconnect: %w", err)
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
		return nil, fmt.Errorf("repo.Connect: %w", err)
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
		return nil, fmt.Errorf("repo.Open: %w", err)
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
		return fmt.Errorf("mkdir cache: %w", err)
	}
	st, err := filesystem.New(ctx, &filesystem.Options{Path: repoPath}, false)
	if err != nil {
		return fmt.Errorf("filesystem storage: %w", err)
	}
	if err := repo.Connect(ctx, cfgPath, st, password, &repo.ConnectOptions{
		ClientOptions: repo.ClientOptions{
			Hostname:    "breakwaterd",
			Username:    "breakwater",
			Description: fmt.Sprintf("breakwater machine repo %s", repoID),
		},
		CachingOptions: content.CachingOptions{
			CacheDirectory: cacheDir,
		},
	}); err != nil {
		return fmt.Errorf("repo.Connect: %w", err)
	}
	return nil
}

func (v *kopiaVault) requireOpen() error {
	if v.rep == nil {
		return errVaultClosed
	}
	return nil
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

// HashingKey returns the repo's content-ID HMAC secret and algorithm name.
// Never returns encryption keys or the master key (PLAN: hashing key only).
func (v *kopiaVault) HashingKey(ctx context.Context) (secret []byte, algorithm string, err error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return nil, "", err
	}
	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return nil, "", fmt.Errorf("hashing key requires direct repository")
	}
	// ContentFormat() embeds hashing.Parameters (GetHmacSecret / GetHashFunction).
	// Do NOT use FormatManager().GetMasterKey() or encryption parameters.
	cf := dr.ContentReader().ContentFormat()
	sec := cf.GetHmacSecret()
	out := make([]byte, len(sec))
	copy(out, sec)
	return out, cf.GetHashFunction(), nil
}

func (v *kopiaVault) PutContent(ctx context.Context, data []byte) (ContentID, error) {
	if len(data) > MaxPutContentBytes {
		return "", fmt.Errorf("PutContent: payload %d bytes exceeds max %d (use WriteObject for larger data)", len(data), MaxPutContentBytes)
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return "", err
	}

	var contentID ContentID
	err := repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "put-content"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			// Single-block object via FIXED-4M: for data ≤4MiB this yields one content.
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
			ids, verr := w.VerifyObject(ctx, oid)
			if verr != nil {
				return fmt.Errorf("VerifyObject after PutContent: %w", verr)
			}
			if len(ids) == 0 {
				return fmt.Errorf("VerifyObject returned no content IDs")
			}
			if len(ids) != 1 {
				return fmt.Errorf("PutContent expected 1 content ID, got %d", len(ids))
			}
			contentID = ContentID(ids[0].String())
			return nil
		})
	return contentID, err
}

func (v *kopiaVault) HasContents(ctx context.Context, ids []ContentID) ([]bool, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return nil, err
	}

	out := make([]bool, len(ids))
	for i, id := range ids {
		cid, err := content.ParseID(string(id))
		if err != nil {
			out[i] = false
			continue
		}
		info, err := v.rep.ContentInfo(ctx, cid)
		out[i] = err == nil && !info.Deleted
	}
	return out, nil
}

func (v *kopiaVault) GetContent(ctx context.Context, id ContentID) ([]byte, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return nil, err
	}

	cid, err := content.ParseID(string(id))
	if err != nil {
		return nil, fmt.Errorf("parse content id: %w", err)
	}
	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return nil, fmt.Errorf("GetContent requires direct repository")
	}
	return dr.ContentReader().GetContent(ctx, cid)
}

func (v *kopiaVault) WriteObject(ctx context.Context, splitter string, r io.Reader) (ObjectID, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return "", err
	}

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

// OpenObject returns a reader for an object.
//
// Invariant (M2): the caller must finish reading before Prune runs on this vault.
// The read lock is released when this method returns (not when the reader closes),
// so concurrent prune can race a long-lived stream. Per-repo job serialization
// in the scheduler must cover open restore streams (see REVIEW-M1 M2).
func (v *kopiaVault) OpenObject(ctx context.Context, id ObjectID) (io.ReadCloser, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return nil, err
	}

	oid, err := object.ParseID(string(id))
	if err != nil {
		return nil, fmt.Errorf("parse object id: %w", err)
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
	if err := v.requireOpen(); err != nil {
		return nil, err
	}

	oid, err := object.ParseID(string(id))
	if err != nil {
		return nil, fmt.Errorf("parse object id: %w", err)
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
	if err := v.requireOpen(); err != nil {
		return "", err
	}

	if rec.Kind == "" {
		return "", fmt.Errorf("snapshot kind required")
	}
	// R2-3: reject unknown kinds at the write boundary.
	if !ValidSnapshotKind(rec.Kind) {
		return "", fmt.Errorf("unknown snapshot kind %q (want %s or %s)", rec.Kind, KindFileSnapshot, KindImageSnapshot)
	}
	// R2-4: validate RootObjectID at write time so a garbage OID cannot wedge Prune.
	if rec.RootObjectID != "" {
		if _, err := object.ParseID(string(rec.RootObjectID)); err != nil {
			return "", fmt.Errorf("invalid root object id %q: %w", rec.RootObjectID, err)
		}
		// R3-1: root must decode as the kind's format (TreeObject / ImageManifest).
		if err := validateSnapshotRoot(ctx, v.rep, rec.Kind, rec.RootObjectID); err != nil {
			return "", err
		}
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

// validateSnapshotRoot ensures the root object decodes as the kind requires (R3-1).
func validateSnapshotRoot(ctx context.Context, rep repo.Repository, kind SnapshotKind, root ObjectID) error {
	raw, err := readObjectBytes(ctx, rep, root)
	if err != nil {
		return fmt.Errorf("read snapshot root %s: %w", root, err)
	}
	switch kind {
	case KindFileSnapshot:
		var tree format.TreeObject
		if err := json.Unmarshal(raw, &tree); err != nil {
			return fmt.Errorf("file snapshot root %s must be a TreeObject JSON: %w", root, err)
		}
	case KindImageSnapshot:
		var img format.ImageManifest
		if err := json.Unmarshal(raw, &img); err != nil {
			return fmt.Errorf("image snapshot root %s must be an ImageManifest JSON: %w", root, err)
		}
	}
	return nil
}

func (v *kopiaVault) GetSnapshotRecord(ctx context.Context, id SnapshotRecordID) (*SnapshotRecord, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return nil, err
	}

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
	if err := v.requireOpen(); err != nil {
		return nil, err
	}

	labels := map[string]string{}
	if kind != "" {
		labels["type"] = string(kind)
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
		// Load payload for snapshot Timestamp (labels only have ModTime).
		// R2-12: propagate GetManifest errors — never silently substitute ModTime.
		var rec SnapshotRecord
		if _, err := v.rep.GetManifest(ctx, e.ID, &rec); err != nil {
			return nil, fmt.Errorf("get manifest %s for list: %w", e.ID, err)
		}
		ts := e.ModTime
		if !rec.Timestamp.IsZero() {
			ts = rec.Timestamp
		}
		out = append(out, SnapshotMeta{
			ID:        SnapshotRecordID(e.ID),
			Kind:      SnapshotKind(t),
			MachineID: e.Labels["machine"],
			Timestamp: ts,
			ModTime:   e.ModTime,
		})
	}
	return out, nil
}

func (v *kopiaVault) DeleteSnapshotRecord(ctx context.Context, id SnapshotRecordID) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return err
	}

	return repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "delete-snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return w.DeleteManifest(ctx, manifest.ID(id))
		})
}

// Prune implements PLAN mark-and-sweep with recursive tree/image walk (R2-1)
// and a configurable minimum content age (R2-2, default 24h).
//
//	mark  = walk live snapshot records → trees/manifests → VerifyObject → live set
//	sweep = IterateContents → DeleteContent(unmarked, aged) → DropDeletedContents → maintenance.Run
//
// Prefixed contents (e.g. manifest "m") are left for kopia maintenance; only
// unprefixed user content is subject to our mark set.
//
// Two write sessions are required: delete markers must be committed before
// DropDeletedContents / RunExclusive refresh indexes from storage.
//
// Serialization: must not run concurrently with an open backup session on this
// repo (see Vault interface). MinContentAge is a safety net for races the
// scheduler fails to prevent.
func (v *kopiaVault) Prune(ctx context.Context, opts ...PruneOption) error {
	cfg := resolvePruneOptions(opts)

	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.requireOpen(); err != nil {
		return err
	}

	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return fmt.Errorf("prune requires direct repository access")
	}

	// Session 1: mark live roots and DeleteContent unmarked aged user contents.
	err := repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "prune-mark-sweep"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			live, err := markLiveContents(ctx, dw)
			if err != nil {
				return fmt.Errorf("mark live contents: %w", err)
			}

			cm := dw.ContentManager()
			cutoff := time.Time{}
			if cfg.minContentAge > 0 {
				cutoff = dw.Time().Add(-cfg.minContentAge)
			}

			var toDelete []content.ID
			err = cm.IterateContents(ctx, content.IterateOptions{}, func(info content.Info) error {
				if info.Deleted {
					return nil
				}
				id := info.ContentID
				// Preserve system/manifest-prefixed contents; kopia maintenance owns those.
				if id.HasPrefix() {
					return nil
				}
				if _, ok := live[id]; ok {
					return nil
				}
				// R2-2: never delete contents younger than the min-age window.
				if !cutoff.IsZero() && info.Timestamp().After(cutoff) {
					return nil
				}
				toDelete = append(toDelete, id)
				return nil
			})
			if err != nil {
				return fmt.Errorf("iterate contents: %w", err)
			}

			for _, id := range toDelete {
				if err := cm.DeleteContent(ctx, id); err != nil {
					return fmt.Errorf("DeleteContent %s: %w", id, err)
				}
			}
			return nil
		})
	if err != nil {
		return err
	}

	// Session 2: drop deleted contents from indexes and run pack GC.
	// R3-5: when minContentAge > 0, derive maintenance SafetyParameters from it
	// so blob GC honors the same window; SafetyNone only for WithMinContentAge(0).
	safety := safetyForMinAge(cfg.minContentAge)
	return repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "prune-gc"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			p, err := maintenance.GetParams(ctx, dw)
			if err != nil {
				pp := maintenance.DefaultParams()
				p = &pp
			}
			p.Owner = dw.ClientOptions().UsernameAtHost()
			if err := maintenance.SetParams(ctx, dw, p); err != nil {
				return fmt.Errorf("set maintenance params: %w", err)
			}

			// dropDeletedBefore in the future so all currently-deleted contents qualify.
			if err := maintenance.DropDeletedContents(ctx, dw, dw.Time().Add(time.Hour), safety); err != nil {
				return fmt.Errorf("DropDeletedContents: %w", err)
			}

			return maintenance.RunExclusive(ctx, dw, maintenance.ModeFull, true,
				func(ctx context.Context, runParams maintenance.RunParameters) error {
					return maintenance.Run(ctx, runParams, safety)
				})
		})
}

// safetyForMinAge builds kopia maintenance SafetyParameters from our min-content-age
// window (R3-5). Zero min-age (explicit test path) keeps SafetyNone so young test
// data can be fully reclaimed in one Prune call.
func safetyForMinAge(minAge time.Duration) maintenance.SafetyParameters {
	if minAge <= 0 {
		return maintenance.SafetyNone
	}
	// Server is sole writer on local FS (strongly consistent). We still apply
	// BlobDeleteMinAge / SessionExpirationAge ≥ minAge so unflushed sessions and
	// young blobs are not GC'd below the content-layer guard. RequireTwoGCCycles
	// stays false: a single exclusive Prune must complete retention for operators.
	return maintenance.SafetyParameters{
		BlobDeleteMinAge:                 minAge,
		SessionExpirationAge:             minAge,
		MinContentAgeSubjectToGC:         minAge,
		DropContentFromIndexExtraMargin:  time.Hour,
		MarginBetweenSnapshotGC:          0,
		RewriteMinAge:                    min(minAge, 2*time.Hour),
		RequireTwoGCCycles:               false,
		DisableEventualConsistencySafety: true, // local filesystem
		MinRewriteToOrphanDeletionDelay:  time.Hour,
	}
}

// markLiveContents walks every live Breakwater snapshot and collects content IDs
// reachable via recursive tree/image walk (R2-1). Unknown kinds fail closed (R2-3).
func markLiveContents(ctx context.Context, rep repo.Repository) (map[content.ID]struct{}, error) {
	live := make(map[content.ID]struct{})

	// Enumerate all manifests; filter to those with a RootObjectID.
	// FindManifests with empty labels returns everything.
	entries, err := rep.FindManifests(ctx, map[string]string{})
	if err != nil {
		return nil, err
	}

	for _, e := range entries {
		t := e.Labels["type"]
		// Skip non-snapshot manifests (kopia internal, etc.).
		if t == "" {
			continue
		}
		// Only consider Breakwater snapshot label namespace (bw-*).
		if len(t) < 3 || t[:3] != "bw-" {
			continue
		}

		var rec SnapshotRecord
		if _, err := rep.GetManifest(ctx, e.ID, &rec); err != nil {
			return nil, fmt.Errorf("get manifest %s: %w", e.ID, err)
		}
		if rec.RootObjectID == "" {
			continue
		}

		// R2-3: fail closed on kinds we cannot walk.
		if !ValidSnapshotKind(SnapshotKind(t)) && !ValidSnapshotKind(rec.Kind) {
			kind := t
			if rec.Kind != "" {
				kind = string(rec.Kind)
			}
			return nil, fmt.Errorf("prune: unknown snapshot kind %q on manifest %s (fail closed)", kind, e.ID)
		}
		kind := rec.Kind
		if kind == "" {
			kind = SnapshotKind(t)
		}

		if err := markSnapshotContents(ctx, rep, live, kind, rec.RootObjectID, e.ID); err != nil {
			return nil, err
		}
	}
	return live, nil
}

// markSnapshotContents marks all content IDs reachable from a snapshot root,
// keyed by kind (R2-1 recursive mark).
func markSnapshotContents(ctx context.Context, rep repo.Repository, live map[content.ID]struct{}, kind SnapshotKind, root ObjectID, manifestID manifest.ID) error {
	switch kind {
	case KindFileSnapshot:
		return markTreeObject(ctx, rep, live, root, string(manifestID), 0)
	case KindImageSnapshot:
		return markImageManifest(ctx, rep, live, root, string(manifestID))
	default:
		return fmt.Errorf("prune: cannot walk snapshot kind %q (manifest %s)", kind, manifestID)
	}
}

const maxTreeDepth = 256

// markTreeObject recursively marks a TreeObject and all referenced file/dir/ADS objects.
// Decode failure ALWAYS fails the prune (R3-1 fail closed — no leaf heuristic).
func markTreeObject(ctx context.Context, rep repo.Repository, live map[content.ID]struct{}, oid ObjectID, manifestID string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("prune: tree depth exceeds %d (manifest %s, oid %s)", maxTreeDepth, manifestID, oid)
	}
	if err := markObjectContents(ctx, rep, live, oid, manifestID); err != nil {
		return err
	}

	raw, err := readObjectBytes(ctx, rep, oid)
	if err != nil {
		return fmt.Errorf("prune: read tree object %s (manifest %s): %w", oid, manifestID, err)
	}
	var tree format.TreeObject
	if err := json.Unmarshal(raw, &tree); err != nil {
		return fmt.Errorf("prune: decode TreeObject %s (manifest %s): %w", oid, manifestID, err)
	}

	for _, ent := range tree.Entries {
		if ent.ObjectID == "" {
			continue
		}
		child := ObjectID(ent.ObjectID)
		switch ent.Type {
		case format.EntryDir:
			if err := markTreeObject(ctx, rep, live, child, manifestID, depth+1); err != nil {
				return err
			}
		default:
			// file, symlink, reparse, or empty type: mark object contents.
			if err := markObjectContents(ctx, rep, live, child, manifestID); err != nil {
				return err
			}
		}
		for _, ads := range ent.ADS {
			if ads.ObjectID == "" {
				continue
			}
			if err := markObjectContents(ctx, rep, live, ObjectID(ads.ObjectID), manifestID); err != nil {
				return err
			}
		}
	}
	return nil
}

// markImageManifest marks the image manifest object and every block content ID.
func markImageManifest(ctx context.Context, rep repo.Repository, live map[content.ID]struct{}, oid ObjectID, manifestID string) error {
	if err := markObjectContents(ctx, rep, live, oid, manifestID); err != nil {
		return err
	}
	raw, err := readObjectBytes(ctx, rep, oid)
	if err != nil {
		return fmt.Errorf("prune: read image manifest object %s (manifest %s): %w", oid, manifestID, err)
	}
	var img format.ImageManifest
	if err := json.Unmarshal(raw, &img); err != nil {
		return fmt.Errorf("prune: decode ImageManifest %s (manifest %s): %w", oid, manifestID, err)
	}
	for i, blk := range img.Blocks {
		if blk.ContentID == "" {
			continue
		}
		cid, err := content.ParseID(blk.ContentID)
		if err != nil {
			return fmt.Errorf("prune: image block %d content id %q (manifest %s): %w", i, blk.ContentID, manifestID, err)
		}
		live[cid] = struct{}{}
	}
	return nil
}

func markObjectContents(ctx context.Context, rep repo.Repository, live map[content.ID]struct{}, oid ObjectID, manifestID string) error {
	parsed, err := object.ParseID(string(oid))
	if err != nil {
		return fmt.Errorf("prune: parse object id %q (manifest %s): %w", oid, manifestID, err)
	}
	ids, err := rep.VerifyObject(ctx, parsed)
	if err != nil {
		return fmt.Errorf("prune: VerifyObject %s (manifest %s): %w", oid, manifestID, err)
	}
	for _, id := range ids {
		live[id] = struct{}{}
	}
	return nil
}

// readObjectBytes reads an object with MaxMarkObjectBytes cap (R3-2).
// Over-limit is an explicit fail-closed error (never silently truncate).
func readObjectBytes(ctx context.Context, rep repo.Repository, oid ObjectID) ([]byte, error) {
	parsed, err := object.ParseID(string(oid))
	if err != nil {
		return nil, err
	}
	r, err := rep.OpenObject(ctx, parsed)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	// Read at most MaxMarkObjectBytes+1 to detect overflow without unbounded RAM.
	limited := io.LimitReader(r, int64(MaxMarkObjectBytes)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxMarkObjectBytes {
		return nil, fmt.Errorf("object %s exceeds mark-phase size limit %d bytes (fail closed)", oid, MaxMarkObjectBytes)
	}
	return data, nil
}

func (v *kopiaVault) Stats(ctx context.Context) (VaultStats, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return VaultStats{}, err
	}

	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return VaultStats{}, fmt.Errorf("stats require direct repository")
	}

	var stats VaultStats
	err := dr.ContentReader().IterateContents(ctx, content.IterateOptions{}, func(info content.Info) error {
		if info.Deleted {
			return nil
		}
		stats.ContentCount++
		stats.TotalSizeBytes += int64(info.PackedLength)
		if !info.ContentID.HasPrefix() {
			stats.UserContentCount++
			stats.UserSizeBytes += int64(info.PackedLength)
		}
		return nil
	})
	return stats, err
}

// MarshalSnapshotJSON is a helper for tests/debug.
func MarshalSnapshotJSON(rec SnapshotRecord) ([]byte, error) {
	return json.Marshal(rec)
}
