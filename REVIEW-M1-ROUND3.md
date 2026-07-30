# Breakwater M1 Code Review — Round 3

**Reviewed:** commit `eea1a46` ("fix(M1): R2-1…R2-15 — recursive prune mark, min-age, enroll/H1/CI/migrations"), i.e. the diff `755f417..eea1a46`, on 2026-07-30.
**Method:** direct line-by-line review of the prune mark/sweep core plus two independent adversarial subreviews (enroll/keystore/gateway; catalog migrations). Key claims verified **empirically**: all round-2 verification commands re-run locally (all green, including the full 10 GiB gate, 152 s), and the new prune tests **mutation-tested** — reverting the recursive mark to flat-mark makes both survival tests fail; disabling the min-age guard makes the in-flight test fail. The commit's red-first evidence is corroborated.
**Prior context:** `REVIEW-M1.md` (round 1), `REVIEW-M1-ROUND2.md` (round 2), `PLAN.md`.

## Verdict

**Close, but not yet.** The round-2 fix set is real: recursive mark and min-age guard are correctly implemented and genuinely enforced by tests (mutation-verified), R2-14's have/want round-trip is legitimate, migrations are structurally sound, and the engine gate now asserts on-disk reclamation at 10 GiB. Twelve of fifteen round-2 findings are fully closed.

But three of the fixes have holes in exactly the failure modes they were written for: the mark phase contains a **fail-open heuristic** that the round-2 review explicitly prohibited (R3-1, empirically demonstrated), the enrollment compensation **no-ops on context cancellation** — the failure class most likely to co-occur with a slow/failing vault create (R3-3, mechanically confirmed against the pinned sqlite driver), and repos enrolled at `755f417` come out of the new migration with a real hashing key and a **silently empty algorithm** — R2-5's exact failure mode resurrected for the upgrade population (R3-4, found independently by both subreviews).

---

## High

### R3-1 · Mark phase has a fail-open heuristic — decode failure on a file-snapshot root can silently sweep every child
`server/internal/vault/kopia.go:643-654` (`markTreeObject`), `:684-710` (`looksLikeTreeJSON`, `bytesContains`, `indexBytes`)
Round 2 mandated: "Any decode failure = fail the prune (fail closed)." Instead, when `json.Unmarshal` of a file-kind root fails, `looksLikeTreeJSON` guesses: bytes that don't "look like" tree JSON are silently treated as a leaf — children unmarked, swept. Bytes that do look like it fail the prune. Both branches are wrong:
- **Fail-open:** any future file-kind root whose bytes fail decode without tripping the heuristic (format drift, truncated write, shape change in M2+) is silently leaf-treated and its entire subtree is reclaimed. Latent live-data loss in exactly the code this round exists to harden.
- **Content-dependent wedging (empirically demonstrated):** a *flat* root whose payload starts with `{` and contains the byte sequence `"v"` wedges Prune for the whole vault — `prune: decode TreeObject …: invalid character …`. Retention disabled by user-controlled backup content. For a multi-MB random flat root the `"v"` sequence is near-certain; only the 1/256 first-byte check keeps the current gate deterministic.
- PROGRESS.md's claim "decode failure / unknown kind → fail closed (R2-3)" is therefore overstated.

**Root cause:** the engine gate, `TestPruneReclaimsForgottenContent`, and `TestPruneMinAgeProtectsInFlightBackup` store **flat raw-bytes roots** under `bw-file-snapshot`, so one kind names two shapes and the mark phase must guess. Real backups (M2 `PutTreeObject`) always have TreeObject roots; flat roots are a test-only artifact.
**Fix:** eliminate the guess. (a) `PutSnapshotRecord` validates at the write boundary that the root object decodes as the kind's format (TreeObject for file, ImageManifest for image) — consistent with the R2-3/R2-4 write-boundary philosophy; (b) gate and all tests wrap payload objects in a real one-entry `TreeObject` root (the gate still writes/restores 10 GiB — the file object becomes a tree entry); (c) `markTreeObject` decode failure ALWAYS fails the prune, loudly, naming the manifest; (d) delete `looksLikeTreeJSON`, `bytesContains`, `indexBytes` (the latter two also reimplement `bytes.Contains`).
**Test first:** a file-kind snapshot whose root is undecodable-as-TreeObject raw bytes NOT starting with `{` → Prune must return an error. On `eea1a46` this test FAILS (Prune returns nil, root silently leaf-treated) — red-first. After the fix, `PutSnapshotRecord` rejects the record at write time and the prune-side check is defense in depth (construct the bad state via a direct manifest write in the test, or assert the write-time rejection).

