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
		if !s.CreatedAt.IsZero() {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO snapshots (
					id, machine_id, kind, source, manifest_ref, root_object_id,
					gfs_tags, verify_state, job_id, bytes_read, bytes_stored, created_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				s.ID, s.MachineID, s.Kind, s.Source, s.ManifestRef, s.RootObjectID,
				s.GFSTags, s.VerifyState, job, s.BytesRead, s.BytesStored,
				s.CreatedAt.UTC().Format(time.RFC3339Nano))
			if err != nil {
				return fmt.Errorf("insert snapshot: %w", err)
			}
			return nil
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
// deletedAt must be supplied by the caller (injected clock) — never time.Now
// inside retention math. No-op if already soft-deleted.
func (db *DB) SoftDeleteSnapshot(ctx context.Context, id string, deletedAt time.Time) error {
	if deletedAt.IsZero() {
		return fmt.Errorf("deleted_at required")
	}
	at := deletedAt.UTC().Format(time.RFC3339Nano)
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE snapshots SET deleted_at = ?
			WHERE id = ? AND deleted_at IS NULL`, at, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// Already deleted or missing — caller may treat as idempotent.
			return nil
		}
		return nil
	})
}

// UndeleteSnapshot clears deleted_at (restore from soft-delete within grace).
// Returns false if the row was not soft-deleted.
func (db *DB) UndeleteSnapshot(ctx context.Context, id string) (bool, error) {
	var applied bool
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE snapshots SET deleted_at = NULL
			WHERE id = ? AND deleted_at IS NOT NULL`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		applied = n > 0
		return nil
	})
	return applied, err
}

// SetSnapshotVerifyState updates verify_state (scrub result).
func (db *DB) SetSnapshotVerifyState(ctx context.Context, id, state string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE snapshots SET verify_state = ? WHERE id = ?`, state, id)
		return err
	})
}

// ListAllSnapshotsByMachine returns snapshots including soft-deleted (newest first).
func (db *DB) ListAllSnapshotsByMachine(ctx context.Context, machineID string, limit int) ([]Snapshot, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, machine_id, kind, source, manifest_ref, root_object_id,
		       gfs_tags, verify_state, deleted_at, COALESCE(job_id, ''),
		       bytes_read, bytes_stored, created_at
		FROM snapshots
		WHERE machine_id = ?
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

// ListSoftDeletedSnapshots returns soft-deleted snapshots for a machine.
func (db *DB) ListSoftDeletedSnapshots(ctx context.Context, machineID string) ([]Snapshot, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, machine_id, kind, source, manifest_ref, root_object_id,
		       gfs_tags, verify_state, deleted_at, COALESCE(job_id, ''),
		       bytes_read, bytes_stored, created_at
		FROM snapshots
		WHERE machine_id = ? AND deleted_at IS NOT NULL
		ORDER BY deleted_at ASC`, machineID)
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

// HardDeleteSnapshot removes a catalog snapshot row after vault prune eligibility.
func (db *DB) HardDeleteSnapshot(ctx context.Context, id string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id = ?`, id)
		return err
	})
}

// DeleteAllSnapshots removes every snapshot row (server-loss drill: wipe the
// rebuildable index before rescan). Does not touch vaults or machines/keystore.
func (db *DB) DeleteAllSnapshots(ctx context.Context) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM snapshots`)
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
