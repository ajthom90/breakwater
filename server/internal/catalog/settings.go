package catalog

import (
	"context"
	"database/sql"
	"fmt"
)

// GetSetting returns a settings value by key. Empty string if missing.
func (db *DB) GetSetting(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("settings key required")
	}
	var v string
	err := db.sql.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get setting %q: %w", key, err)
	}
	return v, nil
}

// SetSetting upserts a settings key/value.
func (db *DB) SetSetting(ctx context.Context, key, value string) error {
	if key == "" {
		return fmt.Errorf("settings key required")
	}
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
		if err != nil {
			return fmt.Errorf("set setting %q: %w", key, err)
		}
		return nil
	})
}

// GetSettingsPrefix returns all settings whose keys have the given prefix
// (e.g. "smtp."). Values are returned as-is; callers must not log secrets.
func (db *DB) GetSettingsPrefix(ctx context.Context, prefix string) (map[string]string, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT key, value FROM settings WHERE key LIKE ?`, prefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
