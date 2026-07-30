// Package vault is the only package allowed to import kopia's repo/content/
// object/manifest/maintenance layers.
//
// M2 stage-3 carve-out (PLAN): pkg/contentid may import pure-Go repo/hashing +
// repo/splitter so agents compute bit-identical content IDs. Nothing else in
// pkg/agent/cli may import kopia.
//
// See PLAN.md → Storage engine. Importing pkg/format is allowed (shared module).
package vault

import (
	"context"
	"io"
	"sync"
	"time"
)

// ContentID is an opaque content-addressed identifier (kopia content ID string).
type ContentID string

// ObjectID is an opaque object identifier (kopia object ID string).
// File contents and tree JSON blobs are stored as objects.
type ObjectID string

// SnapshotRecordID is a manifest ID for a Breakwater snapshot record.
type SnapshotRecordID string

// SnapshotKind identifies the type of snapshot stored in a vault.
type SnapshotKind string

const (
	// KindFileSnapshot is a file-tree snapshot (DIDX-analog).
	KindFileSnapshot SnapshotKind = "bw-file-snapshot"
	// KindImageSnapshot is a fixed-block volume/VHDX snapshot (FIDX-analog; later phases).
	KindImageSnapshot SnapshotKind = "bw-image-snapshot"
)

// ValidSnapshotKind reports whether kind is a known Breakwater snapshot kind.
func ValidSnapshotKind(kind SnapshotKind) bool {
	switch kind {
	case KindFileSnapshot, KindImageSnapshot:
		return true
	default:
		return false
	}
}

// SnapshotRecord is Breakwater-native snapshot metadata stored via kopia manifests.
// Authoritative copy lives in the repo; catalog mirrors it as a rebuildable index.
type SnapshotRecord struct {
	// Kind is required and becomes the manifest "type" label.
	Kind SnapshotKind `json:"kind"`
	// MachineID is the Breakwater machine ULID.
	MachineID string `json:"machine_id"`
	// Timestamp is when the snapshot was taken (agent clock; server may override).
	Timestamp time.Time `json:"timestamp"`
	// RootObjectID is the root tree object (file) or image manifest object.
	RootObjectID ObjectID `json:"root_object_id"`
	// Source describes the backup source (e.g. path, volume, VM name).
	Source string `json:"source,omitempty"`
	// JobID links to the catalog job that produced this snapshot.
	JobID string `json:"job_id,omitempty"`
	// Extra is optional opaque metadata (JSON-serializable).
	Extra map[string]string `json:"extra,omitempty"`
}

// SnapshotMeta is listing metadata for a stored snapshot record.
type SnapshotMeta struct {
	ID        SnapshotRecordID
	Kind      SnapshotKind
	MachineID string
	Timestamp time.Time
	ModTime   time.Time
}

// Splitter names mirror kopia's public splitter IDs.
const (
	// SplitterDynamic is CDC for file contents (DYNAMIC-4M-BUZHASH).
	SplitterDynamic = "DYNAMIC-4M-BUZHASH"
	// SplitterFixed4M is fixed 4MiB blocks for volume/VHDX images.
	SplitterFixed4M = "FIXED-4M"
)

// DefaultPruneMinContentAge is the default sweep safety window: contents younger
// than this are never deleted. Protects in-flight multi-RPC backups whose
// contents are not yet referenced by a committed snapshot record (R2-2).
// Matches kopia's safety philosophy that SafetyNone otherwise opts out of.
const DefaultPruneMinContentAge = 24 * time.Hour

// pruneOptions holds resolved Prune settings.
type pruneOptions struct {
	minContentAge time.Duration
	// minAgeSet is true when the caller explicitly set min age (including zero).
	minAgeSet bool
}

// PruneOption configures Prune.
type PruneOption func(*pruneOptions)

// WithMinContentAge sets the minimum age of content eligible for deletion.
// Contents younger than this are retained (in-flight backup protection).
// Pass 0 to disable the age guard — required by reclamation tests and the
// engine gate so young test data can be observed reclaiming. Production and
// the zero-option Prune(ctx) default to DefaultPruneMinContentAge (24h).
func WithMinContentAge(d time.Duration) PruneOption {
	return func(o *pruneOptions) {
		o.minContentAge = d
		o.minAgeSet = true
	}
}

