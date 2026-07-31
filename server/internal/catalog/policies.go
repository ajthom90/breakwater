package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// Policy is a catalog retention/schedule policy row.
type Policy struct {
	ID             string
	Name           string
	ScheduleCron   string
	WindowStart    string
	WindowEnd      string
	ThrottleBPS    int64
	KeepLast       int
	KeepHourly     int
	KeepDaily      int
	KeepWeekly     int
	KeepMonthly    int
	KeepYearly     int
	PruneGraceDays int
	AppAware       bool
	IsDefault      bool
}

// DefaultPolicy returns the seeded "Standard Server" policy, or nil if missing.
func (db *DB) DefaultPolicy(ctx context.Context) (*Policy, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, schedule_cron, window_start, window_end, throttle_bps,
		       keep_last, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_yearly,
		       prune_grace_days, app_aware, is_default
		FROM policies WHERE is_default = 1 LIMIT 1`)
	return scanPolicy(row)
}

// PolicyByID loads a policy by primary key.
func (db *DB) PolicyByID(ctx context.Context, id string) (*Policy, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, name, schedule_cron, window_start, window_end, throttle_bps,
		       keep_last, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_yearly,
		       prune_grace_days, app_aware, is_default
		FROM policies WHERE id = ?`, id)
	return scanPolicy(row)
}

// PolicyForMachine returns the machine's assigned policy, or the default.
func (db *DB) PolicyForMachine(ctx context.Context, machineID string) (*Policy, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT p.id, p.name, p.schedule_cron, p.window_start, p.window_end, p.throttle_bps,
		       p.keep_last, p.keep_hourly, p.keep_daily, p.keep_weekly, p.keep_monthly, p.keep_yearly,
		       p.prune_grace_days, p.app_aware, p.is_default
		FROM machine_policies mp
		JOIN policies p ON p.id = mp.policy_id
		WHERE mp.machine_id = ?`, machineID)
	p, err := scanPolicy(row)
	if err != nil {
		return nil, err
	}
	if p != nil {
		return p, nil
	}
	return db.DefaultPolicy(ctx)
}

// ListPolicies returns all policies.
func (db *DB) ListPolicies(ctx context.Context) ([]Policy, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, name, schedule_cron, window_start, window_end, throttle_bps,
		       keep_last, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_yearly,
		       prune_grace_days, app_aware, is_default
		FROM policies ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// AssignPolicy binds a machine to a policy (upsert).
func (db *DB) AssignPolicy(ctx context.Context, machineID, policyID string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO machine_policies (machine_id, policy_id) VALUES (?, ?)
			ON CONFLICT(machine_id) DO UPDATE SET policy_id = excluded.policy_id`,
			machineID, policyID)
		return err
	})
}

func scanPolicy(row scannable) (*Policy, error) {
	var p Policy
	var app, def int
	err := row.Scan(
		&p.ID, &p.Name, &p.ScheduleCron, &p.WindowStart, &p.WindowEnd, &p.ThrottleBPS,
		&p.KeepLast, &p.KeepHourly, &p.KeepDaily, &p.KeepWeekly, &p.KeepMonthly, &p.KeepYearly,
		&p.PruneGraceDays, &app, &def,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan policy: %w", err)
	}
	p.AppAware = app != 0
	p.IsDefault = def != 0
	return &p, nil
}
