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
