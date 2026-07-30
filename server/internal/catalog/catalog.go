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

// SchemaVersion is the current migration version (monolithic v1 for M1).
const SchemaVersion = 1

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
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}
	if _, err := db.sql.Exec(string(schema)); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	var n int
	err = db.sql.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, SchemaVersion).Scan(&n)
	if err != nil {
		return fmt.Errorf("check migration: %w", err)
	}
	if n == 0 {
		if _, err := db.sql.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, SchemaVersion); err != nil {
			return fmt.Errorf("record migration: %w", err)
		}
	}
	// Seed default policy if missing.
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
