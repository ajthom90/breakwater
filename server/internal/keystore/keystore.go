// Package keystore holds the master key and per-repo encrypted secrets.
// Agents never hold decryption keys — only the hashing key for content IDs
// (sourced from the vault's kopia ContentFormat after repo create).
package keystore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/crypto/nacl/secretbox"

	"github.com/ajthom90/breakwater/server/internal/catalog"
)

// ErrHashingKeyNotSet is returned when GetHashingKey finds an empty placeholder
// (CreateRepoPassword seals empty until SetHashingKey runs) (R2-6).
var ErrHashingKeyNotSet = errors.New("hashing key not set")

// Store encrypts per-repo passwords and hashing keys with a master key.
type Store struct {
	db     *catalog.DB
	master [32]byte
	mu     sync.Mutex
}

// OpenOrCreate loads master.key from path or generates a new one.
func OpenOrCreate(db *catalog.DB, masterKeyPath string) (*Store, error) {
	var master [32]byte
	data, err := os.ReadFile(masterKeyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		if _, err := rand.Read(master[:]); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dirOf(masterKeyPath), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(masterKeyPath, master[:], 0o600); err != nil {
			return nil, err
		}
	} else {
		if len(data) != 32 {
			return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(data))
		}
		copy(master[:], data)
	}
	return &Store{db: db, master: master}, nil
}

// CreateRepoPassword generates a per-repo password and stores it.
// Hashing key is stored later via SetHashingKey after the vault is created
// (must come from kopia ContentFormat, not random bytes — REVIEW-M1 H1).
func (s *Store) CreateRepoPassword(ctx context.Context, repoID string) (repoPassword string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pw [32]byte
	if _, err := rand.Read(pw[:]); err != nil {
		return "", err
	}
	repoPassword = fmt.Sprintf("%x", pw[:])

	pwEnc, err := s.seal([]byte(repoPassword))
	if err != nil {
		return "", err
	}
	// Placeholder empty hashing key until SetHashingKey; column is NOT NULL.
	hkEnc, err := s.seal([]byte{})
	if err != nil {
		return "", err
	}

	err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO keystore (repo_id, repo_password_enc, hashing_key_enc, hashing_algorithm)
			VALUES (?, ?, ?, ?)`, repoID, pwEnc, hkEnc, "")
		return err
	})
	if err != nil {
		return "", err
	}
	return repoPassword, nil
}

// SetHashingKey stores the vault-sourced content-ID HMAC secret and algorithm name (R2-5).
func (s *Store) SetHashingKey(ctx context.Context, repoID string, hashingKey []byte, algorithm string) error {
	if len(hashingKey) == 0 {
		return fmt.Errorf("hashing key must not be empty")
	}
	if algorithm == "" {
		return fmt.Errorf("hashing algorithm must not be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	hkEnc, err := s.seal(hashingKey)
	if err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE keystore SET hashing_key_enc = ?, hashing_algorithm = ? WHERE repo_id = ?`,
			hkEnc, algorithm, repoID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("keystore row not found for repo %s", repoID)
		}
		return nil
	})
}

// DeleteRepo removes a keystore row (enrollment compensation, R2-9).
func (s *Store) DeleteRepo(ctx context.Context, repoID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM keystore WHERE repo_id = ?`, repoID)
		return err
	})
}

// GetRepoPassword decrypts the repository password.
func (s *Store) GetRepoPassword(ctx context.Context, repoID string) (string, error) {
	var enc []byte
	err := s.db.SQL().QueryRowContext(ctx,
		`SELECT repo_password_enc FROM keystore WHERE repo_id = ?`, repoID).Scan(&enc)
	if err != nil {
		return "", err
	}
	pt, err := s.open(enc)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// GetHashingKey decrypts the hashing key and returns the algorithm name.
// Returns ErrHashingKeyNotSet if the key was never set (empty placeholder) (R2-6).
func (s *Store) GetHashingKey(ctx context.Context, repoID string) (key []byte, algorithm string, err error) {
	var enc []byte
	var algo sql.NullString
	err = s.db.SQL().QueryRowContext(ctx,
		`SELECT hashing_key_enc, hashing_algorithm FROM keystore WHERE repo_id = ?`, repoID).
		Scan(&enc, &algo)
	if err != nil {
		return nil, "", err
	}
	pt, err := s.open(enc)
	if err != nil {
		return nil, "", err
	}
	if len(pt) == 0 {
		return nil, "", ErrHashingKeyNotSet
	}
	return pt, algo.String, nil
}

func (s *Store) seal(plain []byte) ([]byte, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	out := make([]byte, 24, 24+len(plain)+secretbox.Overhead)
	copy(out, nonce[:])
	return secretbox.Seal(out, plain, &nonce, &s.master), nil
}

func (s *Store) open(box []byte) ([]byte, error) {
	if len(box) < 24+secretbox.Overhead {
		return nil, fmt.Errorf("ciphertext too short")
	}
	var nonce [24]byte
	copy(nonce[:], box[:24])
	pt, ok := secretbox.Open(nil, box[24:], &nonce, &s.master)
	if !ok {
		return nil, fmt.Errorf("decryption failed")
	}
	return pt, nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}

// DerivePurposeKey is a helper for future AEAD uses (not master key export).
func DerivePurposeKey(master *[32]byte, purpose string) [32]byte {
	h := sha256.New()
	h.Write(master[:])
	h.Write([]byte(purpose))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