### R3-2 · Mark phase `io.ReadAll`s every file-kind root — 10 GiB of RAM in the full gate, unbounded generally
`server/internal/vault/kopia.go:638` (`readObjectBytes` call), `:753-764`
`markTreeObject` reads the ENTIRE root object into memory before attempting decode. With today's flat 10 GiB gate root, Prune's mark phase materializes all 10 GiB in RAM (passes on this dev machine; marginal on a 16 GB CI runner for the nightly full gate that R2-7 just enabled; OOM for anything larger). Real tree/image-manifest roots are small JSON, so after R3-1's fix this shrinks to KBs — but nothing then enforces it.
**Fix:** cap root-object reads in the mark phase (e.g. 16 MiB via `io.LimitReader` + explicit over-limit error, fail closed, constant with rationale). Applies to both tree and image-manifest roots.

### R3-3 · Enrollment compensation runs on the request context — no-ops on cancellation/deadline, the failure class it was built for
`server/internal/enroll/service.go:125-142`
The compensating `defer` calls `ReleaseEnrollToken(ctx, …)` and `Keystore.DeleteRepo(ctx, …)` with the SAME `ctx` the RPC arrived with. Mechanically confirmed: pinned `modernc.org/sqlite@v1.34.5` implements neither `driver.ConnBeginTx` nor `ExecerContext`/`QueryerContext` (TODO comments in `sqlite.go:37-39`), so `database/sql`'s ctxutil shim returns `ctx.Err()` BEFORE executing anything when the context is already done. Scenario: agent sets an RPC deadline; `Vaults.Create` is slow (disk full — R2-9's own scenario); deadline expires; Enroll errors; compensation fires with a dead ctx → both compensating writes fail instantly (only logged). Token permanently burned + orphaned keystore row: R2-9's bug, reintroduced through its most likely trigger. The new `enroll_compensate_test.go` uses a plain error with a live ctx, so it cannot see this.
**Fix:** run compensation on a fresh context — `context.WithTimeout(context.Background(), 5*time.Second)` (idiom already used in `cmd/breakwaterd/main.go:131`).
**Test first:** failing vault whose `Create` cancels the request context (or a pre-expired ctx) → after failed Enroll, assert the token is reusable and the keystore row is gone. FAILS on `eea1a46` — red-first.

### R3-4 · Repos enrolled at 755f417 migrate to a real hashing key with a silently empty algorithm — R2-5's failure mode for the upgrade population
`server/internal/keystore/keystore.go:150-167`, `server/internal/catalog/catalog.go:167-173`
(Found independently by both subreviews.) `migrateV1ToV2` adds `hashing_algorithm TEXT NOT NULL DEFAULT ''` with no backfill. At `755f417`, `SetHashingKey` took no algorithm and wrote only the key — so every machine enrolled before `eea1a46` upgrades to a row with a REAL `hashing_key_enc` and `hashing_algorithm = ''`. `GetHashingKey` only guards `len(pt) == 0` → returns `(realKey, "", nil)`: no error, and the M2 agent is back to guessing the hash function. The upgrade test only checks the column exists.
**Fix:** in `GetHashingKey`, `len(pt) > 0 && algorithm == ""` → distinct `ErrHashingAlgorithmNotSet` (fail loud; a backfill-from-vault reconciliation may come in M2).
**Test first:** migration test seeds a v1-shaped row with a real sealed key and no algorithm → after upgrade, `GetHashingKey` returns the sentinel, not `(key, "", nil)`. FAILS on `eea1a46` — red-first.

## Medium

### R3-5 · Min-age guard stops at the index layer — maintenance still runs with blanket `SafetyNone`
`server/internal/vault/kopia.go:552-559`
`WithMinContentAge` filters only OUR `DeleteContent` calls; `DropDeletedContents`/`maintenance.Run` still get `SafetyNone` (`BlobDeleteMinAge=0`, `SessionExpirationAge=0`, all margins zero). Under today's invariants (single process, per-vault exclusive lock, per-call write sessions) committed contents are always index-referenced, so blob GC can't touch them — but the guard is documented as "a safety net for races the scheduler fails to prevent," and at the blob layer it is not (e.g. an unflushed session from a second repo handle — see R3-6). **Fix:** when `minContentAge > 0`, derive `maintenance.SafetyParameters` from it (`BlobDeleteMinAge`, `SessionExpirationAge` ≥ minAge, sane margins) instead of `SafetyNone`; keep `SafetyNone` only for the explicit `WithMinContentAge(0)` test path. Update the Vault interface comment to state precisely what the guard does and does not cover.

