// Package retention implements GFS keep-set math, the forget/prune split with
// a 7-day soft-delete grace window (ransomware undo layer), and the pure
// deterministic keep-set function property-tested under random sequences.
//
// # Fail closed
//
// M5 is the first code that deletes customer backups. Prefer keeping too much
// over deleting anything not certain to be expired. The keep-set function never
// reads time.Now(); callers inject now. Soft-deleted snapshots inside the grace
// window are LIVE for vault mark-and-sweep.
//
// # Bucketing rules (explicit — ambiguity here hides retention bugs)
//
// ComputeKeepSet(snapshots, policy, now) returns which snapshot IDs to KEEP.
// Everything else is a forget candidate (soft-delete only; not physical).
//
//  1. Input: each snapshot has a stable ID and a Timestamp (catalog created_at /
//     vault snapshot time). Timestamps are interpreted in UTC for bucketing.
//  2. Sort: newest-first by Timestamp; ties broken by ID descending
//     (lexicographic) for determinism.
//  3. keep-last N: the N newest snapshots are always kept (N<=0 means none from
//     this rule alone). The absolute newest snapshot is ALWAYS kept even when
//     keep-last is 0 — safety property: never forget the tip.
//  4. GFS tiers (hourly / daily / weekly / monthly / yearly): for each tier
//     with count K > 0, consider the K most recent non-empty buckets whose
//     bucket start is ≤ now. Bucket boundaries (UTC):
//     - hourly:  truncated to the hour (YYYY-MM-DD HH:00:00Z)
//     - daily:   calendar day (YYYY-MM-DD 00:00:00Z)
//     - weekly:  Monday 00:00:00Z of the ISO week (Go: weekday Monday start)
//     - monthly: first day of month 00:00:00Z
//     - yearly:  January 1 00:00:00Z
//  5. Bucket winner: within a bucket the NEWEST snapshot wins (one per bucket).
//     Older snapshots in the same bucket are not selected by that tier.
//  6. Empty buckets: a bucket with no snapshot is simply skipped — it does not
//     steal a slot from an older filled bucket. We walk snapshots newest-first
//     and fill up to K distinct bucket keys; we do not invent placeholder times.
//  7. Union: a snapshot is kept if ANY rule selects it (keep-last or any GFS tier).
//  8. Idempotence: applying the same (snapshots, policy, now) twice yields the
//     same keep set. Applying retention (forget the complement) twice changes
//     nothing after the first pass.
//  9. Snapshots with Timestamp after now are treated as present (clock skew);
//     they participate in keep-last and bucketing normally (fail closed: keep).
//
// # Forget vs prune (ransomware layer)
//
//   - Forget: sets catalog deleted_at. Does NOT touch the vault. Reversible via
//     Undelete while deleted_at + grace > now (default grace 7 days from policy).
//   - Prune: only snapshots with deleted_at ≤ now−grace become eligible. For
//     those, vault DeleteSnapshotRecord removes the manifest; then vault.Prune
//     reclaims unreferenced content. Soft-deleted-but-within-grace manifests
//     remain in the vault and are LIVE for mark-and-sweep.
//
// # Grace vs min-content-age composition
//
// Two independent safety layers — BOTH must allow deletion for content to go:
//
//   - Catalog grace (default 7d): soft-deleted snapshots stay LIVE in the vault
//     until deleted_at + grace ≤ now. Within grace, prune does not remove their
//     manifests; mark still walks them.
//   - Content min-age (R2-2, default 24h): vault.Prune never DeleteContent on
//     contents younger than MinContentAge; maintenance SafetyParameters derive
//     from the same window (R3-5).
//
// Composition: a forgotten snapshot past grace may still have young shared
// contents protected by min-age (dedup). Those contents survive until aged.
// Neither layer is weakened for tests in production defaults. Tests that must
// observe reclamation pass WithMinContentAge(0) and/or zero grace explicitly.
//
// # Server-side only
//
// No agent-reachable path may trigger forget or prune. The :9443 interceptor
// exposes no such RPC; Engine.Submit rejects server-only types; REST forget/
// prune live on :8443 only.
package retention