func resolvePruneOptions(opts []PruneOption) pruneOptions {
	o := pruneOptions{}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	if !o.minAgeSet {
		o.minContentAge = DefaultPruneMinContentAge
	}
	return o
}

// Vault is the per-machine content-addressed repository interface.
// Implementations MUST confine all kopia usage behind this boundary.
//
// Concurrency: backup/replication share a read lock; prune/verify take exclusive.
//
// Serialization contract (R2-2 / M2):
//   - Prune must never run concurrently with an open backup session on the same repo.
//     An in-flight backup uploads contents across many PutContent/WriteObject calls
//     and only later commits a snapshot record; without structural serialization those
//     unreferenced young contents would be sweep candidates. DefaultPruneMinContentAge
//     is a safety net; M2's scheduler must enforce per-repo backup-vs-prune
//     serialization so the age guard is defense-in-depth, not the only line.
//   - OpenObject readers must complete before Prune on the same vault (scheduler
//     serializes jobs per-repo in M2+; see REVIEW-M1 M2).
//
// Min-age guard coverage (R3-5):
//   - Covers: our DeleteContent sweep skips contents younger than MinContentAge;
//     when MinContentAge > 0, kopia maintenance also uses SafetyParameters with
//     BlobDeleteMinAge / SessionExpirationAge ≥ that window (not SafetyNone).
//   - Does not cover: concurrent second live handle on the same repo directory
//     (see Manager docs — one live handle per repo; Close must not race Open);
//     structural enforcement is M2 scheduler work.
type Vault interface {
	// Close releases repository resources. Subsequent method calls return an error.
	Close(ctx context.Context) error

	// HashingKey returns the repo's content-ID HMAC secret and algorithm name
	// (e.g. BLAKE2B-256-128). Never returns encryption keys or the master key.
	HashingKey(ctx context.Context) (secret []byte, algorithm string, err error)

	// PutContent stores raw bytes as a single content-addressed blob (max
	// MaxPutContentBytes = 8 MiB, DYNAMIC-4M max segment) and returns its
	// content ID. Larger data must use WriteObject.
	PutContent(ctx context.Context, data []byte) (ContentID, error)

	// ObjectFromContents builds an OpenObject-able ObjectID from content IDs
	// already stored via PutContent. One ID → direct object; multiple →
	// concatenated indirect object (no payload re-upload).
	ObjectFromContents(ctx context.Context, ids []ContentID) (ObjectID, error)

	// HasContents reports which of the given content IDs already exist (have/want).
	// The returned slice is parallel to ids: true if present.
	HasContents(ctx context.Context, ids []ContentID) ([]bool, error)

	// GetContent returns the plaintext bytes for a content ID.
	GetContent(ctx context.Context, id ContentID) ([]byte, error)

	// WriteObject streams data into a chunked object using the named splitter
	// (SplitterDynamic or SplitterFixed4M). Returns the object ID.
	WriteObject(ctx context.Context, splitter string, r io.Reader) (ObjectID, error)

	// OpenObject returns a reader for an object.
	OpenObject(ctx context.Context, id ObjectID) (io.ReadCloser, error)

	// VerifyObject checks object integrity and returns backing content IDs.
	VerifyObject(ctx context.Context, id ObjectID) ([]ContentID, error)

	// PutSnapshotRecord stores a Breakwater snapshot manifest (labels + JSON payload).
	// Rejects unknown kinds, unparseable RootObjectID, and roots that do not decode
	// as the kind's format (TreeObject for file, ImageManifest for image) (R2-3/R2-4/R3-1).
	PutSnapshotRecord(ctx context.Context, rec SnapshotRecord) (SnapshotRecordID, error)

	// GetSnapshotRecord loads a snapshot record by ID.
	GetSnapshotRecord(ctx context.Context, id SnapshotRecordID) (*SnapshotRecord, error)

	// ListSnapshotRecords lists snapshot manifests, optionally filtered by kind.
	ListSnapshotRecords(ctx context.Context, kind SnapshotKind) ([]SnapshotMeta, error)

	// DeleteSnapshotRecord removes a snapshot manifest (forget). Does not free space;
	// call Prune to reclaim unreferenced content.
	DeleteSnapshotRecord(ctx context.Context, id SnapshotRecordID) error

	// Prune runs mark-and-sweep GC. Default min-content-age is 24h; pass
	// WithMinContentAge(0) only in tests that must observe reclamation of young data.
	// Must not run concurrently with an open backup session (see interface comment).
	Prune(ctx context.Context, opts ...PruneOption) error

	// Stats returns rough repository size information.
	Stats(ctx context.Context) (VaultStats, error)
}

