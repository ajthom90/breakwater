package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Job states (schema + engine). Terminal states never resurrect to running.
const (
	JobStatePending    = "pending"
	JobStateRunning    = "running"
	JobStateCancelling = "cancelling" // vault-writing: JobCancel sent, lease held until result/teardown
	JobStateSuccess    = "success"
	JobStateFailed     = "failed"
	JobStateCancelled  = "cancelled"
)

// Job is a catalog row for a scheduled or dispatched unit of work.
type Job struct {
	ID           string
	MachineID    string // empty for pure server-side jobs without a machine
	Type         string // inventory|noop|file|image|hyperv|restore|prune|verify|replicate|update
	State        string
	StartedAt    *time.Time
	FinishedAt   *time.Time
	BytesRead    int64
	BytesStored  int64
	ErrorMessage string
	LogRef       string
	ParamsJSON   string
	CreatedAt    time.Time
}

// InsertJob creates a job in pending state.
func (db *DB) InsertJob(ctx context.Context, j Job) error {
	if j.State == "" {
		j.State = JobStatePending
	}
	if j.ParamsJSON == "" {
		j.ParamsJSON = "{}"
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		var machine any
		if j.MachineID != "" {
			machine = j.MachineID
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, machine_id, type, state, params_json, log_ref)
			VALUES (?, ?, ?, ?, ?, ?)`,
			j.ID, machine, j.Type, j.State, j.ParamsJSON, j.LogRef)
		if err != nil {
			return fmt.Errorf("insert job: %w", err)
		}
		return nil
	})
}

// JobByID loads a job by primary key.
func (db *DB) JobByID(ctx context.Context, id string) (*Job, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, COALESCE(machine_id, ''), type, state, started_at, finished_at,
		       bytes_read, bytes_stored, error_message, log_ref, params_json, created_at
		FROM jobs WHERE id = ?`, id)
	return scanJob(row)
}

// ListJobsByMachine returns jobs for a machine, newest first.
func (db *DB) ListJobsByMachine(ctx context.Context, machineID string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, COALESCE(machine_id, ''), type, state, started_at, finished_at,
		       bytes_read, bytes_stored, error_message, log_ref, params_json, created_at
		FROM jobs WHERE machine_id = ?
		ORDER BY created_at DESC
		LIMIT ?`, machineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// ListPendingJobsByMachine returns pending jobs oldest-first (dispatch order).
func (db *DB) ListPendingJobsByMachine(ctx context.Context, machineID string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 64
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, COALESCE(machine_id, ''), type, state, started_at, finished_at,
		       bytes_read, bytes_stored, error_message, log_ref, params_json, created_at
		FROM jobs
		WHERE machine_id = ? AND state = ?
		ORDER BY created_at ASC
		LIMIT ?`, machineID, JobStatePending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// CountJobsByMachineState counts jobs in a given state for a machine.
func (db *DB) CountJobsByMachineState(ctx context.Context, machineID, state string) (int, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM jobs WHERE machine_id = ? AND state = ?`,
		machineID, state).Scan(&n)
	return n, err
}

// ListJobsByState returns all jobs in the given state (oldest first).
func (db *DB) ListJobsByState(ctx context.Context, state string) ([]Job, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, COALESCE(machine_id, ''), type, state, started_at, finished_at,
		       bytes_read, bytes_stored, error_message, log_ref, params_json, created_at
		FROM jobs WHERE state = ?
		ORDER BY created_at ASC`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

// JobTransition holds optional field updates during a state transition.
type JobTransition struct {
	SetStarted   bool
	SetFinished  bool
	BytesRead    *int64
	BytesStored  *int64
	ErrorMessage *string
	LogRef       *string
}

// TransitionJob atomically moves a job from one of fromStates → toState.
// Returns (true, nil) if a row was updated; (false, nil) if no matching row
// (already terminal, wrong state, missing). Used for idempotent results.
func (db *DB) TransitionJob(ctx context.Context, id string, fromStates []string, toState string, opts JobTransition) (bool, error) {
	if len(fromStates) == 0 {
		return false, fmt.Errorf("fromStates required")
	}

	var b strings.Builder
	b.WriteString(`UPDATE jobs SET state = ?`)
	args := []any{toState}

	if opts.SetStarted {
		b.WriteString(`, started_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`)
	}
	if opts.SetFinished {
		b.WriteString(`, finished_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`)
	}
	if opts.BytesRead != nil {
		b.WriteString(`, bytes_read = ?`)
		args = append(args, *opts.BytesRead)
	}
	if opts.BytesStored != nil {
		b.WriteString(`, bytes_stored = ?`)
		args = append(args, *opts.BytesStored)
	}
	if opts.ErrorMessage != nil {
		b.WriteString(`, error_message = ?`)
		args = append(args, *opts.ErrorMessage)
	}
	if opts.LogRef != nil {
		b.WriteString(`, log_ref = ?`)
		args = append(args, *opts.LogRef)
	}

	b.WriteString(` WHERE id = ? AND state IN (`)
	args = append(args, id)
	for i, s := range fromStates {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('?')
		args = append(args, s)
	}
	b.WriteByte(')')

	var applied bool
	err := db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, b.String(), args...)
		if err != nil {
			return fmt.Errorf("transition job: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		applied = n > 0
		return nil
	})
	return applied, err
}

// UpdateJobProgress updates bytes while job is running (no state change).
// No-op if the job is not running (idempotent against late progress).
func (db *DB) UpdateJobProgress(ctx context.Context, id string, bytesRead, bytesStored int64) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE jobs SET bytes_read = ?, bytes_stored = ?
			WHERE id = ? AND state = ?`,
			bytesRead, bytesStored, id, JobStateRunning)
		return err
	})
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func scanJob(row scannable) (*Job, error) {
	var j Job
	var started, finished sql.NullString
	var created string
	err := row.Scan(&j.ID, &j.MachineID, &j.Type, &j.State, &started, &finished,
		&j.BytesRead, &j.BytesStored, &j.ErrorMessage, &j.LogRef, &j.ParamsJSON, &created)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if started.Valid {
		t, _ := time.Parse(time.RFC3339Nano, started.String)
		j.StartedAt = &t
	}
	if finished.Valid {
		t, _ := time.Parse(time.RFC3339Nano, finished.String)
		j.FinishedAt = &t
	}
	return &j, nil
}