### R3-6 · `Manager.Close`/`Open` race can produce two live handles on one repo directory
`server/internal/vault/vault.go:263-274`
`Close(repoID)` evicts from the cache, releases the manager lock, THEN blocks in `v.Close` waiting for the vault's exclusive lock. A concurrent `Open` creates a second live handle while the first still has in-flight writes — prune on one handle concurrent with writes on the other is the R2-2 scenario, in-process. No current caller does this; M2's scheduler will be the enforcement point. **Fix now:** document the invariant on `Manager` (one live handle per repo; Close must not race Open) and add it to the M2 serialization work item. A structural fix (per-repo closing state) may wait for M2.

### R3-7 · Compensation test never proves the keystore row was deleted
`server/internal/enroll/enroll_compensate_test.go:78-98`
Each Enroll mints a fresh machine ULID, so the successful retry can never collide with the failed attempt's keystore row — the test only proves token release. **Fix:** capture the repoID seen by `failingVault.Create` and assert `SELECT COUNT(*) FROM keystore WHERE repo_id = ?` is 0 after the failed attempt (folds naturally into R3-3's new test).

### R3-8 · No test asserts gRPC status codes for enroll errors
`server/internal/agentgw/gateway.go:234-252`
The R2-11 mapping is correct on inspection (all internal failures → `codes.Internal` "enrollment failed", no path leaks), but no test asserts `status.Code(err)` or that internal error text stays out of client-visible messages — a regression re-leaking `/repos/<ulid>` paths would pass CI. **Fix:** extend the mTLS tests: bad token → `InvalidArgument`/`PermissionDenied`; injected internal failure → `Internal` with message exactly "enrollment failed".

### R3-9 · Migration test lacks the 755f417-shape fixture
`server/internal/catalog/migrate_test.go:15-85`
The v1 fixture models only the `f92837f` shape. The most realistic upgrade source — `755f417` (has `actor_type`, has the UNIQUE, lacks `keystore.hashing_algorithm`, stamped version 1) — has zero coverage; it was verified by hand this round, not by a test. **Fix:** second fixture reproducing the `755f417` schema (from `git show 755f417:server/internal/catalog/schema.sql`), assert idempotent convergence; combine with R3-4's seeded-row assertion.

## Nits / notes (fix cheaply or record; not gating)

