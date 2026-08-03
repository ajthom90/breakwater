package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// EnrollToken is a one-time enrollment token row.
type EnrollToken struct {
	ID         string
	SecretHash string
	ExpiresAt  time.Time
	UsedAt     *time.Time
	CreatedBy  string
	MachineID  string
	CreatedAt  time.Time
}

// InsertEnrollToken stores a new token (hash of secret only).
func (db *DB) InsertEnrollToken(ctx context.Context, id, secret, createdBy string, expiresAt time.Time) error {
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO enroll_tokens (id, secret_hash, expires_at, created_by)
			VALUES (?, ?, ?, ?)`,
			id, hash, expiresAt.UTC().Format(time.RFC3339Nano), createdBy)
		return err
	})
}

// ConsumeEnrollToken validates secret, marks single-use, returns token id or error.
func (db *DB) ConsumeEnrollToken(ctx context.Context, secret, machineID string) (tokenID string, err error) {
	sum := sha256.Sum256([]byte(secret))
	hash := hex.EncodeToString(sum[:])

	err = db.WithTx(ctx, func(tx *sql.Tx) error {
		var id, expires string
		var used sql.NullString
		row := tx.QueryRowContext(ctx, `
			SELECT id, expires_at, used_at FROM enroll_tokens WHERE secret_hash = ?`, hash)
		if err := row.Scan(&id, &expires, &used); err == sql.ErrNoRows {
			return fmt.Errorf("invalid enrollment token")
		} else if err != nil {
			return err
		}
		if used.Valid {
			return fmt.Errorf("enrollment token already used")
		}
		exp, err := time.Parse(time.RFC3339Nano, expires)
		if err != nil {
			exp, err = time.Parse(time.RFC3339, expires)
			if err != nil {
				return fmt.Errorf("bad expires_at: %w", err)
			}
		}
		if time.Now().UTC().After(exp) {
			return fmt.Errorf("enrollment token expired")
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE enroll_tokens
			SET used_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), machine_id = ?
			WHERE id = ? AND used_at IS NULL`, machineID, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("enrollment token already used")
		}
		tokenID = id
		return nil
	})
	return tokenID, err
}

// ReleaseEnrollToken un-consumes a token after a failed enrollment (R2-9).
// Clears used_at and machine_id so the token can be retried.
func (db *DB) ReleaseEnrollToken(ctx context.Context, tokenID string) error {
	return db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE enroll_tokens
			SET used_at = NULL, machine_id = NULL
			WHERE id = ?`, tokenID)
		return err
	})
}

// HashSecret is exported for tests.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// EnrollTokenByID loads a token metadata row (never returns the secret; hash
// is available for test assertions only — REST list omits it).
func (db *DB) EnrollTokenByID(ctx context.Context, id string) (*EnrollToken, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT id, secret_hash, expires_at, used_at, COALESCE(created_by, ''),
		       COALESCE(machine_id, ''), created_at
		FROM enroll_tokens WHERE id = ?`, id)
	return scanEnrollToken(row)
}

// ListEnrollTokens returns token metadata newest-first (no secrets).
func (db *DB) ListEnrollTokens(ctx context.Context, limit int) ([]EnrollToken, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := db.sql.QueryContext(ctx, `
		SELECT id, secret_hash, expires_at, used_at, COALESCE(created_by, ''),
		       COALESCE(machine_id, ''), created_at
		FROM enroll_tokens
		ORDER BY created_at DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EnrollToken
	for rows.Next() {
		t, err := scanEnrollToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func scanEnrollToken(row scannable) (*EnrollToken, error) {
	var t EnrollToken
	var expires, created string
	var used, machine sql.NullString
	if err := row.Scan(&t.ID, &t.SecretHash, &expires, &used, &t.CreatedBy, &machine, &created); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var err error
	t.ExpiresAt, err = parseTimeFlexible(expires)
	if err != nil {
		return nil, fmt.Errorf("expires_at: %w", err)
	}
	t.CreatedAt, err = parseTimeFlexible(created)
	if err != nil {
		return nil, fmt.Errorf("created_at: %w", err)
	}
	if used.Valid {
		ut, err := parseTimeFlexible(used.String)
		if err == nil {
			t.UsedAt = &ut
		}
	}
	if machine.Valid {
		t.MachineID = machine.String
	}
	return &t, nil
}

func parseTimeFlexible(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
	}
	return t, err
}
