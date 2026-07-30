package catalog

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// JobListFilter filters ListJobs. Empty fields are ignored.
type JobListFilter struct {
	MachineID string
	State     string
	Limit     int
}

// ListJobs returns jobs newest-first with optional machine/state filters.
func (db *DB) ListJobs(ctx context.Context, f JobListFilter) ([]Job, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var b strings.Builder
	b.WriteString(`
		SELECT id, COALESCE(machine_id, ''), type, state, started_at, finished_at,
		       bytes_read, bytes_stored, error_message, log_ref, params_json, created_at
		FROM jobs WHERE 1=1`)
	args := []any{}
	if f.MachineID != "" {
		b.WriteString(` AND machine_id = ?`)
		args = append(args, f.MachineID)
	}
	if f.State != "" {
		b.WriteString(` AND state = ?`)
		args = append(args, f.State)
	}
	b.WriteString(` ORDER BY created_at DESC LIMIT ?`)
	args = append(args, limit)

	rows, err := db.sql.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// SnapshotListFilter filters ListSnapshots.
type SnapshotListFilter struct {
	MachineID string
	Limit     int
}

// ListSnapshots returns snapshots newest-first (excludes soft-deleted).
// machine_id is optional; when empty, returns fleet-wide.
func (db *DB) ListSnapshots(ctx context.Context, f SnapshotListFilter) ([]Snapshot, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var b strings.Builder
	b.WriteString(`
		SELECT id, machine_id, kind, source, manifest_ref, root_object_id,
		       gfs_tags, verify_state, deleted_at, COALESCE(job_id, ''),
		       bytes_read, bytes_stored, created_at
		FROM snapshots WHERE deleted_at IS NULL`)
	args := []any{}
	if f.MachineID != "" {
		b.WriteString(` AND machine_id = ?`)
		args = append(args, f.MachineID)
	}
	b.WriteString(` ORDER BY created_at DESC LIMIT ?`)
	args = append(args, limit)

	rows, err := db.sql.QueryContext(ctx, b.String(), args...)
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

// FleetSummary is aggregate counts for the dashboard.
type FleetSummary struct {
	MachinesTotal   int
	MachinesOnline  int // status = active
	MachinesOffline int // enrolled (connected before, not now) + other non-active
	JobsLast24h     int
	JobsSuccess24h  int
	JobsFailed24h   int
	JobsRunning     int
	SnapshotsTotal  int
	// CapacityBytes is not available in M2 (no vault stats aggregation yet).
	// API returns null/omitted; UI labels as placeholder.
}

// Summary returns fleet-wide counts for the dashboard.
func (db *DB) Summary(ctx context.Context) (FleetSummary, error) {
	var s FleetSummary
	since := time.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02T15:04:05.000Z")

	err := db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM machines WHERE status NOT IN ('removed')`).Scan(&s.MachinesTotal)
	if err != nil {
		return s, fmt.Errorf("count machines: %w", err)
	}
	err = db.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM machines WHERE status = ?`, MachineStatusActive).Scan(&s.MachinesOnline)
	if err != nil {
		return s, fmt.Errorf("count online: %w", err)
	}
	s.MachinesOffline = s.MachinesTotal - s.MachinesOnline
	if s.MachinesOffline < 0 {
		s.MachinesOffline = 0
	}

	err = db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs WHERE created_at >= ?`, since).Scan(&s.JobsLast24h)
	if err != nil {
		return s, fmt.Errorf("count jobs 24h: %w", err)
	}
	err = db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs WHERE state = ? AND created_at >= ?`,
		JobStateSuccess, since).Scan(&s.JobsSuccess24h)
	if err != nil {
		return s, fmt.Errorf("count success 24h: %w", err)
	}
	err = db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs WHERE state = ? AND created_at >= ?`,
		JobStateFailed, since).Scan(&s.JobsFailed24h)
	if err != nil {
		return s, fmt.Errorf("count failed 24h: %w", err)
	}
	err = db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs WHERE state IN (?, ?)`,
		JobStateRunning, JobStateCancelling).Scan(&s.JobsRunning)
	if err != nil {
		return s, fmt.Errorf("count running: %w", err)
	}
	err = db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM snapshots WHERE deleted_at IS NULL`).Scan(&s.SnapshotsTotal)
	if err != nil {
		return s, fmt.Errorf("count snapshots: %w", err)
	}
	return s, nil
}
