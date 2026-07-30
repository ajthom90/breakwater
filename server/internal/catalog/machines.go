package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Machine is a catalog row for an enrolled agent.
type Machine struct {
	ID           string
	CertFP       string
	Hostname     string
	OSInfo       string
	AgentVersion string
	Status       string
	RepoID       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastSeenAt   *time.Time
}

// InsertMachine inserts a newly enrolled machine.
func (db *DB) InsertMachine(ctx context.Context, m Machine) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO machines (id, cert_fp, hostname, os_info, agent_version, status, repo_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.CertFP, m.Hostname, m.OSInfo, m.AgentVersion, m.Status, m.RepoID)
		if err != nil {
			return fmt.Errorf("insert machine: %w", err)
		}
		return nil
	})
}

// MachineByCertFP looks up a machine by client certificate fingerprint.
func (db *DB) MachineByCertFP(ctx context.Context, fp string) (*Machine, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, cert_fp, hostname, os_info, agent_version, status, repo_id,
		       created_at, updated_at, last_seen_at
		FROM machines WHERE cert_fp = ?`, fp)
	return scanMachine(row)
}

// MachineByID looks up a machine by ID.
func (db *DB) MachineByID(ctx context.Context, id string) (*Machine, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, cert_fp, hostname, os_info, agent_version, status, repo_id,
		       created_at, updated_at, last_seen_at
		FROM machines WHERE id = ?`, id)
	return scanMachine(row)
}

// ListMachines returns all machines ordered by hostname.
func (db *DB) ListMachines(ctx context.Context) ([]Machine, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, cert_fp, hostname, os_info, agent_version, status, repo_id,
		       created_at, updated_at, last_seen_at
		FROM machines ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Machine
	for rows.Next() {
		m, err := scanMachineRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// Machine presence statuses (subset of machines.status).
// "active" = control channel currently connected (online).
// "enrolled" = enrolled but not currently connected (offline after disconnect).
const (
	MachineStatusEnrolled = "enrolled"
	MachineStatusActive   = "active"
	MachineStatusDisabled = "disabled"
	MachineStatusRemoved  = "removed"
)

// TouchLastSeen updates last_seen_at (and updated_at) without changing status.
func (db *DB) TouchLastSeen(ctx context.Context, id string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE machines SET last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
			                    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ?`, id)
		return err
	})
}

// SetMachineOnline marks the machine active (control channel up) and touches last_seen.
func (db *DB) SetMachineOnline(ctx context.Context, id string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE machines SET status = ?,
			                    last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'),
			                    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ? AND status NOT IN ('disabled', 'removed')`,
			MachineStatusActive, id)
		return err
	})
}

// SetMachineOffline marks the machine enrolled (channel down). last_seen_at is
// left as the last heartbeat so the UI can show "last seen …".
func (db *DB) SetMachineOffline(ctx context.Context, id string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE machines SET status = ?,
			                    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE id = ? AND status = ?`,
			MachineStatusEnrolled, id, MachineStatusActive)
		return err
	})
}

type scannable interface {
	Scan(dest ...any) error
}

func scanMachine(row scannable) (*Machine, error) {
	var m Machine
	var created, updated string
	var lastSeen sql.NullString
	err := row.Scan(&m.ID, &m.CertFP, &m.Hostname, &m.OSInfo, &m.AgentVersion,
		&m.Status, &m.RepoID, &created, &updated, &lastSeen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	m.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	if lastSeen.Valid {
		t, _ := time.Parse(time.RFC3339Nano, lastSeen.String)
		m.LastSeenAt = &t
	}
	return &m, nil
}

func scanMachineRows(rows *sql.Rows) (*Machine, error) {
	return scanMachine(rows)
}
