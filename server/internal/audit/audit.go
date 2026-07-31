// Package audit implements append-only, hash-chained audit events over the
// catalog's audit_events table (PLAN: Auditor component).
//
// Canonical encoding (compatibility surface — do not change without a migration):
//
//	row_hash = SHA-256( prev_hash || canonical )
//
// where canonical is the UTF-8 concatenation of these seven fields, each
// length-prefixed as decimal-len + ':' + raw bytes (no trailing delimiter),
// in this exact order:
//
//	id, ts, actor, actor_type, action, target, detail_json
//
// Example for id="ab", ts="t":  "2:ab1:t…"
//
// Length-prefixing is unambiguous even when a field contains ':' or newlines
// (S1-F2). The first commit (bc65f8a) used newline-terminated raw fields; that
// encoding was never deployed outside tests — no migration is required.
//
// The first row's prev_hash is the empty string "". Fields are the exact strings
// stored in the database (no JSON re-encoding of detail beyond the stored
// detail_json blob). row_hash and prev_hash are lowercase hex encodings of the
// 32-byte SHA-256 digest (64 hex chars).
//
// # Audit policy (M2 stage 2–5 — explicit)
//
// PLAN taxonomy: audit records ADMIN actions and security-boundary events.
// Scope of this package's interceptors and current emitters:
//
//   - Audited: machine.enroll (success + reject), auth.fail (unknown cert pin denials
//     on non-enroll methods), snapshot.commit (agent CommitSnapshot — first-class
//     backup completion, not per-chunk noise), restore.browse / restore.file (M4
//     RestoreService List/GetSnapshot and GetObject — one event per operation),
//     job.run_manual (POST /api/v1/jobs), catalog.rescan (POST /api/v1/rescan).
//   - NOT audited: agent heartbeats, control-channel traffic (Hello/JobProgress/…),
//     automatic job state transitions, inventory reports, per-chunk PutContents /
//     CheckContents, per-chunk GetContentRange (range reads are not first-class
//     restore events — the parent restore job / GetObject is).
//   - NOT audited (M2-S5 decision): read-only REST GETs on :8443
//     (/api/v1/machines, /jobs, /snapshots, /audit, /summary, /events). Auditing
//     every dashboard poll would drown the chain; list/read noise is not admin action.
//   - MUST audit: every mutating REST endpoint on :8443 (job submit/
//     cancel, policy change, enroll token mint, user/settings, forget/undelete/prune).
//     The job engine already stores `initiator` in params_json so those events can
//     name the actor without a schema change.
//
// Placeholder Unary/Stream interceptors remain pass-through for gRPC method-level
// audit. Do not log agent Channel messages as audit rows.
//
// Decision (M2-S3): CommitSnapshot emits snapshot.commit with actor=machine id,
// target=snapshot id. No per-chunk audit.
package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ajthom90/breakwater/server/internal/catalog"
	"github.com/oklog/ulid/v2"
)

// Actor types (PLAN / schema).
const (
	ActorUser   = "user"
	ActorAgent  = "agent"
	ActorSystem = "system"
)

// Action names used by the server (subset of PLAN audit taxonomy).
const (
	ActionMachineEnroll  = "machine.enroll"
	ActionAuthFail       = "auth.fail"
	ActionSnapshotCommit = "snapshot.commit" // M2-S3: agent CommitSnapshot (not per-chunk)
	// Restores are first-class audit events (PLAN taxonomy). One event per
	// browse/list or file GetObject — NOT per GetContentRange chunk.
	ActionRestoreBrowse = "restore.browse"
	ActionRestoreFile   = "restore.file"
	// Mutating :8443 (M4): job submit + catalog rescan.
	ActionJobRunManual  = "job.run_manual"
	ActionCatalogRescan = "catalog.rescan"
)

// Event is an audit event to append.
type Event struct {
	Actor     string
	ActorType string
	Action    string
	Target    string
	// Detail is JSON-encoded into detail_json. Prefer small, structured maps.
	// nil becomes "{}".
	Detail map[string]any
}

// Record is a stored audit row (for verification / queries).
type Record struct {
	ID        string
	TS        string
	Actor     string
	ActorType string
	Action    string
	Target    string
	Detail    string // raw detail_json
	PrevHash  string
	RowHash   string
}

// Writer appends hash-chained audit events via the catalog's single-writer path.
type Writer struct {
	db *catalog.DB
}

// NewWriter constructs an audit Writer over the catalog.
func NewWriter(db *catalog.DB) *Writer {
	return &Writer{db: db}
}

