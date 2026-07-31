package vault

import (
	"bytes"
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

	"github.com/ajthom90/breakwater/pkg/contentid"
	"github.com/ajthom90/breakwater/pkg/format"
)

// MaxPutContentBytes is the maximum payload for PutContent.
// Aligned with DYNAMIC-4M-BUZHASH MaxSegmentSize (8 MiB) so agent-side CDC chunks
// can be uploaded via PutContents. Image fixed blocks remain 4 MiB (FIXED-4M /
// ImageBlockSizeBytes on the data plane).
//
// H2 amendment (M2-S3 / S3-F4): originally 4 MiB (one FIXED-4M block). Raised to
// 8 MiB so DYNAMIC-4M max segments fit; image path enforces 4 MiB separately.
const MaxPutContentBytes = 8 << 20 // 8 MiB

// SplitterFixed8M is a single-segment fixed splitter for PutContent payloads
// up to MaxPutContentBytes (S3-F4: named constant, not a bare string).
const SplitterFixed8M = "FIXED-8M"

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

// repoPaths returns blob storage, config, and cache locations (M4).
// Config: <dataDir>/kopia-config/<repoID>.config
// Cache:  <dataDir>/cache/<repoID>
// Blobs:  <reposDir>/<repoID>
func repoPaths(reposDir, dataDir, repoID string) (repoPath, cfgPath, cacheDir string) {
	repoPath = filepath.Join(reposDir, repoID)
	cfgPath = filepath.Join(dataDir, "kopia-config", repoID+".config")
	cacheDir = filepath.Join(dataDir, "cache", repoID)
	return repoPath, cfgPath, cacheDir
}

// legacyRepoPaths is the M1 layout (config/cache inside the repo directory).
func legacyRepoPaths(repoPath string) (cfgPath, cacheDir string) {
	return filepath.Join(repoPath, "breakwater.config"), filepath.Join(repoPath, ".cache")
}

// migrateLegacyLayout re-Connects an M1-era repo (config/cache under the repo path)
// into the M4 layout under dataDir. Re-Connect is used (not a blind rename) so the
// new config records the new cache directory path correctly.
// Returns true if a legacy layout was migrated.
func migrateLegacyLayout(ctx context.Context, repoPath, cfgPath, cacheDir, password, repoID string) (bool, error) {
	if _, err := os.Stat(cfgPath); err == nil {
		return false, nil // already on new layout
	}
	legacyCfg, legacyCache := legacyRepoPaths(repoPath)
	if _, err := os.Stat(legacyCfg); err != nil {
		return false, nil // no legacy config either
	}
	if err := ensureConnected(ctx, repoPath, cfgPath, cacheDir, password, repoID); err != nil {
		return false, fmt.Errorf("migrate legacy layout: %w", err)
	}
	_ = os.Remove(legacyCfg)
	// Drop stale M1 cache directory; new cache lives under dataDir.
	_ = os.RemoveAll(legacyCache)
	return true, nil
}

func createKopiaVault(ctx context.Context, reposDir, dataDir, repoID, password string) (*kopiaVault, error) {
	repoPath, cfgPath, cacheDir := repoPaths(reposDir, dataDir, repoID)
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir repo: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir kopia-config: %w", err)
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

	return openKopiaVault(ctx, reposDir, dataDir, repoID, password)
}

