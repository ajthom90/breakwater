# Breakwater M4 Review — restore path

**Reviewed:** commit `0d8057f` ("feat(M4): restore path — RestoreService, agent JOB_TYPE_RESTORE, bwctl"), diff `4a0bf14..0d8057f`, on 2026-07-31.
**Method:** full verification re-run locally (gofmt/vet; `-race` across server, pkg, agent, cli, tools/golden; all seven `TestM4_*` green; reduced engine gate; docker build). Line-by-line review of the authorization core (`server/internal/agentgw/restore.go`), the reachability walk, lease revalidation, and the portable restore engine.

**Context:** M3 (VSS) is blocked on a Windows VM, so M4 was pulled forward. That reordering is sound — restore depends on M2's plain-directory backups, not on VSS — and it is recorded in PROGRESS.md.

## Verdict

**Good milestone; two Medium findings, neither a blocker.** This closes the largest gap in the product: until now Breakwater could back up but had never restored through its own API. The authorization design is the part I scrutinised hardest and it holds up — cross-machine restore is genuinely job-scoped *and* reachability-scoped, so a restore job is not a read-anything ticket for the source repo. Per-chunk lease revalidation correctly applies the S3-F2 lesson to streams, `ObjectDataContentIDs` is used instead of `VerifyObject` (the S5-F1 lesson), skips are visible records rather than silent omissions (the S3-F5 lesson), and the server-loss drill finally exercises PLAN's "catalog is a rebuildable index" claim that has been asserted since M1 without proof.

The findings are about resource bounds on the new reachability walk, plus a pre-existing depth limit that this milestone makes inconsistent.

## Findings

### M4-F1 · Medium — reachability walk has no depth bound, and disagrees with prune's; prune's limit of 256 can fail-closed on legitimate data
`server/internal/agentgw/restore.go:579-627` (`walkTreeReachable` — unbounded recursion), vs `server/internal/vault/kopia.go:952-958` (`maxTreeDepth = 256`, fail-closed)
Two related problems:

- **Inconsistency:** the restore reachability walk recurses into child trees with no depth limit, while prune's mark phase refuses any tree deeper than 256. Two walkers of the same structure now disagree about what is walkable, which is how divergent-invariant bugs start (this milestone already inherited one such lesson from S5-F1).
- **The bigger issue is prune's limit itself.** `maxTreeDepth = 256` **fails the entire prune** for the repo when exceeded. A directory tree deeper than 256 levels is unusual but reachable with ordinary data (deeply nested build output, long-path-enabled trees — and we ship a >260-char-path golden fixture precisely because such paths are real). If a customer's data ever exceeds it, retention wedges permanently and silently for that repo — the exact failure class R2-2/S2-F3/S3-F9 were each about, arriving this time through data shape rather than a race.

**Fix:** share one constant between both walkers; raise it well beyond plausible real-world nesting (e.g. 4096) so it is a runaway guard rather than a data-shape limit; and make prune's over-limit error name the offending manifest and path so an operator can act instead of discovering that retention silently stopped. The restore walk should enforce the same bound (unbounded recursion over attacker-influenced structure is worth bounding on principle, even though Go's growable stacks make exhaustion impractical).

