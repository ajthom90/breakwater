# Breakwater M2 Stage 3 Review

**Reviewed:** commit `24300b1` ("feat(M2-s3): data plane — DataService, content IDs, backup pipeline"), diff `737c853..24300b1`, on 2026-07-30.
**Method:** full verification suite re-run locally (all green including the full 10 GiB engine gate, 124 s), line-by-line review of `data.go` / vault diff / engine lease surface, plus independent subreviews of `pkg/contentid`+`pkg/backup` and the scheduler changes. **S3-F1 was confirmed empirically** with a probe test against this commit.

## Verdict

**Do not build stage 4 on this commit — one blocker.** The stage delivers a lot that is right: machine binding and cross-machine isolation on every DataService RPC, `CheckContents` scoped to the caller's repo (no cross-repo oracle), lease-checked vault access via `Engine.VaultForJob`, strict JSON validation mirroring the vault's write-boundary discipline, `snapshot.commit` audit, a real dedup result (run2 uploaded 28 bytes vs run1's 10.5 MB), and the M1 prune-survival guarantee now exercised end-to-end through the real data path. But the multi-chunk-file wire hack introduces a **silent backup-corruption bug that is triggerable by an ordinary filename**, and the append-only data path has two structural gaps that must close before a real agent writes production data.

## Findings

### S3-F1 · BLOCKER — `.bw-object-from-contents` sentinel: a real filename silently corrupts the backup (reports success, restore broken)
`server/internal/agentgw/data.go:157-201` (sentinel interception in `PutTreeObject`), `pkg/backup/backup.go` (emits the sentinel tree)
To assemble multi-chunk files without a proto change, the agent sends a *fake* single-entry `TreeObject` named `.bw-object-from-contents` whose `ObjectID` field holds comma-separated content IDs; the server intercepts any such tree and calls `ObjectFromContents` instead of storing it. The interception is **content-based, not context-based** — so a genuine user directory containing exactly one entry named `.bw-object-from-contents` is intercepted too.

**Empirically confirmed on `24300b1`** (probe test, since removed): backing up a directory `userdir/` whose single file is named `.bw-object-from-contents` →
- the backup job reports **`state=success`, no error**;
- the parent tree's `userdir` entry points at the *file's own object* instead of a directory tree object;
- restore fails: `decode tree 7307831a…: invalid character 'r' looking for beginning of value` (`r` = first byte of the file's contents).

A successful backup that cannot be restored is the exact failure class this product exists to prevent, and it is reachable by a user creating one dotfile (or an attacker who can write one filename into a protected tree).

**Fix:** delete the sentinel; carry content IDs in a first-class field. The proto is frozen but **additive changes are pre-approved by precedent** (R2-5 `hashing_algorithm`, S2-F7 job types): add to `PutTreeObjectRequest` a `repeated string content_ids = 3;` (when non-empty, the server materializes an object from those contents and `tree_json` must be empty — mutually exclusive, validated), or add a dedicated `PutObjectFromContents` RPC to DataService if you prefer a cleaner surface. The `backup.Client` interface already has `PutObjectFromContents` — only the wire encoding changes. No in-band magic names anywhere in the data path.
**Test first:** the probe above — directory whose only entry is named `.bw-object-from-contents` → backup succeeds AND every file restores byte-identical. Must FAIL on `24300b1`. Also add a general "adversarial filenames" case to the pipeline tests (leading dots, names colliding with any reserved token, unicode, very long names).

### S3-F2 · High — the vault lease is checked once per PutContents stream, not per message
`server/internal/agentgw/data.go:86-113` (binds `v` on first message; no re-check), `server/internal/scheduler/engine.go:485-493`
`PutContents` resolves the job, validates the lease, opens the vault, and then streams for as long as the agent keeps sending. If the job reaches a terminal state mid-stream — cancel, agent-disconnect handling, `RecoverOnStartup`, dispatch failure — the engine releases the shared lease while this stream keeps writing. Prune can then take the exclusive lock and run concurrently with an active writer: exactly the R2-2 racing-writer scenario that stage 2's lease discipline was built to make structurally impossible. The 24 h min-age guard is the only remaining protection (defense in depth doing load-bearing work).
**Fix:** re-validate the lease per received message (`VaultForJob` is an in-memory map lookup — negligible cost), or hold a stream-scoped lease reference obtained at bind time and released at stream end, so the engine cannot drop the last reference while a writer is live. On lease loss: stop accepting, send `accepted=false`, end the stream cleanly.
**Test first:** start a `PutContents` stream, cancel the job mid-stream, assert subsequent messages are rejected and the stream terminates. Must FAIL on `24300b1`.

### S3-F3 · Medium — PutContents writes to the vault *before* verifying the client-supplied content ID
`server/internal/agentgw/data.go:125-140`
The handler calls `v.PutContent(ctx, data)` first and compares the client ID to the server ID afterwards. PLAN specifies the server "re-computes ID on write and **rejects** mismatches (free integrity check)" — here the mismatch is only *reported*: the payload is already persisted (under its true hash). Transfer corruption or a buggy agent therefore writes garbage into the repo that is unreferenced but occupies space for at least the 24 h min-age window, and the have/want discipline can be bypassed by an agent that simply uploads whatever it likes.
**Fix:** compute the expected ID **before** writing and reject mismatches without touching the vault — expose a `ComputeContentID(data []byte) (ContentID, error)` on the Vault interface (it already owns the repo's hashing params via `HashingKey`) and use it in the handler; only call `PutContent` once the client ID matches (or when the client omitted an ID).
**Test:** a mismatched-ID PutContents leaves the repo content count unchanged (assert via `Stats`), and still returns `accepted=false`.

### S3-F4 · Medium — `MaxPutContentBytes` doubled to 8 MiB with a hardcoded splitter string; image 4 MiB invariant now unenforced
`server/internal/vault/kopia.go:24-28` (constant), `:236` (`Splitter: "FIXED-8M"` literal)
The H2 guard (round 1) established a hard 4 MiB `PutContent` limit matching PLAN's "4MiB aligned blocks"; this commit raises it to 8 MiB to accommodate DYNAMIC-4M-BUZHASH max segments — defensible for the *file* path, but: (a) the splitter is a bare string literal rather than a named constant alongside `SplitterFixed4M`/`SplitterDynamic`; (b) nothing enforces that **image** blocks stay 4 MiB, so PLAN's fixed-block invariant (which RCT extent mapping depends on in Phase 3/4) is now only a comment; (c) the change is not called out in the M1 decisions list where H2's limit was recorded.
**Fix:** add a named `SplitterFixed8M` constant used by `PutContent`; document the 8 MiB CDC rationale in PROGRESS decisions (amending H2 explicitly); and enforce the 4 MiB block invariant where image manifests are validated (`PutImageManifest` should reject `block_size != 4 MiB` for now, or record why not).

## Verified clean (my pass)

- Machine binding and cross-machine isolation on every DataService RPC (`vaultForJobRPCFull`: peer → machine → job ownership → vault-writing type → active state → lease → repo match). `CheckContents` cannot be used as a cross-repo content-ID oracle — it only ever consults the caller's own vault.
- Append-only: no delete/overwrite surface; existing-ID `PutContents` is a dedup no-op; `CheckContents` batch cap enforced; oversized payload rejected per-message without stream teardown (the S2-F1 lesson applied).
- `CommitSnapshot` → vault `PutSnapshotRecord` (strict root validation from stages 1-2 still gates it) → catalog mirror → single `snapshot.commit` audit event on `WithoutCancel` (S1-F1 lesson applied). No per-chunk audit noise.
- `ObjectFromContents` verifies single-content presence before returning, parses every ID, and uses a write session consistent with the other writers.
- Full 10 GiB engine gate still green after the vault changes; `-race` suites green.

## Required fix order

1. **Red-first tests:** S3-F1 (sentinel-named directory), S3-F2 (cancel mid-stream), S3-F3 (mismatch leaves repo unchanged).
2. **S3-F1** — additive proto field/RPC, delete the sentinel path, adversarial-filename pipeline tests.
3. **S3-F2** — per-message lease validation (or stream-scoped lease hold).
4. **S3-F3** — compute-then-write.
5. **S3-F4** — named splitter constant, documented H2 amendment, image block-size enforcement.
6. Subreview findings (below) in their stated order; PROGRESS.md updated with red-first captures and the amended decisions.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore        # nothing
cd server
go vet ./...
go test ./... -count=1 -short -race -timeout 10m
go test ./internal/agentgw/ ./internal/scheduler/ -count=1 -race -v 2>&1 | grep -E '^(--- |ok |FAIL)'
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
cd ../pkg && go test ./... -count=1 -race
cd ../agent && go test ./... -count=1 -race
# No in-band magic names left in the data path:
grep -rn 'bw-object-from-contents' server/ pkg/ agent/ ; echo "sentinel grep exit=$? (want 1)"
```

**Standing rules:** unchanged, except the proto freeze admits S3-F1's additive field/RPC (precedent R2-5, S2-F7). The `pkg/contentid` kopia carve-out (repo/hashing + repo/splitter only) stands as specified in the stage-3 contract.

---

## Subreview A — `pkg/contentid`, `pkg/backup`, agent module

Independent adversarial pass (source-level, cross-checked against kopia v0.19.0 sources in the module cache).

### S3-F5 · High — symlinks are silently dropped from every backup (success reported, data missing)
`pkg/backup/backup.go:173-181`
The comment says "symlinks treated as files with content of target path — portable MVP", but the code never special-cases `os.ModeSymlink`. `os.ReadDir` + `DirEntry.Info()` are Lstat-based, so a symlink (to file *or* directory) always fails `IsRegular()` and hits `continue`: no `TreeEntry`, no stats increment, no warning, no error. A tree containing symlinks backs up as **success** with every symlink silently omitted — the same "success but incomplete restore" class as S3-F1. `pkg/backup` has **no unit tests at all**; the only exercising test is the server demo fixture, which contains no symlinks. Corollary: the pre-scan counts symlink sizes into `totalBytes` but they're never added to `bytesDone`, so progress can never reach 100% when symlinks exist.
**Fix:** decide and implement the M2 policy explicitly — store symlinks as `format.EntrySymlink` with the target in `ReparseData` (the format already has both fields) — and record any deliberately skipped entry type in the job result/stats so "skipped" is always visible, never silent. Add `pkg/backup` unit tests covering symlinks (file + dir targets, loops), device/socket files, empty files/dirs.
**Test first:** backup a tree with symlinks → assert they appear in the snapshot (or are explicitly reported as skipped, never silently missing).
**Client-side defense-in-depth for S3-F1:** `backupDir` must also reject/escape a real entry named `.bw-object-from-contents` regardless of the server-side fix.

### S3-F6 · Medium — the multi-chunk round-trip test is non-deterministic and usually tests only one chunk
`server/internal/vault/contentid_roundtrip_test.go:41-54`
The "multi-chunk" case uses a 3 MiB random payload. Verified against pinned kopia v0.19.0: `DYNAMIC-4M-BUZHASH` has minSize 2 MiB / avgSize 4 MiB / maxSize 8 MiB, and bytes below minSize are never hash-checked — so only ~1 MiB is eligible, giving **P(zero splits) ≈ 78%**. The test never asserts `len(ids) > 1`, so it usually degenerates to the single-chunk path and never exercises `ConcatenateObjects`. This is the test that is supposed to lock the have/want contract (the R2-14 role).
**Fix:** use a payload > maxSize (**> 8 MiB**), which forces a split unconditionally, and assert `len(ids) > 1` explicitly. Add small/edge sizes (0, 1, exactly minSize, exactly maxSize) as separate cases.

### S3-F7 · Medium — kopia confinement is enforced only for the `pkg` module, not `agent`/`cli`
`pkg/contentid/confinement_test.go:75-97`
The test walks only the directory containing `pkg/go.mod` (it does correctly catch a *new* `pkg/<subpackage>` violation). But `agent/` and `cli/` are separate modules and are never scanned, while `agent/go.mod` already carries kopia as an indirect dep — so a direct `import "github.com/kopia/kopia/repo/content"` anywhere under `agent/` or `cli/` compiles and every test stays green. The carve-out doc comment claims coverage of "pkg/agent/cli".
**Fix:** extend the confinement check to all modules (walk the workspace root, or add equivalent tests in `agent`/`cli`), and wire it into CI so the rule is enforced by the pipeline, not one module's test.

### S3-F8 · Low — splitter boundary identity with `WriteObject` is true by construction but untested
`pkg/contentid/contentid.go:127-197` vs `server/internal/vault/kopia.go` (`WriteObject`, `SplitterDynamic`)
Both sides resolve the same `"DYNAMIC-4M-BUZHASH"` factory from the same pinned kopia and the streaming loops are algorithmically equivalent, so boundaries match today — but nothing tests it, and the production data path never invokes `WriteObject` with the dynamic splitter (only `FIXED-4M` for tree/manifest JSON). A future kopia bump could silently break the agent/server chunk agreement.
**Fix:** one regression test that splits a >8 MiB payload with `pkg/contentid` and with vault `WriteObject(SplitterDynamic)` and asserts identical content-ID sequences.

### Verified clean (subreview A)
- `contentid.New()` cannot fall back to an unkeyed hash (unexported fields, empty algorithm/secret rejected, unknown algorithm hits an explicit error).
- Content-ID string form is byte-identical to kopia's `index.ID.String()` for unprefixed IDs.
- `CheckContents` batching caps at 4096 with no off-by-one; directory entries are name-sorted before tree construction (deterministic trees → dedup of unchanged subtrees works); empty dirs/files produce valid entries consistent with kopia's own flush-on-empty behavior; no symlink-loop hang risk (dir symlinks never satisfy `IsDir()`).
- `agent/internal/fileback` is a pure re-export with no kopia imports.

### Noted, not scored
The pipeline aborts the whole job on the first per-file error rather than skip-and-record. PLAN doesn't state a policy; fail-loud is defensible and consistent with this project's aversion to silent omissions. **Decide it explicitly in the fix round** and document it — with S3-F5 fixed, "skipped" must always be visible in the job result either way.

---

## Subreview B — scheduler / job engine (stage-3 changes)

Independent adversarial pass over `server/internal/scheduler/` + `catalog/jobs.go`.

### S3-F9 · BLOCKER — concurrent dispatch of the same job leaks a Shared lease: prune permanently wedged AND the job cannot write
`server/internal/scheduler/engine.go:138-202` (state check at :146, lease acquire + map write at :153-166, CAS at :168-180), `:595-605` (`releaseLease`)
`tryDispatch` has no per-job serialization, so two goroutines can both observe a job as `pending` and both acquire a lease before either commits its CAS. Reachable routinely: `Submit`'s inline dispatch races heartbeat-triggered `DeliverPending` (new in this stage), and an old session's in-flight `DeliverPending` races the new session's connect-time call during supersede (`TryAcquire` takes no context, so stream cancellation never short-circuits it).

Trace: A acquires (shared 0→1), stores leaseA; B acquires (shared 1→2), **overwrites the map entry** with leaseB — leaseA is now unreferenced; A's CAS wins (job→running); B's CAS fails, so B calls `releaseLease`, which releases whatever is in the map (leaseB, shared 2→1) **and deletes the entry**; on terminal, A's `releaseLease` finds nothing and no-ops. `shared` is stuck at 1 forever → `Exclusive` can never be acquired → **prune/verify wedged on that repo permanently, with no operator-visible symptom.**

**Empirically confirmed on `24300b1`** (probe test, since removed; reproduced 20/20 runs):
```
after concurrent dispatch: shared=1 exclusive=0 leases=0
after job success:         shared=1 exclusive=0 leases=0
LEASE LEAK CONFIRMED: shared=1 after terminal job — prune permanently blocked
```
Note `leases=0` while the job is still *running* — so the second symptom is immediate: the dispatched job holds no tracked lease, and every DataService call for it is rejected by `vaultForJobRPCFull`'s lease check (`FailedPrecondition: no vault lease held for job`). The race therefore both **breaks the backup** and **wedges retention**. This is the R2-2/S2-F3 failure class reintroduced through lease accounting rather than job-state accounting (the state CAS is correct; the lease map is not).
**Fix:** make the claim atomic — perform the `pending → running` (or an intermediate `dispatching`) CAS **before** acquiring the lease, and acquire only if the CAS was applied; or serialize `tryDispatch` per jobID (singleflight / per-job mutex). Whichever: exactly one goroutine may reach lease acquisition per job, and every acquired lease must be reachable from the map for release.
**Test first:** the probe above — concurrent `DeliverPending` for one pending job, then complete it, assert `Held(repoID) == (0,0)` and that `Exclusive` acquires. Must FAIL on `24300b1`. Add a stress variant (N goroutines × M jobs) asserting locks return to zero — no existing test checks lock accounting after concurrency; all current scheduler tests are single-goroutine.

### S3-F10 · High — `cancelling` holds the vault lease with no timeout: a hung agent on a live channel wedges prune forever
`server/internal/scheduler/engine.go:391-429`
The documented release conditions for a cancelling vault-writing job are "agent JobResult or channel teardown" — both verified correct — but they are **not exhaustive**. If the channel stays healthy (heartbeats flowing, so `OnAgentDisconnect` never fires) and the agent never sends a `JobResult` (bug, deadlock, or an agent deliberately ignoring `JobCancel`), the job stays `cancelling` and holds its Shared lease indefinitely → prune/verify blocked forever on that repo. No timeout, watchdog, or deadline exists anywhere in the server (the M5 watchdog in PLAN is the *missed-backup* notifier, unrelated).
**Fix:** bound it — start a deadline on entering `cancelling`; on expiry force-fail the job, release the lease, and log loudly (data may still be in flight to the vault, so the min-age guard matters here). If you'd rather defer, that must be an explicit documented decision, not silence in a doc comment that reads as exhaustive.

### Verified clean (subreview B)
- Non-blocking dispatch lease acquisition: `TryAcquire` then a hard-bounded 50 ms `Acquire`; failure leaves the job `pending` (never failed, never stuck running), and lease-free types (inventory/noop/update) still dispatch in the same loop.
- Writer preference respected by the non-blocking path (`TryAcquire` checks `exclusiveWaiters`) — no S2-F3 regression.
- `cancelling` exists in the catalog state machine with conditional CAS transitions; JobResult (success or failure) and agent disconnect both release the lease; non-vault jobs still cancel immediately.
- `RecoverOnStartup` covers both `running` and `cancelling`.
- `TestS2F4`'s new failure-result assertion is real (the guard fires before the success/failure branch, so both are uniformly ignored for pending jobs).
- No regression to the S2 undelivered-JobStart revert ordering.

### Minor (noted, not gating)
- `schema.sql:69` state comment not updated to include `cancelling` (no CHECK constraint; cosmetic).
- `Engine.Cancel` ignores `SendJobCancel`'s `sent` bool; `Registry.send` never returns a non-nil error, so a queue-full channel silently drops the JobCancel while the job still enters `cancelling` — unlike JobStart, there is no redelivery. Worth a follow-up given S3-F10.

---

## Fix dispositions (post-24300b1)

| ID | Status | Notes |
|----|--------|-------|
| S3-F1 | ✅ Fixed | Additive `content_ids=3` on PutTreeObjectRequest; sentinel path deleted; adversarial-filename restore test |
| S3-F2 | ✅ Fixed | PutContents re-validates lease via vaultForJobRPC every message |
| S3-F3 | ✅ Fixed | Vault.ComputeContentID before PutContent; Stats unchanged on mismatch |
| S3-F4 | ✅ Fixed | SplitterFixed8M constant; H2 amendment in PROGRESS; PutImageManifest block_size=4MiB |
| S3-F5 | ✅ Fixed | EntrySymlink + ReparseData; Stats.Skipped for unsupported; fail-loud I/O policy; unit tests |
| S3-F6 | ✅ Fixed | 10 MiB case + assert len(ids)>1 |
| S3-F7 | ✅ Fixed | Confinement walks pkg/agent/cli; CI step |
| S3-F8 | ✅ Fixed | WriteObject(DYNAMIC) content-ID sequence identity test |
| S3-F9 | ✅ Fixed | pending→running CAS before lease acquire; concurrent + stress lock accounting |
| S3-F10 | ✅ Fixed | CancelConfirmTimeout (default 2m) force-fails cancelling jobs + releases lease |
| schema cancelling comment | ✅ Fixed | schema.sql state comment includes cancelling |

Red-first captures and after-fix evidence: see `PROGRESS.md` stage-3 fix round.
## Consolidated fix order (supersedes the earlier section)

1. **Red-first tests:** S3-F1 (sentinel-named dir → restore must work), S3-F9 (concurrent dispatch → locks return to zero), S3-F5 (symlinks visible), S3-F2 (cancel mid-stream), S3-F3 (mismatch leaves repo unchanged).
2. **Blockers:** S3-F1 (additive proto field/RPC, delete sentinel, client-side reserved-name rejection), S3-F9 (atomic claim before lease acquisition).
3. **Data-path structure:** S3-F2, S3-F3, S3-F10.
4. **Correctness/policy:** S3-F5 (+ explicit per-file error policy), S3-F4.
5. **Test/infra hardening:** S3-F6, S3-F7, S3-F8, the minors.
6. PROGRESS.md: red-first captures, decisions (error policy, H2/8 MiB amendment, confinement scope).

---

## Reviewer verification (fix round, 2026-07-30)

Independently verified `2241698`. Both blockers re-tested with the reviewer's own
probes (the ones that originally caught them):

- **S3-F1:** a file literally named `.bw-object-from-contents` now backs up and
  **restores byte-identical** (`restored paths: [userdir/.bw-object-from-contents]`).
  The sentinel path is deleted; the wire uses the additive `content_ids` field; CI
  greps non-test code to keep the magic name from returning.
- **S3-F9:** the leak probe that failed 20/20 on `24300b1` now passes **25/25 under
  `-race`**, asserting exactly one tracked lease while running and `Held == (0,0)`
  plus a successful `Exclusive` acquire after terminal.

Mutation battery — four for four killed: CAS-before-lease reverted → leak probe +
2 S3 tests fail; per-message lease revalidation reverted to bind-once →
`TestS3F2_CancelMidPutContents…` fails; compute-then-write reverted →
`TestS3F3_MismatchLeavesRepoUnchanged` fails; symlink handling dropped →
`TestBackup_SymlinksStored` + `TestS3F5_SymlinksPresentInSnapshot` fail.

Also verified: multi-chunk round-trip now uses a 10 MiB payload and asserts
`len(ids)>1`; confinement walks pkg/agent/cli with a CI job; policy decisions
(fail-loud I/O, visible `Skipped`, H2→8 MiB with image blocks pinned at 4 MiB) are
recorded in PROGRESS.md. Full suite green under `-race`; reduced gate green; **full
10 GiB engine gate green (122.8 s)**.

**Stage 3 closed.** Carried into stage 4: `pkg/backup` and `pkg/contentid` are the
only pipeline/content-ID implementations (no forks); agent must send a terminal
JobResult on cancel (the server now bounds cancel confirmation); reconnect
idempotency is contract, not best-effort.