func openKopiaVault(ctx context.Context, reposDir, dataDir, repoID, password string) (*kopiaVault, error) {
	repoPath, cfgPath, cacheDir := repoPaths(reposDir, dataDir, repoID)
	if _, err := migrateLegacyLayout(ctx, repoPath, cfgPath, cacheDir, password, repoID); err != nil {
		return nil, err
	}
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
			// Single-content object via FIXED-8M: payload ≤ MaxPutContentBytes yields one content.
			// (kopia's WriteContent takes internal gather.Bytes; object path is the public API.)
			// No object-layer compressor: content ID must match agent plaintext keyed hash.
			ow := w.NewObjectWriter(ctx, object.WriterOptions{
				Splitter: SplitterFixed8M,
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

// ComputeContentID returns the content ID PutContent would produce without writing (S3-F3).
func (v *kopiaVault) ComputeContentID(ctx context.Context, data []byte) (ContentID, error) {
	if len(data) > MaxPutContentBytes {
		return "", fmt.Errorf("ComputeContentID: payload %d bytes exceeds max %d", len(data), MaxPutContentBytes)
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return "", err
	}
	secret, algo, err := v.hashingParamsLocked()
	if err != nil {
		return "", err
	}
	id, err := computeContentIDString(algo, secret, data)
	if err != nil {
		return "", err
	}
	return ContentID(id), nil
}

// hashingParamsLocked returns hashing secret+algo; caller must hold v.mu.
func (v *kopiaVault) hashingParamsLocked() (secret []byte, algorithm string, err error) {
	if err := v.requireOpen(); err != nil {
		return nil, "", err
	}
	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return nil, "", fmt.Errorf("hashing key requires direct repository")
	}
	cf := dr.ContentReader().ContentFormat()
	sec := cf.GetHmacSecret()
	out := make([]byte, len(sec))
	copy(out, sec)
	return out, cf.GetHashFunction(), nil
}

// computeContentIDString uses pkg/contentid so server re-computation matches
// the agent have/want IDs bit-for-bit (S3-F3 / R2-14).
func computeContentIDString(algo string, secret, payload []byte) (string, error) {
	h, err := contentid.New(algo, secret)
	if err != nil {
		return "", err
	}
	return h.ContentID(payload)
}

// ObjectFromContents builds a readable object from content IDs already stored
// via PutContent. One ID → that content as a direct object; multiple → kopia
// ConcatenateObjects (indirect object). Used by the agent after have/want
// PutContents so multi-chunk files get an OpenObject-able ObjectID without
// re-uploading payload (M2 stage 3).
func (v *kopiaVault) ObjectFromContents(ctx context.Context, ids []ContentID) (ObjectID, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("ObjectFromContents: no content IDs")
	}
	if len(ids) == 1 {
		// Direct object ID == content ID string.
		if _, err := object.ParseID(string(ids[0])); err != nil {
			return "", fmt.Errorf("ObjectFromContents: invalid content id %q: %w", ids[0], err)
		}
		// Verify present.
		present, err := v.HasContents(ctx, ids)
		if err != nil {
			return "", err
		}
		if len(present) != 1 || !present[0] {
			return "", fmt.Errorf("ObjectFromContents: content %s not present", ids[0])
		}
		return ObjectID(ids[0]), nil
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return "", err
	}

	oids := make([]object.ID, len(ids))
	for i, id := range ids {
		oid, err := object.ParseID(string(id))
		if err != nil {
			return "", fmt.Errorf("ObjectFromContents: parse %q: %w", id, err)
		}
		oids[i] = oid
	}

	var result ObjectID
	err := repo.WriteSession(ctx, v.rep, repo.WriteSessionOptions{Purpose: "object-from-contents"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			type concatenator interface {
				ConcatenateObjects(ctx context.Context, objectIDs []object.ID, opt repo.ConcatenateOptions) (object.ID, error)
			}
			cw, ok := w.(concatenator)
			if !ok {
				return fmt.Errorf("ObjectFromContents: repository writer does not support ConcatenateObjects")
			}
			oid, err := cw.ConcatenateObjects(ctx, oids, repo.ConcatenateOptions{})
			if err != nil {
				return err
			}
			result = ObjectID(oid.String())
			return nil
		})
	return result, err
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
	// NOTE (S5-F1): kopia object.VerifyObject returns IDs from a map tracker —
	// order is non-deterministic and must not be used for sequence identity.
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

// ObjectDataContentIDs returns data content IDs in stream order by walking the
// kopia indirect index (or a single direct content for small objects).
//
// S5-F1: VerifyObject cannot provide this order — it iterates a map. Agent-side
// ChunkAndID and server-side WriteObject produce the same sequence; this method
// is how tests and future re-chunk-sensitive paths observe it.
func (v *kopiaVault) ObjectDataContentIDs(ctx context.Context, id ObjectID) ([]ContentID, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if err := v.requireOpen(); err != nil {
		return nil, err
	}

	oid, err := object.ParseID(string(id))
	if err != nil {
		return nil, fmt.Errorf("parse object id: %w", err)
	}

	// Direct single-content object (common for small payloads / FIXED-8M PutContent).
	if cid, _, ok := oid.ContentID(); ok {
		return []ContentID{ContentID(cid.String())}, nil
	}

	indexOID, ok := oid.IndexObjectID()
	if !ok {
		return nil, fmt.Errorf("object %s: unrecognized object type", id)
	}

	dr, ok := v.rep.(repo.DirectRepository)
	if !ok {
		return nil, fmt.Errorf("object content IDs: repository is not DirectRepository")
	}

	// Walk seek table in stream order. Each entry's Object is a direct content
	// (or nested indirect — recurse for completeness).
	return collectDataContentIDsInOrder(ctx, directContentReader{dr: dr}, indexOID)
}

// directContentReader adapts DirectRepository to the contentReader surface
// expected by object.LoadIndexObject (GetContent/ContentInfo/PrefetchContents).
// ContentManager is only on DirectRepositoryWriter; ContentReader + PrefetchContents
// on DirectRepository/Repository are sufficient for index walks.
type directContentReader struct {
	dr repo.DirectRepository
}

func (r directContentReader) GetContent(ctx context.Context, id content.ID) ([]byte, error) {
	return r.dr.ContentReader().GetContent(ctx, id)
}

func (r directContentReader) ContentInfo(ctx context.Context, id content.ID) (content.Info, error) {
	return r.dr.ContentReader().ContentInfo(ctx, id)
}

func (r directContentReader) PrefetchContents(ctx context.Context, ids []content.ID, hint string) []content.ID {
	return r.dr.PrefetchContents(ctx, ids, hint)
}

// collectDataContentIDsInOrder walks an indirect index object and returns leaf
// data content IDs in stream order. Nested indirection is expanded; the index
// content itself is not included (only data leaves).
func collectDataContentIDsInOrder(ctx context.Context, cr directContentReader, indexOID object.ID) ([]ContentID, error) {
	entries, err := object.LoadIndexObject(ctx, cr, indexOID)
	if err != nil {
		return nil, fmt.Errorf("LoadIndexObject: %w", err)
	}
	var out []ContentID
	for _, e := range entries {
		ids, err := leafDataContentIDs(ctx, cr, e.Object)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	return out, nil
}

func leafDataContentIDs(ctx context.Context, cr directContentReader, oid object.ID) ([]ContentID, error) {
	if cid, _, ok := oid.ContentID(); ok {
		return []ContentID{ContentID(cid.String())}, nil
	}
	if indexOID, ok := oid.IndexObjectID(); ok {
		return collectDataContentIDsInOrder(ctx, cr, indexOID)
	}
	return nil, fmt.Errorf("unrecognized object ID in index: %v", oid)
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
// Uses strict JSON (DisallowUnknownFields) so a mislabeled kind — e.g. TreeObject
// stored under bw-image-snapshot — is rejected at the write boundary (M2 / R3 note 1).
func validateSnapshotRoot(ctx context.Context, rep repo.Repository, kind SnapshotKind, root ObjectID) error {
	raw, err := readObjectBytes(ctx, rep, root)
	if err != nil {
		return fmt.Errorf("read snapshot root %s: %w", root, err)
	}
	switch kind {
	case KindFileSnapshot:
		var tree format.TreeObject
		if err := strictJSONDecode(raw, &tree); err != nil {
			return fmt.Errorf("file snapshot root %s must be a TreeObject JSON: %w", root, err)
		}
	case KindImageSnapshot:
		var img format.ImageManifest
		if err := strictJSONDecode(raw, &img); err != nil {
			return fmt.Errorf("image snapshot root %s must be an ImageManifest JSON: %w", root, err)
		}
	}
	return nil
}

// strictJSONDecode decodes with DisallowUnknownFields so cross-kind roots fail,
// and requires EOF after the first JSON value so trailing garbage is rejected
// (S1-F3 — Decoder.Decode alone is weaker than json.Unmarshal on this axis).
// Shared by write-boundary validation and the mark phase (same contract).
func strictJSONDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// Second Decode must hit EOF — any further token is trailing garbage.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing data after JSON value")
		}
		return fmt.Errorf("trailing data after JSON value: %w", err)
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
		return markTreeObject(ctx, rep, live, root, string(manifestID), 0, "")
	case KindImageSnapshot:
		return markImageManifest(ctx, rep, live, root, string(manifestID))
	default:
		return fmt.Errorf("prune: cannot walk snapshot kind %q (manifest %s)", kind, manifestID)
	}
}

// markTreeObject recursively marks a TreeObject and all referenced file/dir/ADS objects.
// Decode failure ALWAYS fails the prune (R3-1 fail closed — no leaf heuristic).
//
// Depth is bounded by format.MaxTreeDepth (shared with restore reachability — M4-F1).
// Over-limit fails the prune with an actionable error naming the manifest and path
// prefix so operators can forget the snapshot rather than discover silent retention stop.
func markTreeObject(ctx context.Context, rep repo.Repository, live map[content.ID]struct{}, oid ObjectID, manifestID string, depth int, pathPrefix string) error {
	if depth > format.MaxTreeDepth {
		return treeDepthExceededError("prune", manifestID, pathPrefix, string(oid))
	}
	if err := markObjectContents(ctx, rep, live, oid, manifestID); err != nil {
		return err
	}

	raw, err := readObjectBytes(ctx, rep, oid)
	if err != nil {
		return fmt.Errorf("prune: read tree object %s (manifest %s): %w", oid, manifestID, err)
	}
	var tree format.TreeObject
	// Same strictness as validateSnapshotRoot (write boundary) — fail closed on
	// unknown fields so a mislabeled kind cannot partial-decode and under-mark.
	if err := strictJSONDecode(raw, &tree); err != nil {
		return fmt.Errorf("prune: decode TreeObject %s (manifest %s): %w", oid, manifestID, err)
	}

	for _, ent := range tree.Entries {
		if ent.ObjectID == "" {
			continue
		}
		child := ObjectID(ent.ObjectID)
		childPath := joinTreePath(pathPrefix, ent.Name)
		switch ent.Type {
		case format.EntryDir:
			if err := markTreeObject(ctx, rep, live, child, manifestID, depth+1, childPath); err != nil {
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

// treeDepthExceededError is the shared actionable wording for prune/restore walks.
func treeDepthExceededError(op, manifestID, pathPrefix, oid string) error {
	return fmt.Errorf("%s: tree depth exceeds %d (runaway guard); manifest=%s path=%q oid=%s — forget this snapshot or flatten the source tree; retention/restore cannot proceed while this tree is live",
		op, format.MaxTreeDepth, manifestID, pathPrefix, oid)
}

func joinTreePath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	if name == "" {
		return prefix
	}
	return prefix + "/" + name
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
	// Same strictness as validateSnapshotRoot (write boundary).
	if err := strictJSONDecode(raw, &img); err != nil {
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
