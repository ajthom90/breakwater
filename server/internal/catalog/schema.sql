-- Breakwater catalog schema (SQLite WAL).
-- System of record for policy/users/audit; rebuildable index for snapshots
-- (bwctl rescan rebuilds from in-repo manifests). ULID keys throughout.
-- Chunk→pack indexes live in the vault (kopia), NEVER here.
--
-- Schema version tracked in schema_migrations.

PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    applied_at  TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS machines (
    id              TEXT PRIMARY KEY,          -- ULID
    cert_fp         TEXT NOT NULL UNIQUE,      -- SHA-256 hex of agent client cert
    hostname        TEXT NOT NULL DEFAULT '',
    os_info         TEXT NOT NULL DEFAULT '',
    agent_version   TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'enrolled', -- enrolled|active|disabled|removed
    repo_id         TEXT NOT NULL UNIQUE,      -- equals machine id typically
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_seen_at    TEXT
);

CREATE TABLE IF NOT EXISTS machine_inventory (
    machine_id      TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL,             -- volume|vm
    external_id     TEXT NOT NULL,
    name            TEXT NOT NULL DEFAULT '',
    details_json    TEXT NOT NULL DEFAULT '{}',
    rct_capable     INTEGER NOT NULL DEFAULT 0,
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (machine_id, kind, external_id)
);

CREATE TABLE IF NOT EXISTS policies (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    schedule_cron   TEXT NOT NULL DEFAULT '0 20 * * *',
    window_start    TEXT NOT NULL DEFAULT '20:00',
    window_end      TEXT NOT NULL DEFAULT '06:00',
    throttle_bps    INTEGER NOT NULL DEFAULT 0, -- 0 = unlimited
    keep_last       INTEGER NOT NULL DEFAULT 3,
    keep_hourly     INTEGER NOT NULL DEFAULT 0,
    keep_daily      INTEGER NOT NULL DEFAULT 14,
    keep_weekly     INTEGER NOT NULL DEFAULT 8,
    keep_monthly    INTEGER NOT NULL DEFAULT 12,
    keep_yearly     INTEGER NOT NULL DEFAULT 2,
    prune_grace_days INTEGER NOT NULL DEFAULT 7,
    app_aware       INTEGER NOT NULL DEFAULT 0,
    is_default      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS machine_policies (
    machine_id      TEXT NOT NULL REFERENCES machines(id) ON DELETE CASCADE,
    policy_id       TEXT NOT NULL REFERENCES policies(id),
    PRIMARY KEY (machine_id)
);

CREATE TABLE IF NOT EXISTS jobs (
    id              TEXT PRIMARY KEY,
    machine_id      TEXT REFERENCES machines(id),
    type            TEXT NOT NULL, -- file|image|hyperv|restore|prune|verify|replicate|update
    state           TEXT NOT NULL DEFAULT 'pending', -- pending|running|cancelling|success|failed|cancelled
    started_at      TEXT,
    finished_at     TEXT,
    bytes_read      INTEGER NOT NULL DEFAULT 0,
    bytes_stored    INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT NOT NULL DEFAULT '',
    log_ref         TEXT NOT NULL DEFAULT '',
    params_json     TEXT NOT NULL DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_jobs_machine ON jobs(machine_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_jobs_state ON jobs(state);

CREATE TABLE IF NOT EXISTS snapshots (
    id              TEXT PRIMARY KEY,          -- ULID (catalog id)
    machine_id      TEXT NOT NULL REFERENCES machines(id),
    kind            TEXT NOT NULL,             -- file|image|hyperv
    source          TEXT NOT NULL DEFAULT '',
    manifest_ref    TEXT NOT NULL,             -- kopia manifest ID
    root_object_id  TEXT NOT NULL DEFAULT '',
    gfs_tags        TEXT NOT NULL DEFAULT '',  -- comma-separated tags
    verify_state    TEXT NOT NULL DEFAULT 'none', -- none|ok|failed|partial
    deleted_at      TEXT,                      -- soft-delete for prune grace
    job_id          TEXT REFERENCES jobs(id),
    bytes_read      INTEGER NOT NULL DEFAULT 0,
    bytes_stored    INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_snapshots_machine ON snapshots(machine_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_snapshots_deleted ON snapshots(deleted_at);

CREATE TABLE IF NOT EXISTS users (
    id              TEXT PRIMARY KEY,
    username        TEXT NOT NULL UNIQUE,
    password_hash   TEXT NOT NULL,             -- argon2id
    totp_secret_enc TEXT,                      -- encrypted; null until enrolled
    role            TEXT NOT NULL DEFAULT 'admin', -- MVP: admin only
    locked_until    TEXT,
    failed_logins   INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS audit_events (
    id              TEXT PRIMARY KEY,
    ts              TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    actor           TEXT NOT NULL DEFAULT '',  -- user id or system
    actor_type      TEXT NOT NULL DEFAULT 'system', -- user|agent|system (PLAN)
    action          TEXT NOT NULL,             -- e.g. machine.enroll
    target          TEXT NOT NULL DEFAULT '',
    detail_json     TEXT NOT NULL DEFAULT '{}',
    prev_hash       TEXT NOT NULL DEFAULT '',
    row_hash        TEXT NOT NULL              -- SHA-256 chain
);

CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_events(ts);
CREATE INDEX IF NOT EXISTS idx_audit_action ON audit_events(action);

CREATE TABLE IF NOT EXISTS enroll_tokens (
    id              TEXT PRIMARY KEY,
    secret_hash     TEXT NOT NULL UNIQUE,      -- hash of secret portion
    expires_at      TEXT NOT NULL,
    used_at         TEXT,
    created_by      TEXT NOT NULL DEFAULT '',
    machine_id      TEXT,                      -- set when consumed
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_enroll_tokens_secret ON enroll_tokens(secret_hash);

CREATE TABLE IF NOT EXISTS api_tokens (
    id              TEXT PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            TEXT NOT NULL DEFAULT '',
    token_hash      TEXT NOT NULL UNIQUE,
    expires_at      TEXT,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    last_used_at    TEXT
);

CREATE TABLE IF NOT EXISTS settings (
    key             TEXT PRIMARY KEY,
    value           TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS replication_peers (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    endpoint        TEXT NOT NULL,             -- host:port
    role            TEXT NOT NULL DEFAULT 'push', -- push|pull (MVP push)
    cert_fp         TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending',
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE TABLE IF NOT EXISTS replication_state (
    peer_id         TEXT NOT NULL REFERENCES replication_peers(id) ON DELETE CASCADE,
    machine_id      TEXT NOT NULL,
    cursor          TEXT NOT NULL DEFAULT '',
    lag_seconds     INTEGER NOT NULL DEFAULT 0,
    updated_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (peer_id, machine_id)
);

CREATE TABLE IF NOT EXISTS keystore (
    repo_id             TEXT PRIMARY KEY,      -- machine repo id
    repo_password_enc   BLOB NOT NULL,
    hashing_key_enc     BLOB NOT NULL,
    hashing_algorithm   TEXT NOT NULL DEFAULT '', -- e.g. BLAKE2B-256-128 (R2-5)
    created_at          TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
