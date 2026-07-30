// Package format defines Breakwater-native snapshot and tree object formats.
// Shared by agent, server, restore, and bwctl. Authoritative storage is the
// vault (kopia manifests + objects); catalog mirrors for query.
package format

import "time"

// FormatVersion is the on-disk/object schema version for Breakwater-native
// structures. Repo format (kopia) is separate; this versions our JSON trees
// and image manifests so fixed-block image objects can be added later.
const FormatVersion = 1

// TreeObject is a directory tree (DIDX-analog). Stored as a JSON object in the vault.
type TreeObject struct {
	Version int         `json:"v"`
	Entries []TreeEntry `json:"entries"`
}

// TreeEntry is one name within a directory.
type TreeEntry struct {
	Name string    `json:"name"`
	Type EntryType `json:"type"` // file|dir|symlink|reparse
	Size int64     `json:"size,omitempty"`
	// Timestamps as Unix nanoseconds (UTC).
	MtimeNS int64 `json:"mtime_ns,omitempty"`
	CtimeNS int64 `json:"ctime_ns,omitempty"`
	AtimeNS int64 `json:"atime_ns,omitempty"`
	// Windows attributes (FILE_ATTRIBUTE_*).
	Attrs uint32 `json:"attrs,omitempty"`
	// SecurityDescriptor is the raw NTFS SD (binary, base64 in JSON via custom codecs later;
	// for now store as base64 string).
	SecurityDescriptor string `json:"sd,omitempty"`
	// ObjectID for file content or child directory tree.
	ObjectID string `json:"oid,omitempty"`
	// ADS: each alternate data stream is its own object.
	ADS []ADSEntry `json:"ads,omitempty"`
	// Reparse/symlink target data.
	ReparseData string `json:"reparse,omitempty"`
}

// EntryType classifies a tree entry.
type EntryType string

const (
	EntryFile    EntryType = "file"
	EntryDir     EntryType = "dir"
	EntrySymlink EntryType = "symlink"
	EntryReparse EntryType = "reparse"
)

// ADSEntry is an NTFS alternate data stream.
type ADSEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	ObjectID string `json:"oid"`
}

// ImageManifest is a fixed-block volume/VHDX snapshot (FIDX-analog).
// 4MiB aligned blocks; all-zero → well-known zero content ID.
// Reserved for Phase 3/4; schema frozen for format versioning.
type ImageManifest struct {
	Version   int             `json:"v"`
	BlockSize int             `json:"block_size"` // bytes; default 4MiB
	Size      int64           `json:"size"`       // total image bytes
	Blocks    []ImageBlockRef `json:"blocks"`
}

// ImageBlockRef is one fixed block in an image.
type ImageBlockRef struct {
	ContentID string `json:"cid"`
	XXH64     uint64 `json:"xxh64"`
}

// SnapshotMeta is shared snapshot metadata (catalog + API).
type SnapshotMeta struct {
	ID           string    `json:"id"`
	MachineID    string    `json:"machine_id"`
	Kind         string    `json:"kind"` // file|image|hyperv
	Source       string    `json:"source"`
	RootObjectID string    `json:"root_object_id"`
	ManifestRef  string    `json:"manifest_ref,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	BytesRead    int64     `json:"bytes_read,omitempty"`
	BytesStored  int64     `json:"bytes_stored,omitempty"`
	JobID        string    `json:"job_id,omitempty"`
	VerifyState  string    `json:"verify_state,omitempty"`
}
