package catalog_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/ajthom90/breakwater/server/internal/catalog"
)

// v1Schema is a minimal pre-R2-8 catalog (no actor_type, no hashing_algorithm,
// enroll_tokens.secret_hash not UNIQUE). Used as an upgrade fixture (R2-8).
const v1Schema = `
PRAGMA foreign_keys = ON;

CREATE TABLE schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE machines (
    id TEXT PRIMARY KEY,
    cert_fp TEXT NOT NULL UNIQUE,
    hostname TEXT NOT NULL DEFAULT '',
    os_info TEXT NOT NULL DEFAULT '',
    agent_version TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'enrolled',
    repo_id TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at TEXT
);

CREATE TABLE policies (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    schedule_cron TEXT NOT NULL DEFAULT '0 20 * * *',
    window_start TEXT NOT NULL DEFAULT '20:00',
    window_end TEXT NOT NULL DEFAULT '06:00',
    throttle_bps INTEGER NOT NULL DEFAULT 0,
    keep_last INTEGER NOT NULL DEFAULT 3,
    keep_hourly INTEGER NOT NULL DEFAULT 0,
    keep_daily INTEGER NOT NULL DEFAULT 14,
    keep_weekly INTEGER NOT NULL DEFAULT 8,
    keep_monthly INTEGER NOT NULL DEFAULT 12,
    keep_yearly INTEGER NOT NULL DEFAULT 2,
    prune_grace_days INTEGER NOT NULL DEFAULT 7,
    app_aware INTEGER NOT NULL DEFAULT 0,
    is_default INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE audit_events (
    id TEXT PRIMARY KEY,
    ts TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor TEXT NOT NULL DEFAULT '',
    action TEXT NOT NULL,
    target TEXT NOT NULL DEFAULT '',
    detail_json TEXT NOT NULL DEFAULT '{}',
    prev_hash TEXT NOT NULL DEFAULT '',
    row_hash TEXT NOT NULL
);

CREATE TABLE enroll_tokens (
    id TEXT PRIMARY KEY,
    secret_hash TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at TEXT,
    created_by TEXT NOT NULL DEFAULT '',
    machine_id TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE keystore (
    repo_id TEXT PRIMARY KEY,
    repo_password_enc BLOB NOT NULL,
    hashing_key_enc BLOB NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO schema_migrations(version) VALUES (1);
`

func TestUpgradeFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")

	// Build a v1 database fixture without going through current Open().
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(v1Schema); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	// Confirm pre-upgrade state.
	if columnExists(t, raw, "audit_events", "actor_type") {
		t.Fatal("v1 fixture should not have actor_type")
	}
	if columnExists(t, raw, "keystore", "hashing_algorithm") {
		t.Fatal("v1 fixture should not have hashing_algorithm")
	}
	_ = raw.Close()

	// Open with current code — must apply v1→v2.
	db, err := catalog.Open(path)
	if err != nil {
		t.Fatalf("Open upgraded: %v", err)
	}
	defer db.Close()

	sq := db.SQL()
	if !columnExists(t, sq, "audit_events", "actor_type") {
		t.Fatal("after upgrade: missing audit_events.actor_type")
	}
	if !columnExists(t, sq, "keystore", "hashing_algorithm") {
		t.Fatal("after upgrade: missing keystore.hashing_algorithm")
	}

	// UNIQUE index on enroll_tokens.secret_hash must exist.
	var idxCount int
	err = sq.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND tbl_name = 'enroll_tokens'
		  AND (sql LIKE '%secret_hash%' OR name LIKE '%secret%')`).Scan(&idxCount)
	if err != nil || idxCount < 1 {
		t.Fatalf("expected unique index on enroll_tokens.secret_hash, count=%d err=%v", idxCount, err)
	}

	var ver int
	if err := sq.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&ver); err != nil {
		t.Fatal(err)
	}
	if ver != catalog.SchemaVersion {
		t.Fatalf("schema version %d, want %d", ver, catalog.SchemaVersion)
	}
	t.Logf("v1→v%d upgrade OK", ver)
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	return false
}
