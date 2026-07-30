// Package vault is the ONLY package allowed to import kopia.
// It exposes a narrow Breakwater-facing interface over kopia's repo layers
// (repo, repo/content, repo/object, repo/manifest, repo/maintenance).
//
// See PLAN.md → Storage engine. All kopia imports stay confined here.
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

// Vault is the per-machine content-addressed repository interface.
// Implementations MUST confine all kopia usage behind this boundary.
//
// Concurrency: backup/replication share a read lock; prune/verify take exclusive.
type Vault interface {
	// Close releases repository resources.
	Close(ctx context.Context) error

	// PutContent stores raw bytes as a content-addressed blob and returns its ID.
	// Server re-computes the ID from data (integrity check for agent uploads).
	PutContent(ctx context.Context, data []byte) (ContentID, error)

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
	PutSnapshotRecord(ctx context.Context, rec SnapshotRecord) (SnapshotRecordID, error)

	// GetSnapshotRecord loads a snapshot record by ID.
	GetSnapshotRecord(ctx context.Context, id SnapshotRecordID) (*SnapshotRecord, error)

	// ListSnapshotRecords lists snapshot manifests, optionally filtered by kind.
	ListSnapshotRecords(ctx context.Context, kind SnapshotKind) ([]SnapshotMeta, error)

	// DeleteSnapshotRecord removes a snapshot manifest (forget). Does not free space;
	// call Prune to reclaim unreferenced content.
	DeleteSnapshotRecord(ctx context.Context, id SnapshotRecordID) error

	// Prune runs retention-aware GC: after forgets, delete unreferenced packs via
	// kopia maintenance. Server-only; never exposed on the agent port.
	Prune(ctx context.Context) error

	// Stats returns rough repository size information.
	Stats(ctx context.Context) (VaultStats, error)
}

// VaultStats is a snapshot of repository usage.
type VaultStats struct {
	// ContentCount is the number of content items (approx via iteration).
	ContentCount int64
	// TotalSizeBytes is summed packed lengths when available.
	TotalSizeBytes int64
}

// Manager owns per-machine vaults under a repos root directory.
type Manager struct {
	reposDir string
	mu       sync.Mutex
	open     map[string]*kopiaVault // keyed by machine ULID / repo ID
}

// NewManager creates a vault manager. reposDir is typically /repos.
func NewManager(reposDir string) *Manager {
	return &Manager{
		reposDir: reposDir,
		open:     make(map[string]*kopiaVault),
	}
}

// Open returns an existing open vault or opens the repo at reposDir/<repoID>.
func (m *Manager) Open(ctx context.Context, repoID, password string) (Vault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.open[repoID]; ok {
		return v, nil
	}
	v, err := openKopiaVault(ctx, m.reposDir, repoID, password)
	if err != nil {
		return nil, err
	}
	m.open[repoID] = v
	return v, nil
}

// Create initializes a new per-machine repository and opens it.
func (m *Manager) Create(ctx context.Context, repoID, password string) (Vault, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.open[repoID]; ok {
		return v, nil
	}
	v, err := createKopiaVault(ctx, m.reposDir, repoID, password)
	if err != nil {
		return nil, err
	}
	m.open[repoID] = v
	return v, nil
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
