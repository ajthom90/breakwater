# Breakwater M1 Code Review — Round 2

**Reviewed:** commit `755f417` ("fix(M1): review blockers — mark-sweep prune, TLS peer enroll, hashing key"), i.e. the diff `f92837f..755f417`, on 2026-07-30.
**Method:** multi-agent adversarial review (48 agents: independent finders per correctness angle, then an independent verifier per finding). **All 15 findings below were CONFIRMED by verification — none are speculative.**
**Prior context:** `REVIEW-M1.md` (round 1) and `PLAN.md` (design source of truth). This round reviewed the commit that claimed to fix round 1's B1/B2/H1.

## Verdict

**M1 must not close on this commit.** B2 (TLS-peer-bound enrollment) held up. But the B1 fix replaced a harmless no-op prune with a **latent live-data destroyer**: the mark phase only understands flat snapshots (the only shape the tests exercise), and there is no protection for in-flight backups. Two independent paths to permanent loss of *live* backup data exist (R2-1, R2-2), plus a third for any future snapshot kind (R2-3). The H1 hashing-key fix is also incomplete — the algorithm name is dropped at the wire boundary (R2-5), which recreates the exact M2 failure H1 was meant to prevent.

Fix order matters: **write the failing tests first** (R2-13/14/15 describe exactly the tests whose absence let this through), then fix the prune core, then the rest.

---

## Blockers — prune can destroy live data