// Append writes one event, chaining row_hash from the previous tip.
// Concurrent Appends are safe: catalog.WithTx serializes writers.
func (w *Writer) Append(ctx context.Context, ev Event) error {
	if w == nil || w.db == nil {
		return fmt.Errorf("audit: nil writer")
	}
	if ev.Action == "" {
		return fmt.Errorf("audit: action required")
	}
	if ev.ActorType == "" {
		ev.ActorType = ActorSystem
	}

	detailJSON := "{}"
	if ev.Detail != nil {
		b, err := json.Marshal(ev.Detail)
		if err != nil {
			return fmt.Errorf("audit: marshal detail: %w", err)
		}
		detailJSON = string(b)
	}

	entropy := ulid.Monotonic(rand.Reader, 0)
	id := ulid.MustNew(ulid.Timestamp(time.Now()), entropy).String()
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	return w.db.WithTx(ctx, func(tx *sql.Tx) error {
		prevHash, err := tipHash(tx)
		if err != nil {
			return err
		}
		rowHash := ComputeRowHash(prevHash, id, ts, ev.Actor, ev.ActorType, ev.Action, ev.Target, detailJSON)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO audit_events (id, ts, actor, actor_type, action, target, detail_json, prev_hash, row_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, ts, ev.Actor, ev.ActorType, ev.Action, ev.Target, detailJSON, prevHash, rowHash,
		)
		return err
	})
}

// tipHash returns the latest row_hash, or "" if the table is empty.
func tipHash(tx *sql.Tx) (string, error) {
	var h sql.NullString
	err := tx.QueryRow(`SELECT row_hash FROM audit_events ORDER BY rowid DESC LIMIT 1`).Scan(&h)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !h.Valid {
		return "", nil
	}
	return h.String, nil
}

// ComputeRowHash implements the canonical chain hash (exported for tests/docs).
func ComputeRowHash(prevHash, id, ts, actor, actorType, action, target, detailJSON string) string {
	canonical := CanonicalEncoding(id, ts, actor, actorType, action, target, detailJSON)
	sum := sha256.Sum256(append([]byte(prevHash), []byte(canonical)...))
	return hex.EncodeToString(sum[:])
}

// CanonicalEncoding returns the exact bytes hashed after prev_hash.
// Documented compatibility surface — see package comment.
// Each field is encoded as "<decimal-byte-len>:<field-bytes>" (S1-F2).
func CanonicalEncoding(id, ts, actor, actorType, action, target, detailJSON string) string {
	return lengthPrefixed(id) +
		lengthPrefixed(ts) +
		lengthPrefixed(actor) +
		lengthPrefixed(actorType) +
		lengthPrefixed(action) +
		lengthPrefixed(target) +
		lengthPrefixed(detailJSON)
}

func lengthPrefixed(s string) string {
	return fmt.Sprintf("%d:%s", len(s), s)
}

// ChainBreak describes where VerifyChain found a mismatch.
type ChainBreak struct {
	Index    int    // 0-based position in chain order
	ID       string // row id at the break
	Expected string // recomputed row_hash
	Actual   string // stored row_hash
	PrevOK   bool   // whether prev_hash linked to previous row
	Reason   string
}

func (b *ChainBreak) Error() string {
	if b == nil {
		return "audit: nil chain break"
	}
	return fmt.Sprintf("audit chain break at index %d id=%s: %s (expected row_hash=%s actual=%s)",
		b.Index, b.ID, b.Reason, b.Expected, b.Actual)
}

// VerifyChain re-walks all audit_events in insertion order and validates the hash chain.
// Returns nil if valid, or *ChainBreak pinpointing the first failure.
func (w *Writer) VerifyChain(ctx context.Context) error {
	if w == nil || w.db == nil {
		return fmt.Errorf("audit: nil writer")
	}
	rows, err := w.db.SQL().QueryContext(ctx, `
		SELECT id, ts, actor, actor_type, action, target, detail_json, prev_hash, row_hash
		FROM audit_events
		ORDER BY rowid ASC`)
	if err != nil {
		return fmt.Errorf("audit: query chain: %w", err)
	}
	defer rows.Close()

	prev := ""
	i := 0
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.TS, &rec.Actor, &rec.ActorType, &rec.Action, &rec.Target, &rec.Detail, &rec.PrevHash, &rec.RowHash); err != nil {
			return err
		}
		if rec.PrevHash != prev {
			return &ChainBreak{
				Index:    i,
				ID:       rec.ID,
				Expected: prev,
				Actual:   rec.PrevHash,
				PrevOK:   false,
				Reason:   "prev_hash does not match previous row_hash",
			}
		}
		want := ComputeRowHash(rec.PrevHash, rec.ID, rec.TS, rec.Actor, rec.ActorType, rec.Action, rec.Target, rec.Detail)
		if want != rec.RowHash {
			return &ChainBreak{
				Index:    i,
				ID:       rec.ID,
				Expected: want,
				Actual:   rec.RowHash,
				PrevOK:   true,
				Reason:   "row_hash mismatch",
			}
		}
		prev = rec.RowHash
		i++
	}
	return rows.Err()
}

// ListByAction returns audit rows for an action (tests / admin).
func (w *Writer) ListByAction(ctx context.Context, action string) ([]Record, error) {
	rows, err := w.db.SQL().QueryContext(ctx, `
		SELECT id, ts, actor, actor_type, action, target, detail_json, prev_hash, row_hash
		FROM audit_events WHERE action = ? ORDER BY rowid ASC`, action)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var rec Record
		if err := rows.Scan(&rec.ID, &rec.TS, &rec.Actor, &rec.ActorType, &rec.Action, &rec.Target, &rec.Detail, &rec.PrevHash, &rec.RowHash); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}