// VaultStats is a snapshot of repository usage.
type VaultStats struct {
	// ContentCount is non-deleted contents (including system/manifest-prefixed).
	ContentCount int64
	// TotalSizeBytes is summed packed lengths of non-deleted contents.
	TotalSizeBytes int64
	// UserContentCount is non-deleted unprefixed contents (backup payloads).
	// Use this for reclamation assertions — maintenance may add manifest blobs.
	UserContentCount int64
	// UserSizeBytes is packed size of unprefixed non-deleted contents.
	UserSizeBytes int64
}

// Manager owns per-machine vaults under a repos root directory.
//
// Config and cache live under dataDir (M4):
//
//	<dataDir>/kopia-config/<repoID>.config
//	<dataDir>/cache/<repoID>/
//
// Repository blob storage remains under reposDir/<repoID>/.
//
// Invariant (R3-6): at most one live handle per repoID. Close(repoID) must not
// race Open/Create for the same ID — Close releases the manager lock before
// waiting on the vault exclusive lock, so a concurrent Open can create a second
// live handle while the first still has in-flight writes.
//
// Structural enforcement (M2 stage 2): callers must hold an exclusive lease from
// scheduler.RepoLocks for the repoID around Manager.Close and any Open that could
// race Close (scheduler.RepoLocks.WithExclusive). Job-scoped shared leases cover
// backup/restore; exclusive covers prune/verify. Enrollment Create is exempt:
// the repo ID is brand-new and no job can hold a lease yet.
type Manager struct {
	reposDir string
	dataDir  string
	mu       sync.Mutex
	open     map[string]*kopiaVault // keyed by machine ULID / repo ID
}

// NewManager creates a vault manager.
// reposDir is typically /repos (blob storage); dataDir is typically /data
// (kopia config + cache). When dataDir is empty, it defaults to reposDir
// (tests may pass the same temp root for both).
func NewManager(reposDir, dataDir string) *Manager {
	if dataDir == "" {
		dataDir = reposDir
	}
	return &Manager{
		reposDir: reposDir,
		dataDir:  dataDir,
		open:     make(map[string]*kopiaVault),
	}
}

// isClosed reports whether the vault has been closed (rep nilled).
func (v *kopiaVault) isClosed() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.rep == nil
}

// Open returns an existing open vault or opens the repo at reposDir/<repoID>.
// If a cached instance was closed, it is evicted and re-opened (R2-10).
func (m *Manager) Open(ctx context.Context, repoID, password string) (Vault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.open[repoID]; ok {
		if !v.isClosed() {
			return v, nil
		}
		delete(m.open, repoID)
	}
	v, err := openKopiaVault(ctx, m.reposDir, m.dataDir, repoID, password)
	if err != nil {
		return nil, err
	}
	m.open[repoID] = v
	return v, nil
}

// Create initializes a new per-machine repository and opens it.
// If a cached instance was closed, it is evicted (R2-10).
func (m *Manager) Create(ctx context.Context, repoID, password string) (Vault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.open[repoID]; ok {
		if !v.isClosed() {
			return v, nil
		}
		delete(m.open, repoID)
	}
	v, err := createKopiaVault(ctx, m.reposDir, m.dataDir, repoID, password)
	if err != nil {
		return nil, err
	}
	m.open[repoID] = v
	return v, nil
}

// Close removes a vault from the cache and closes it (R2-10).
func (m *Manager) Close(ctx context.Context, repoID string) error {
	m.mu.Lock()
	v, ok := m.open[repoID]
	if ok {
		delete(m.open, repoID)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return v.Close(ctx)
}

// CloseAll closes every open vault.
func (m *Manager) CloseAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var first error
	for id, v := range m.open {
		if err := v.Close(ctx); err != nil && first == nil {
			first = err
		}
		delete(m.open, id)
	}
	return first
}