### R2-1 · Mark phase never walks tree/image manifest references — first real backup loses everything
`server/internal/vault/kopia.go:533` (also :552)
`markLiveContents` runs `VerifyObject` only on each snapshot's `RootObjectID`. PLAN.md specifies mark = "walk live snapshot records → **trees/manifests** → VerifyObject → live content-ID set". The moment a snapshot uses the real Breakwater format (M2 `PutTreeObject`/`PutImageManifest`: a root `TreeObject` JSON whose `TreeEntry.ObjectID`s point at file objects, child trees, and ADS objects; or an `ImageManifest` whose blocks reference contents), `VerifyObject(root)` marks only the contents backing the root JSON blob. Every indirectly-referenced content of every **live** snapshot is unmarked → `DeleteContent`'d → permanently purged under `SafetyNone`. Both new prune tests pass only because they use flat roots where the root object IS the data.
**Fix:** implement a recursive marker in the vault keyed by snapshot kind: file kind → decode `TreeObject` (import `pkg/format` — it's our own shared module), mark the root object's contents, then recurse into every `TreeEntry.ObjectID` (dirs recurse, files/ADS mark via `VerifyObject`); image kind → decode `ImageManifest`, mark root contents plus every `ImageBlockRef.ContentID`. Any decode failure = fail the prune (fail closed).
**Test first (see R2-15):** build a snapshot with a real root `TreeObject` referencing separate file objects (and one with an `ImageManifest`), prune, assert every indirect object still opens and checksum-verifies.

### R2-2 · Sweep has no minimum-age or in-flight-backup guard — racing backup is silently destroyed
`server/internal/vault/kopia.go:487` (also :453, :520)
A multi-RPC backup uploads contents across many `PutContent`/`WriteObject` calls, releasing the vault RLock between calls (and `HasContents` may return true, telling the agent to skip re-upload). If `Prune` takes the exclusive lock between two RPCs, the just-uploaded contents are referenced by no manifest yet → swept. The agent then commits its snapshot record successfully; it references content IDs that no longer exist. Backup reports success; restore fails. The code comment defers only the *reader* (OpenObject) race to M2 — the *writer* race is undocumented and live.
**Fix (both):** (a) add a minimum-age cutoff to the sweep — never delete a content whose kopia timestamp is younger than a configurable window (default ≥ 24h; this is also kopia's own safety philosophy that `SafetyNone` opted out of); (b) document on the `Vault` interface that Prune must never run concurrently with an open backup session on the same repo, and track it so M2's scheduler enforces per-repo backup-vs-prune serialization structurally.
**Test first:** simulate the interleaving — write contents, run Prune *before* `PutSnapshotRecord`, then commit the record and assert the object still restores (with min-age guard this passes).

### R2-3 · Unknown snapshot kinds: record kept, data swept
`server/internal/vault/kopia.go:535` (also `PutSnapshotRecord` :344)
`markLiveContents` hardcodes `bw-file-snapshot` and `bw-image-snapshot`; `PutSnapshotRecord` accepts any non-empty `Kind`. A record stored under any other kind stays listable while the next Prune deletes every content behind it.
**Fix:** `PutSnapshotRecord` rejects kinds outside the known set (validation at the write boundary), AND `markLiveContents` enumerates **all** manifests carrying a `RootObjectID` and fails the prune if it encounters a kind it cannot walk (fail closed, belt and suspenders).

### R2-4 · One bad snapshot record wedges Prune for the whole vault
`server/internal/vault/kopia.go:550` (also :543, :553)
`markLiveContents` hard-fails on `object.ParseID`/`GetManifest`/`VerifyObject` errors and Prune returns before sweeping — while `PutSnapshotRecord` accepts any unparsed `RootObjectID` string. One record with `RootObjectID: "not-an-oid"` (or a corrupted root) permanently disables retention on that repo.
**Fix:** validate `RootObjectID` with `object.ParseID` at `PutSnapshotRecord` time (reject garbage at the write boundary). Keep the mark phase fail-closed — that instinct is correct — but make the error identify the offending manifest ID so an operator can act on it.

## High priority

### R2-5 · H1 incomplete: HashingAlgorithm dropped at the gateway; never on the wire, never persisted
`server/internal/agentgw/gateway.go:221` (struct at :191-196), plus keystore
`enroll.Service.Enroll` now returns `HashingAlgorithm`, but the gateway response struct drops it, the proto `EnrollResponse` has no field for it, and the keystore persists only the secret. An M2 agent gets the HMAC secret but must *guess* the hash function — a wrong guess (different repo config, kopia default change) makes every agent-computed ID mismatch: dedup dead, every upload rejected, unrecoverable after enrollment.
**Fix:** add `string hashing_algorithm = 5;` to `EnrollResponse` in `proto/breakwater/v1/breakwater.proto` (additive — allowed under the freeze), thread it through the gateway struct/handler, and persist it alongside the hashing key in the keystore so it is re-fetchable.

### R2-6 · `GetHashingKey` returns a sealed-empty placeholder as a valid key
`server/internal/keystore/keystore.go:71` (also :130)
`CreateRepoPassword` seals an empty `[]byte` placeholder; if `SetHashingKey` never runs (nil `Vaults` wiring, or a failure between the two steps), `GetHashingKey` returns `([]byte{}, nil)`. A consumer computing HMACs with an empty secret silently mismatches everything.
**Fix:** return a distinct `ErrHashingKeyNotSet` when the unsealed value is empty; add a test.

### R2-7 · Nightly 10GB engine gate is dead CI config
`.github/workflows/ci.yml:88` (`on:` block at :3)
The `engine-gate-full` job is conditioned on `schedule`/`workflow_dispatch` events, but the workflow only declares `push` and `pull_request` — the full gate can never run, not even manually. A multi-GiB-scale regression merges green.
**Fix:** add to `on:`: a `schedule:` cron (nightly) and `workflow_dispatch:`. Verify the job actually triggers once.

### R2-8 · Schema changes silently skipped on existing databases
`server/internal/catalog/schema.sql:131` (also :118), `catalog.go migrate()`
The new UNIQUE on `enroll_tokens.secret_hash` and the `audit_events.actor_type` column live only inside `CREATE TABLE IF NOT EXISTS`; `migrate()` just replays schema.sql, so a catalog created at `f92837f` never gets them — fresh and upgraded installs diverge (`no such column` only on upgraded boxes; token-hash uniqueness unenforced).
**Fix:** introduce real incremental migrations now (v1→v2: `ALTER TABLE audit_events ADD COLUMN actor_type ...`; `CREATE UNIQUE INDEX IF NOT EXISTS ... ON enroll_tokens(secret_hash)` — index form works for existing tables), applied by version from `schema_migrations`. Add an upgrade test: build a v1 database fixture, open with current code, assert the column and index exist. This pattern is load-bearing for every future milestone.

### R2-9 · Enrollment burns the single-use token before fallible steps, no rollback
`server/internal/enroll/service.go:103`
`ConsumeEnrollToken` → then `Vaults.Create`/`SetHashingKey`/`InsertMachine` can fail (disk full, perms): token permanently burned, agent retry rejected, orphaned keystore row + on-disk repo under a ULID with no machines row.
**Fix:** compensate on failure — deferred cleanup that un-consumes the token (`used_at = NULL, machine_id = NULL`) and removes the keystore row if any post-consume step fails; log any orphaned repo dir for cleanup. Add a failure-injection test (vault creator that errors) asserting the token is reusable afterward.

## Medium

### R2-10 · Manager cache returns a closed vault forever
`server/internal/vault/vault.go:159`
`kopiaVault.Close` + the new `requireOpen` guards poison the cached instance: `Manager.Open`/`Create` keep returning it (only `CloseAll` evicts). One `Close` through the interface bricks that machine's vault until process restart.
**Fix:** eviction on close (Manager-owned `Close(repoID)` or Open/Create re-opening when the cached instance is closed).

### R2-11 · All enroll errors → `InvalidArgument` with raw internal text
`server/internal/agentgw/gateway.go:219`
DB/keystore/vault failures reach unauthenticated clients as `InvalidArgument "enroll: create vault: repo.Initialize: /repos/<ulid>..."` — agents treat retryable server faults as permanent, and internal paths leak on the pre-enrollment surface.
**Fix:** typed errors from `enroll.Service` (token-invalid/expired/used → `InvalidArgument`/`PermissionDenied`; everything else → `Internal` with generic message, details to the server log only).

### R2-12 · `ListSnapshotRecords` swallows manifest read errors via shadowed `err`
`server/internal/vault/kopia.go:416`
A failed `GetManifest` inside the if-condition silently falls back to kopia's `ModTime` as `Timestamp` — wrong times in listings, and M2's retention engine keyed on `Timestamp` would evaluate age against the wrong instant with no surfaced error.
**Fix:** propagate the error (or at minimum mark the entry degraded and log); never silently substitute.

## Test gaps (write these FIRST — they are why round 2 was needed)

### R2-13 · Reclamation tests prove index shrinkage, not disk reclamation
`server/internal/vault/prune_reclaim_test.go:86` (also kopia_test.go gate)
`Stats` derives from index entries; if blob-layer GC regresses, tests stay green while pack files accumulate until the volume fills.
**Fix:** assert actual on-disk bytes (walk the repo dir summing file sizes) shrink materially after prune, alongside the existing index assertions.

### R2-14 · H1's core invariant untested: nothing proves the agent can reproduce server content IDs
`server/internal/vault/kopia_test.go:52`
Tests check only `len(hk) > 0` / `algo != ""` / stored==returned.
**Fix:** round-trip test — using ONLY the enrollment-returned (algorithm, secret), construct kopia's hash function (`hashing.CreateHashFunc` with those parameters), hash a known payload, and assert equality with the content ID `PutContent` returns for the same payload. This single test locks the entire have/want contract.

### R2-15 · No prune test models indirect references or image snapshots
`server/internal/vault/prune_reclaim_test.go:13`
No test stores a `bw-image-snapshot`, and none uses a root that *references* other objects — precisely the shapes R2-1/R2-3 destroy.
**Fix:** add prune-survival tests: (a) live file snapshot whose root `TreeObject` references separate file objects and a child tree (assert all objects restore post-prune); (b) live image snapshot with an `ImageManifest` referencing block contents (assert all blocks readable post-prune); (c) a forgotten indirect snapshot IS reclaimed. These must fail against `755f417` before the R2-1 fix, then pass.

---

## Required fix order

1. **Tests first:** R2-15 (indirect + image prune-survival — must fail on current code), R2-13 (on-disk bytes), R2-14 (ID reproduction).
2. **Prune core:** R2-1 (recursive mark), R2-2 (min-age guard + serialization contract), R2-3 (kind validation + fail-closed enumeration), R2-4 (RootObjectID validation at write).
3. **H1 completion:** R2-5 (wire + persist algorithm), R2-6 (ErrHashingKeyNotSet).
4. **Infra:** R2-7 (CI on: block), R2-8 (real migrations + upgrade test).
5. **Robustness:** R2-9, R2-10, R2-11, R2-12.
6. Update `PROGRESS.md` and `REVIEW-M1.md` checkboxes honestly; the engine gate may be marked PASSED only when the new R2-15/R2-13 assertions pass at full 10GB size.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore        # nothing
cd server
go vet ./...
go test ./... -count=1 -short -race -timeout 10m
# New prune-survival tests green, including indirect-reference and image kinds:
go test ./internal/vault/ -count=1 -run 'Prune|Reclaim' -v
# Reduced gate with on-disk-bytes assertion:
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
# Full gate before closing M1:
go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -timeout 45m -v
# Migration upgrade test green; CI workflow file shows schedule + workflow_dispatch in `on:`.
cd ../pkg && go test ./... -count=1
```

**Standing rules unchanged:** kopia imports confined to `server/internal/vault` (importing `pkg/format` into vault for the recursive mark is fine — it's Breakwater's own module); no AGPL/GPL library deps; no destructive RPCs on :9443; no invented storage format; no new one-way doors without stopping for human input.
