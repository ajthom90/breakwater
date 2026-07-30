// Package state manages the agent state directory under ProgramData (or a test path).
//
// Layout (default C:\ProgramData\Breakwater\ on Windows):
//
//	identity.json   — machine id, server addr/FP, hashing key+algorithm
//	cert.pem        — enrolled client certificate
//	key.pem         — enrolled private key
//	completed.json  — recently completed job_ids (reconnect idempotency)
//	logs/           — agent log files
//
// Identity is written atomically (temp-then-rename). A half-written identity
// must never be loadable.
package state

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ajthom90/breakwater/agent/internal/identity"
)

// DefaultStateDir is the Windows production path. Overridable via --state-dir.
const DefaultStateDir = `C:\ProgramData\Breakwater`

// MaxCompletedJobs is the ring size for completed job_id records.
const MaxCompletedJobs = 1024

// Identity is the persisted enrolled agent configuration (no private key material
// except hashing key — encryption keys never leave the server).
type Identity struct {
	MachineID        string `json:"machine_id"`
	ServerAddr       string `json:"server_addr"`
	ServerFP         string `json:"server_fp"`
	HashingAlgorithm string `json:"hashing_algorithm"`
	HashingKeyB64    string `json:"hashing_key_b64"`
	EnrolledAt       string `json:"enrolled_at"` // RFC3339
	Hostname         string `json:"hostname,omitempty"`
}

// HashingKey decodes the base64 hashing key.
func (id *Identity) HashingKey() ([]byte, error) {
	return base64.StdEncoding.DecodeString(id.HashingKeyB64)
}

// Dir is a state directory handle.
type Dir struct {
	Path string

	mu        sync.Mutex
	completed *completedStore
}

// Open ensures the state directory exists and returns a handle.
// On Windows, SecureDir restricts ACLs to SYSTEM/Administrators
// (untested-on-Windows until first real CI/VM run).
func Open(path string) (*Dir, error) {
	if path == "" {
		path = DefaultStateDir
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(path, "logs"), 0o700); err != nil {
		return nil, err
	}
	if err := SecureDir(path); err != nil {
		// Log-level concern; do not fail open on non-Windows stubs.
		// Callers may inspect; we surface as soft failure via returned error only
		// when the platform claims support. SecureDir returns nil on non-Windows.
		return nil, fmt.Errorf("secure state dir: %w", err)
	}
	d := &Dir{Path: path}
	cs, err := loadCompleted(filepath.Join(path, "completed.json"))
	if err != nil {
		return nil, err
	}
	d.completed = cs
	return d, nil
}

// IdentityPath returns the path to identity.json.
func (d *Dir) IdentityPath() string { return filepath.Join(d.Path, "identity.json") }

// IsEnrolled reports whether a complete identity is loadable.
func (d *Dir) IsEnrolled() bool {
	_, _, err := d.LoadIdentity()
	return err == nil
}

// LoadIdentity loads identity.json + cert/key. Fails if any piece is missing
// or corrupt (half-written identity is not loadable).
func (d *Dir) LoadIdentity() (*Identity, *identity.Identity, error) {
	raw, err := os.ReadFile(d.IdentityPath())
	if err != nil {
		return nil, nil, err
	}
	var meta Identity
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, nil, fmt.Errorf("identity.json: %w", err)
	}
	if meta.MachineID == "" || meta.ServerAddr == "" || meta.ServerFP == "" ||
		meta.HashingAlgorithm == "" || meta.HashingKeyB64 == "" {
		return nil, nil, fmt.Errorf("identity.json: incomplete")
	}
	id, err := identity.Load(d.Path)
	if err != nil {
		return nil, nil, err
	}
	return &meta, id, nil
}

// SaveEnrolled persists enrollment response + cert/key atomically.
// Order: write cert+key first (pair), then identity.json last so LoadIdentity
// only succeeds when everything is present.
func (d *Dir) SaveEnrolled(meta *Identity, id *identity.Identity) error {
	if meta == nil || id == nil {
		return fmt.Errorf("nil identity")
	}
	if meta.EnrolledAt == "" {
		meta.EnrolledAt = time.Now().UTC().Format(time.RFC3339)
	}
	// Write cert/key pair.
	if err := identity.Save(d.Path, id); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	// identity.json last — LoadIdentity requires it + certs.
	if err := writeAtomic(d.IdentityPath(), raw, 0o600); err != nil {
		// Roll back certs so half-written state is not loadable.
		_ = os.Remove(filepath.Join(d.Path, "cert.pem"))
		_ = os.Remove(filepath.Join(d.Path, "key.pem"))
		return err
	}
	return nil
}

// HasCompleted reports whether jobID was already completed locally.
func (d *Dir) HasCompleted(jobID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.completed.has(jobID)
}

// MarkCompleted records a completed job_id for reconnect idempotency.
func (d *Dir) MarkCompleted(jobID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completed.add(jobID)
	return d.completed.save(filepath.Join(d.Path, "completed.json"))
}

// CompletedCount returns the number of tracked completed job IDs (tests).
func (d *Dir) CompletedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.completed.IDs)
}

type completedStore struct {
	IDs []string `json:"ids"`
	set map[string]struct{}
}

func loadCompleted(path string) (*completedStore, error) {
	cs := &completedStore{set: make(map[string]struct{})}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cs, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, cs); err != nil {
		// Corrupt file: start empty rather than crash the agent.
		return &completedStore{set: make(map[string]struct{})}, nil
	}
	cs.set = make(map[string]struct{}, len(cs.IDs))
	for _, id := range cs.IDs {
		cs.set[id] = struct{}{}
	}
	return cs, nil
}

func (c *completedStore) has(id string) bool {
	_, ok := c.set[id]
	return ok
}

func (c *completedStore) add(id string) {
	if id == "" {
		return
	}
	if _, ok := c.set[id]; ok {
		return
	}
	c.IDs = append(c.IDs, id)
	c.set[id] = struct{}{}
	for len(c.IDs) > MaxCompletedJobs {
		old := c.IDs[0]
		c.IDs = c.IDs[1:]
		delete(c.set, old)
	}
}

func (c *completedStore) save(path string) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, raw, 0o600)
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
