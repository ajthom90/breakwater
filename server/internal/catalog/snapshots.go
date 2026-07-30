package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Snapshot is a catalog row mirroring a vault snapshot record (rebuildable index).
type Snapshot struct {
	ID           string
	MachineID    string
	Kind         string // file|image|hyperv
	Source       string
	ManifestRef  string // kopia/vault snapshot record ID
	RootObjectID string
	GFSTags      string
	VerifyState  string
	DeletedAt    *time.Time
	JobID        string
	BytesRead    int64
	BytesStored  int64
	CreatedAt    time.Time
}

// InsertSnapshot mirrors a committed vault snapshot into the catalog index.
func (db *DB) InsertSnapshot(ctx context.Context, s Snapshot) error {
	if s.ID == "" || s.MachineID == "" || s.ManifestRef == "" {
		return fmt.Errorf("snapshot id, machine_id, and manifest_ref required")
	}
	if s.Kind == "" {
		s.Kind = "file"
	}
	if s.VerifyState == "" {
		s.VerifyState = "none"
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		var job any
		if s.JobID != "" {
			job = s.JobID
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO snapshots (
				id, machine_id, kind, source, manifest_ref, root_object_id,
				gfs_tags, verify_state, job_id, bytes_read, bytes_stored
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.MachineID, s.Kind, s.Source, s.ManifestRef, s.RootObjectID,
			s.GFSTags, s.VerifyState, job, s.BytesRead, s.BytesStored)
		if err != nil {
			return fmt.Errorf("insert snapshot: %w", err)
		}
		return nil
	})
}

// SnapshotByID loads a catalog snapshot by primary key.
func (db *DB) SnapshotByID(ctx context.Context, id string) (*Snapshot, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, machine_id, kind, source, manifest_ref, root_object_id,
		       gfs_tags, verify_state, deleted_at, COALESCE(job_id, ''),
		       bytes_read, bytes_stored, created_at
		FROM snapshots WHERE id = ?`, id)
	return scanSnapshot(row)
}

// SnapshotByManifestRef loads by vault manifest/record ID.
func (db *DB) SnapshotByManifestRef(ctx context.Context, manifestRef string) (*Snapshot, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, machine_id, kind, source, manifest_ref, root_object_id,
		       gfs_tags, verify_state, deleted_at, COALESCE(job_id, ''),
		       bytes_read, bytes_stored, created_at
		FROM snapshots WHERE manifest_ref = ?`, manifestRef)
	return scanSnapshot(row)
}

// ListSnapshotsByMachine returns snapshots newest first (excludes soft-deleted).
func (db *DB) ListSnapshotsByMachine(ctx context.Context, machineID string, limit int) ([]Snapshot, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, machine_id, kind, source, manifest_ref, root_object_id,
		       gfs_tags, verify_state, deleted_at, COALESCE(job_id, ''),
		       bytes_read, bytes_stored, created_at
		FROM snapshots
		WHERE machine_id = ? AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT ?`, machineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		s, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// SoftDeleteSnapshot sets deleted_at (forget); prune reclaims later.
func (db *DB) SoftDeleteSnapshot(ctx context.Context, id string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE snapshots SET deleted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ? AND deleted_at IS NULL`, id)
		return err
	})
}

func scanSnapshot(row scannable) (*Snapshot, error) {
	var s Snapshot
	var deleted sql.NullString
	var created string
	err := row.Scan(&s.ID, &s.MachineID, &s.Kind, &s.Source, &s.ManifestRef, &s.RootObjectID,
		&s.GFSTags, &s.VerifyState, &deleted, &s.JobID,
		&s.BytesRead, &s.BytesStored, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if deleted.Valid {
		t, _ := time.Parse(time.RFC3339Nano, deleted.String)
		s.DeletedAt = &t
	}
	return &s, nil
}