### M4-F2 · Medium — `reachCache` is never evicted: every restore job's full object/content set is retained for the process lifetime
`server/internal/agentgw/restore.go:56-58` (`reachCache map[string]*reachableSet`), `:528-558` (populated, never deleted)
The reachable set is computed eagerly per restore job and cached by job ID, but nothing removes an entry when the job reaches a terminal state. For a large snapshot the set is one map entry per object *and* per content ID — millions of entries for a multi-TB machine — held until `breakwaterd` restarts. A server that runs many restores (or PLAN's Phase-2 scheduled restore-drill feature, which restores random files periodically) grows without bound.
**Fix:** evict on job terminal state (the engine already knows when that happens — the same hook that releases the lease), or bound the cache with an LRU. Add a test asserting the cache does not retain entries for completed jobs.

## Verified clean (my pass)

- **Cross-machine authorization is structural.** Default is own-repo-only, resolved from the connection cert. A foreign object is reachable only when the caller has an *active* restore job whose lease is still held, whose params name that snapshot, whose `jobMatchesSnapshot` check passes, **and** whose precomputed reachable set contains the requested object/content. Without a job, the request falls through to the caller's own vault, misses, and is denied. Both red-first security tests (`TestM4_RedFirst_CrossMachineWithoutJobDenied`, `TestM4_RedFirst_JobDoesNotAuthorizeOutsideReachableSet`) exist and pass.
- **Per-chunk lease revalidation** (`revalidateLease`) checks both that the engine still holds the job lease *and* that the job is still `running`/`cancelling`, on every chunk of `GetObject`/`GetContentRange` — the S3-F2 lesson applied to restore streams. `TestM4_RestoreLeaseBlocksPrune` proves prune cannot proceed while a restore stream is open, closing the original R2-2 "OpenObject vs prune" hazard end-to-end.
- **Correct ordering primitive:** the walk uses `ObjectDataContentIDs` (stream order), not `VerifyObject` (map order) — the S5-F1 trap avoided, with the reason cited in a comment.
- **Fail-closed reachability:** a root that does not decode as a `TreeObject` aborts the walk rather than granting a partial set.
- **No silent data loss on restore:** unsupported entry types, missing OIDs, invalid names, and symlink failures all produce visible `SkipRecord`s; conflict policy implements all three modes (`overwrite`/`rename`/`skip`) with `rename` falling back to `.restored.N`.
- **Read-only on :9443:** no mutating RPC added; the append-only ransomware boundary is intact.
- **Audit:** one `restore.file` per `GetObject` and `restore.browse` per listing — not per chunk or per byte range — matching PLAN's "restores are first-class audit events" without flooding the chain.
- **Server-loss drill** (`TestM4_ServerLossDrill`, Trust Checklist #12): snapshot index wiped, rebuilt via `internal/rescan` from the repo alone, restore still succeeds. First real evidence for the rebuildable-index claim.
- Outstanding debt cleared: CI now `git diff --exit-code`s the committed UI bundle, and `golden.Compare` returns a partial result alongside the walk error instead of `nil` (the nil-panic my own probe hit).

## Not claimed (correctly)

Windows ACL/ADS restore via `BackupWrite` is stubbed behind the platform split and listed as untested-on-Windows. The portable path records a skip when SD/ADS metadata is present rather than silently dropping it.

## Suggested fix order

1. M4-F1 — shared depth constant, raised bound, actionable prune error.
2. M4-F2 — evict reachability cache on job terminal state + test.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore tools
cd server && go vet ./... && go test ./... -count=1 -short -race -timeout 12m
go test ./internal/agentgw/ -count=1 -race -run 'TestM4' -v
go test ./internal/vault/ -count=1 -run 'Prune|Depth' -v
cd ../pkg && go test ./... -count=1 -race
BW_GATE_BYTES=268435456 go test ../server/internal/vault/ -count=1 -run TestEngineGate_Kopia -v
```

## Disposition (fix round)

| ID | Status | Notes |
|----|--------|-------|
| M4-F1 | ✅ Fixed | Shared `format.MaxTreeDepth = 4096` used by prune mark and restore reachability. Raised from 256 so it is a runaway guard, not a data-shape limit. Over-limit errors name manifest + path prefix (actionable). Tests: `TestPruneDeepTreeBeyondOld256Limit` (depth=300), `TestTreeDepthExceededError_Actionable`, `TestMarkTreeDepthPathPrefix`, `TestM4F1_DeepTreeRestoreReachability`. |
| M4-F2 | ✅ Fixed | `Engine.OnJobTerminal` hooks fire from `releaseLease`; `RestoreServer.EvictReachCache` registered in `breakwaterd` + test env. `TestM4F2_ReachCacheEvictedOnTerminal` asserts no entry after terminal. |

### Decision (depth bound)

Historical prune-only `maxTreeDepth = 256` could fail-closed on legitimate deep trees and permanently wedge retention (same failure class as R2-2/S2-F3, via data shape). Bound is now **4096**, shared by both walkers, documented on `pkg/format.MaxTreeDepth`. Exceeding it still fails prune/restore **loudly** with operator-actionable wording (forget snapshot / flatten tree) — never silent skip.


---

## Reviewer verification (fix round, 2026-07-31)

Independently verified `6eb8dac`. Full suite green under `-race` (server, pkg,
agent, cli, tools/golden); all `TestM4_*` green; reduced engine gate green.

- **M4-F1:** `format.MaxTreeDepth = 4096` is now shared by prune's mark phase and
  the restore reachability walk. `TestPruneDeepTreeBeyondOld256Limit` proves a
  300-deep tree — which previously **wedged retention permanently** — now prunes
  and restores successfully. The over-limit error names manifest and path prefix
  (`TestTreeDepthExceededError_Actionable`, `TestMarkTreeDepthPathPrefix`).
- **M4-F2:** reachability cache is evicted via an `Engine.OnJobTerminal` hook
  fired from `releaseLease`, wired in `breakwaterd`;
  `TestM4F2_ReachCacheEvictedOnTerminal` asserts no entry survives job terminal.

**Mutation battery — two for two killed:** reverting `MaxTreeDepth` to 256 fails
the deep-tree prune test; disabling the terminal hooks fails the cache-eviction
test. Neither test passes vacuously.

**M4 closed.** The product can now back up *and* restore through its own API,
with cross-machine restore authorized structurally (job-scoped + reachability-
scoped) and restore streams holding a shared lease that provably blocks prune.
