// Package catalog is the SQLite system of record for Breakwater metadata.
// Snapshots are a rebuildable index; authoritative manifests live in the vault.
package catalog

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaFS embed.FS

// SchemaVersion is the current migration version.
// v1: initial M1 schema (pre actor_type UNIQUE / hashing_algorithm).
// v2: actor_type on audit_events, UNIQUE index on enroll_tokens.secret_hash,
//
//	hashing_algorithm on keystore (R2-5/R2-8).
const SchemaVersion = 2

// DB wraps SQLite with a single-writer discipline.
type DB struct {
	sql *sql.DB
	mu  sync.Mutex // one writer goroutine discipline for safety
}

// Open opens (or creates) the catalog at path with WAL mode and applies migrations.
func Open(path string) (*DB, error) {
	// modernc.org/sqlite pure Go driver — CGO_ENABLED=0.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	sqldb.SetMaxOpenConns(1) // single writer; readers share via WAL
	if _, err := sqldb.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("wal: %w", err)
	}
	db := &DB{sql: sqldb}
	if err := db.migrate(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

// Close closes the database.
func (db *DB) Close() error {
	return db.sql.Close()
}

// SQL returns the underlying *sql.DB for advanced queries (tests, migrations).
func (db *DB) SQL() *sql.DB {
	return db.sql
}

// WithTx runs fn inside a transaction with the writer lock held.
func (db *DB) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (db *DB) migrate() error {
	// Ensure migrations table exists even on empty DBs before version checks.
	if _, err := db.sql.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version     INTEGER PRIMARY KEY,
			applied_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		)`); err != nil {
		return fmt.Errorf("schema_migrations: %w", err)
	}

	current, err := db.currentVersion()
	if err != nil {
		return err
	}

	// Fresh install: apply full schema.sql then stamp at SchemaVersion.
	if current == 0 {
		schema, err := schemaFS.ReadFile("schema.sql")
		if err != nil {
			return fmt.Errorf("read schema: %w", err)
		}
		if _, err := db.sql.Exec(string(schema)); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		// schema.sql may already insert nothing for migrations; stamp current.
		if err := db.recordVersion(SchemaVersion); err != nil {
			return err
		}
		return db.seedDefaultPolicy()
	}

	// Incremental upgrades.
	if current < 2 {
		if err := db.migrateV1ToV2(); err != nil {
			return fmt.Errorf("migrate v1→v2: %w", err)
		}
		if err := db.recordVersion(2); err != nil {
			return err
		}
	}

	if current > SchemaVersion {
		return fmt.Errorf("catalog schema version %d is newer than supported %d", current, SchemaVersion)
	}

	return db.seedDefaultPolicy()
}

func (db *DB) currentVersion() (int, error) {
	var v sql.NullInt64
	err := db.sql.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !v.Valid {
		return 0, nil
	}
	return int(v.Int64), nil
}

func (db *DB) recordVersion(v int) error {
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, v).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := db.sql.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, v)
	return err
}

// migrateV1ToV2 applies R2-8 incremental changes for catalogs created before
// actor_type / enroll_tokens UNIQUE / hashing_algorithm.
func (db *DB) migrateV1ToV2() error {
	// audit_events.actor_type
	if !db.columnExists("audit_events", "actor_type") {
		if _, err := db.sql.Exec(`
			ALTER TABLE audit_events ADD COLUMN actor_type TEXT NOT NULL DEFAULT 'system'`); err != nil {
			return fmt.Errorf("add actor_type: %w", err)
		}
	}

	// enroll_tokens.secret_hash uniqueness via index (works on existing tables).
	if _, err := db.sql.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_enroll_tokens_secret_hash_unique
		ON enroll_tokens(secret_hash)`); err != nil {
		return fmt.Errorf("unique enroll_tokens.secret_hash: %w", err)
	}

	// keystore.hashing_algorithm (R2-5 persistence).
	if !db.columnExists("keystore", "hashing_algorithm") {
		if _, err := db.sql.Exec(`
			ALTER TABLE keystore ADD COLUMN hashing_algorithm TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add hashing_algorithm: %w", err)
		}
	}
	return nil
}

func (db *DB) columnExists(table, column string) bool {
	rows, err := db.sql.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

func (db *DB) seedDefaultPolicy() error {
	var policies int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM policies`).Scan(&policies); err != nil {
		return err
	}
	if policies == 0 {
		_, err := db.sql.Exec(`
			INSERT INTO policies (
				id, name, schedule_cron, window_start, window_end,
				keep_last, keep_daily, keep_weekly, keep_monthly, keep_yearly,
				prune_grace_days, is_default
			) VALUES (
				'01DEFAULTPOLICY000000000000', 'Standard Server', '0 20 * * *', '20:00', '06:00',
				3, 14, 8, 12, 2, 7, 1
			)`)
		if err != nil {
			return fmt.Errorf("seed default policy: %w", err)
		}
	}
	return nil
}

// Ping checks database connectivity.
func (db *DB) Ping(ctx context.Context) error {
	return db.sql.PingContext(ctx)
}