- `server/internal/vault/prune_reclaim_test.go:106-109` — comment says "at least half," assertion is `/4`.
- `server/go.mod` pins `…/pkg v0.0.0-…-755f41736ae3`; dormant everywhere (go.work is tracked; CI and the Dockerfile both resolve `pkg` locally — verified), but it will drift. Add a comment noting go.work overrides it, or a `replace` directive.
- `REVIEW-M1.md` round-1 finding bodies were replaced with fix summaries rather than checkboxed — history is in git only. Acceptable; don't repeat for this file (append dispositions, keep finding text).
- Informational: hand-rolled enroll structs serialize PascalCase JSON (not the proto's snake_case) — inert while both ends share the structs; dies with M13's codec swap. `markLiveContents` silently skips a `bw-*` manifest whose payload decodes to an empty `RootObjectID` — unreachable via our writers today; M2's manifest hygiene should revisit.

---

## What was verified clean this round

- R2-1/R2-2 core logic (recursive mark, min-age sweep) — correct and **mutation-verified**: flat-mark revert → both survival tests fail; guard disabled → in-flight test fails.
- R2-14 round-trip test reconstructs content IDs from only enrollment-returned material; R2-15 tests model real M2 shapes; R2-13 asserts on-disk shrink in both reclaim test and gate.
- R2-3/R2-4 write-boundary validation; R2-7 CI `on:` block; R2-8 migration structure (versioned, idempotent, fresh/upgraded converge — modulo R3-4/R3-9); R2-10 manager eviction (modulo R3-6); R2-11 mapping (modulo R3-8 coverage); R2-12 error propagation. B2 unchanged and intact.
- Full verification suite re-run locally on `eea1a46`: gofmt/vet clean, short+race green, all Prune/Reclaim tests green, reduced gate green, **full 10 GiB gate green (152 s)**, pkg tests green.

## Required fix order

1. **Failing tests first:** R3-1 (undecodable file-kind root must fail prune — red on `eea1a46`), R3-3 (canceled-ctx compensation — red), R3-4 (migrated key-without-algorithm — red).
2. **Prune core:** R3-1 (write-boundary root validation + always-fail-closed decode + tree-root test data + delete heuristic helpers), R3-2 (bounded root reads), R3-5 (SafetyParameters from min-age).
3. **Enroll/keystore:** R3-3 (fresh compensation context), R3-4 (`ErrHashingAlgorithmNotSet`).
4. **Test hardening:** R3-7, R3-8, R3-9.
5. **Docs/nits:** R3-6 documentation, nit fixes; update `PROGRESS.md` (round-3 disposition table + corrected engine-gate description + fresh evidence including full 10 GiB gate) — append, don't rewrite history in this file.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore        # nothing
cd server
go vet ./...
go test ./... -count=1 -short -race -timeout 10m
go test ./internal/vault/ -count=1 -run 'Prune|Reclaim' -v
go test ./internal/enroll/ -count=1 -v
go test ./internal/catalog/ -count=1 -run 'Migrate|Upgrade' -v
go test ./internal/agentgw/ -count=1 -run 'TestM1_|TestEnroll_' -v
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -timeout 45m -v   # full 10 GiB
cd ../pkg && go test ./... -count=1
```

**Standing rules unchanged:** kopia imports confined to `server/internal/vault`; no AGPL/GPL library deps; no destructive RPCs on :9443; no invented storage format; no new one-way doors without stopping for human input.

---

## Disposition (append-only — do not rewrite finding text above)

Fixed on commit after `eea1a46` (round-3 fix set). Evidence in PROGRESS.md.

| ID | Status |
|----|--------|
| R3-1 | ✅ Fixed — write-boundary TreeObject/ImageManifest validation; mark always fail-closed; heuristic helpers deleted; tests wrap TreeObject roots |
| R3-2 | ✅ Fixed — `MaxMarkObjectBytes` 16 MiB LimitReader + fail-closed |
| R3-3 | ✅ Fixed — compensate on `context.WithTimeout(Background, 5s)` |
| R3-4 | ✅ Fixed — `ErrHashingAlgorithmNotSet` |
| R3-5 | ✅ Fixed — `safetyForMinAge`; SafetyNone only for zero min-age |
| R3-6 | ✅ Documented on Manager + M2 work item |
| R3-7 | ✅ Fixed — keystore COUNT assert in compensate tests |
| R3-8 | ✅ Fixed — `TestEnroll_gRPCStatusCodes` |
| R3-9 | ✅ Fixed — `TestUpgradeFrom755f417` |

---

## Round-4 verification addendum (reviewer, 2026-07-30)

Independent verification of the fix commit `161fb18`:

- **All verification commands re-run locally and green**, including the full 10 GiB
  gate — **124 s**, faster than round 3's 152 s because the mark phase no longer
  materializes the payload (gate log now shows a `rootTree=` TreeObject root).
- **Mutation battery — all five killed:** flat-mark revert → both survival tests
  fail; min-age guard disabled → in-flight test fails; fail-open decode restored →
  `TestMarkTreeObject_UndecodableRootFailsClosed` fails; compensation moved back to
  the request ctx → `TestEnroll_CompensateDespiteCanceledContext` fails; algorithm
  sentinel removed → `TestGetHashingKey_EmptyAlgorithmIsError` fails. Every round-3
  guard is genuinely enforced by a test.
- Diff inspection: fixes match the prescriptions exactly; heuristic helpers deleted;
  `validateSnapshotRoot` + always-fail-closed decode + 16 MiB capped reads +
  `safetyForMinAge` all present and correct.

**Verdict: round 3 findings are all closed. No new gating findings. M1 is honestly
closeable at `161fb18`.**

Two non-gating hardening notes recorded for M2 (also added to PROGRESS.md's M2 list):

1. `validateSnapshotRoot` uses loose JSON decoding, so a *mislabeled* kind (e.g. a
   TreeObject root stored under `bw-image-snapshot`) passes validation and would
   mark only the manifest's own contents — children swept. Only reachable via a bug
   in our own M2 writer code; fix cheaply with `json.Decoder.DisallowUnknownFields`
   in `validateSnapshotRoot` when M2's `PutTreeObject`/`PutImageManifest` land.
2. `MaxMarkObjectBytes` (16 MiB) implies a maximum single-directory entry count
   (~100k+ entries per TreeObject). M2's backup pipeline must either shard huge
   directories across child trees or revisit the cap; over-limit fails closed at
   both write and prune, so this is a usability ceiling, not a data-loss risk.
