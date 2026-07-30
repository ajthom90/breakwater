// Package state manages the agent state directory under ProgramData (or a test path).
//
// Layout (default C:\ProgramData\Breakwater\ on Windows):
//
//	identity.json          — machine id, server addr/FP, hashing key+algorithm
//	cert.pem / key.pem     — enrolled client credentials
//	completed.json         — recently completed job outcomes (reconnect idempotency)
//	pending-enroll.token   — MSI/first-start enrollment token (SecureDir-restricted)
//	logs/                  — agent log files
//
// Identity is written atomically (temp → fsync → rename → dir fsync). A
// half-written identity must never be loadable.
package state

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ajthom90/breakwater/agent/internal/identity"
)

// DefaultStateDir is the Windows production path. Overridable via --state-dir.
const DefaultStateDir = `C:\ProgramData\Breakwater`

// MaxCompletedJobs is the ring size for completed job records.
const MaxCompletedJobs = 1024

// PendingEnrollTokenFile is the SecureDir-restricted token path (S4-F2).
const PendingEnrollTokenFile = "pending-enroll.token"

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
	Log  *slog.Logger

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
		return nil, fmt.Errorf("secure state dir: %w", err)
	}
	log := slog.Default()
	d := &Dir{Path: path, Log: log}
	cs, err := loadCompleted(filepath.Join(path, "completed.json"), log)
	if err != nil {
		return nil, err
	}
	d.completed = cs
	return d, nil
}

// IdentityPath returns the path to identity.json.
func (d *Dir) IdentityPath() string { return filepath.Join(d.Path, "identity.json") }

// PendingTokenPath is the SecureDir-restricted enrollment token file (S4-F2).
func (d *Dir) PendingTokenPath() string {
	return filepath.Join(d.Path, PendingEnrollTokenFile)
}

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
	if err := identity.Save(d.Path, id); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(d.IdentityPath(), raw, 0o600); err != nil {
		_ = os.Remove(filepath.Join(d.Path, "cert.pem"))
		_ = os.Remove(filepath.Join(d.Path, "key.pem"))
		return err
	}
	return nil
}

// WritePendingToken stores the enrollment token under the SecureDir-restricted
// state directory (S4-F2). Prefer this over HKLM.
func (d *Dir) WritePendingToken(token string) error {
	if token == "" {
		return nil
	}
	return writeAtomic(d.PendingTokenPath(), []byte(token), 0o600)
}

// ReadPendingToken returns and does not delete the pending token file.
func (d *Dir) ReadPendingToken() string {
	raw, err := os.ReadFile(d.PendingTokenPath())
	if err != nil {
		return ""
	}
	return string(raw)
}

// ClearPendingToken deletes the pending-enroll.token file (S4-F2: delete, not blank).
func (d *Dir) ClearPendingToken() {
	_ = os.Remove(d.PendingTokenPath())
}

// JobOutcome is the recorded result of a completed job (S4-F3).
type JobOutcome struct {
	ID           string `json:"id"`
	Success      bool   `json:"success"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// HasCompleted reports whether jobID was already completed locally.
func (d *Dir) HasCompleted(jobID string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.completed.has(jobID)
}

// CompletedOutcome returns the recorded outcome for jobID (S4-F3).
// ok is false if the job is not in the completed set.
func (d *Dir) CompletedOutcome(jobID string) (ok, success bool, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, found := d.completed.get(jobID)
	if !found {
		return false, false, ""
	}
	return true, e.Success, e.ErrorMessage
}

// MarkCompleted records a completed job outcome for reconnect idempotency.
// success/errMsg are the real terminal result — never synthesize success on
// replay (S4-F3).
func (d *Dir) MarkCompleted(jobID string, success bool, errMsg string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.completed.add(JobOutcome{ID: jobID, Success: success, ErrorMessage: errMsg})
	return d.completed.save(filepath.Join(d.Path, "completed.json"))
}

// CompletedCount returns the number of tracked completed jobs (tests).
func (d *Dir) CompletedCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.completed.Entries)
}

type completedStore struct {
	Entries []JobOutcome `json:"entries"`
	// Legacy field for migration from 37e5fc3 {"ids":[...]} format.
	IDs []string `json:"ids,omitempty"`
	set map[string]JobOutcome
}

func loadCompleted(path string, log *slog.Logger) (*completedStore, error) {
	cs := &completedStore{set: make(map[string]JobOutcome)}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cs, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(raw, cs); err != nil {
		// Corrupt file: start empty rather than crash the agent — but log loudly (S4-F4).
		if log != nil {
			log.Error("completed.json corrupt; resetting completed-job set (idempotency lost until jobs re-complete)",
				"path", path, "err", err)
		}
		return &completedStore{set: make(map[string]JobOutcome)}, nil
	}
	cs.set = make(map[string]JobOutcome, len(cs.Entries)+len(cs.IDs))
	for _, e := range cs.Entries {
		if e.ID == "" {
			continue
		}
		cs.set[e.ID] = e
	}
	// Migrate legacy ids-only format: treat as success=true (unknown; old agents
	// only marked after any terminal — but we cannot invent failure either).
	// New writers always use Entries; legacy entries without outcome default to
	// success=false so we never invent a success we did not record (S4-F3).
	for _, id := range cs.IDs {
		if id == "" {
			continue
		}
		if _, ok := cs.set[id]; ok {
			continue
		}
		// Unknown outcome from legacy: re-run is safer than claiming success.
		// We do not put them in set as success — leave them out of set so they
		// re-run, OR mark with success=false. Re-run is dedup-safe for backups.
		// Leaving them out means HasCompleted=false → re-run. Clear IDs.
		_ = id
	}
	cs.IDs = nil // drop legacy on next save
	// Rebuild Entries from set for stable ring.
	cs.Entries = cs.Entries[:0]
	for _, e := range cs.set {
		cs.Entries = append(cs.Entries, e)
	}
	return cs, nil
}

func (c *completedStore) has(id string) bool {
	_, ok := c.set[id]
	return ok
}

func (c *completedStore) get(id string) (JobOutcome, bool) {
	e, ok := c.set[id]
	return e, ok
}

func (c *completedStore) add(e JobOutcome) {
	if e.ID == "" {
		return
	}
	if _, ok := c.set[e.ID]; ok {
		// Update outcome if already present (should be identical).
		c.set[e.ID] = e
		for i := range c.Entries {
			if c.Entries[i].ID == e.ID {
				c.Entries[i] = e
				return
			}
		}
		return
	}
	c.Entries = append(c.Entries, e)
	c.set[e.ID] = e
	for len(c.Entries) > MaxCompletedJobs {
		old := c.Entries[0]
		c.Entries = c.Entries[1:]
		delete(c.set, old.ID)
	}
}

func (c *completedStore) save(path string) error {
	// Never write legacy IDs field.
	out := struct {
		Entries []JobOutcome `json:"entries"`
	}{Entries: c.Entries}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, raw, 0o600)
}

// writeAtomic: temp → write → fsync → chmod → close → rename → fsync dir (S4-F4).
// On Windows, directory fsync is best-effort (semantics differ; untested-on-Windows).
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
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync temp: %w", err)
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
	if err := syncDir(dir); err != nil {
		// Directory fsync failures are logged but non-fatal on platforms where
		// the operation is unsupported; still return so callers can decide.
		// On Linux/macOS a real fsync failure is serious — return it.
		return fmt.Errorf("fsync dir after rename: %w", err)
	}
	return nil
}
