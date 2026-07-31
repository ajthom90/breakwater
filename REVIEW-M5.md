# Breakwater M5 Review — retention, scrub, schedules, alerts

**Reviewed:** commit `cfb0a82` ("feat(M5): GFS retention, forget/prune grace, scrub, schedules, alerts"), diff `76ea92f..cfb0a82`, on 2026-07-31.
**Method:** full verification re-run locally (gofmt/vet; `-race` across all modules; retention property tests at `-count=5`, ~211 s; engine gate). Line-by-line review of the retention service, keep-set, grace enforcement, scrub, and the new mutating REST surface. Grace enforcement **mutation-tested**.

## Verdict

**Strong milestone — two findings, no blockers.** This is the first code in the project that deletes customer backups, and the safety architecture is right where it matters most: **`Forget` never touches the vault**, so an in-grace snapshot's data survives prune *structurally* rather than by remembering to special-case it in the mark phase. That is the correct way to build this guarantee. The keep-set is pure and clock-injected with a test asserting production wires the real clock, the property tests assert every surviving snapshot **fully restores** (not merely that prune returned no error), and the agent still has no path to forget or prune.

The two findings are about a destructive surface arriving ahead of the auth PLAN assigns to it, and a lock mode that is stronger than necessary in a way that will silently eat the backup window.

## Findings

### M5-F1 · Medium-High — destructive REST endpoints are gated only by the static dev API token; no admin role, no read/write separation
`server/internal/web/api.go:54-64` (`POST …/snapshots/{id}/forget`, `…/machines/{id}/prune|retention|scrub`, `POST /api/v1/jobs`, `POST /api/v1/rescan`), mounted behind the single `RequireAPIToken` gate at `:69-73`
M2 stage 5 deliberately shipped `:8443` as **GET-only**, with the dev token documented as a placeholder until real sessions land in M6. M5 adds endpoints that forget snapshots and run prune behind that *same* single static token. PLAN is explicit that **mass-forget requires admin + audit**, and there is no admin concept yet.

Blast radius is genuinely bounded, and I want to state that precisely rather than overstate the finding:
- `Prune` only reclaims snapshots whose `deleted_at` is fully past the grace cutoff (verified in `service.go:188-200`), so an API caller **cannot** destroy live or in-grace data — it only accelerates reclamation of data already forgotten and expired.
- `Forget` and `retention`-apply are reversible for the 7-day grace window.

So the realistic attack is: leak the token → forget everything (recoverable for 7 days, and audited) → wait out the grace. That is survivable *if noticed*, which is exactly what the alerting in this milestone is for. But the same token is what a monitoring script, `bwctl` config, or a support bundle would carry for **read-only** use — so any read-scoped leak is also a destroy-scoped leak.
**Fix (pick one, and record it):** put the destructive subset behind an explicit opt-in until M6 auth exists — a `--enable-destructive-api` flag defaulting **off**, or a second token distinct from the read token. Either way, document the gap in PROGRESS as a known pre-auth limitation, and keep the audit actor distinction (already passed through) so M6 can slot real identities in without changing call sites.

### M5-F2 · Medium — scrub takes an **exclusive** lease though it only reads; with writer preference this starves backups
`server/internal/retention/scrub.go:48-54` (rationale), `:76` (`locks.Acquire(..., scheduler.Exclusive, ...)`)
The stated reason is that "scrub's `GetContent` path would race a concurrent prune's `DeleteContent`". That hazard is real but **a shared lease already prevents it**: prune takes exclusive, and exclusive cannot be acquired while any shared holder exists. Two shared holders (a backup writer and a scrub reader) cannot conflict — neither deletes, and content is immutable and content-addressed.

The cost of over-locking is not theoretical. S2-F3 added **writer preference**: while an exclusive waiter exists, *new shared acquisitions block*. So a queued scrub blocks new backups for that machine until it completes — and PLAN schedules scrub daily on a subset plus a **full read-back monthly**. A full read-back on a multi-TB repo holding exclusive would stall that machine's backup window entirely, silently, on exactly the night it runs.
**Fix:** scrub acquires **shared** (it is vault-read-only; verify state is written to the catalog, not the vault). Keep exclusive for prune and for any future repair-style verify that mutates. Update the rationale comment to explain why shared is sufficient (prune exclusion comes free) so it is not "corrected" back later. Add a test that a scrub in progress does not block a backup, and that prune still cannot start during a scrub.

### Nit (not gating)
`server/internal/retention/property_test.go:27` seeds from `time.Now().UnixNano()`. The seed **is** logged, so failures are reproducible — the M2 S3-F8 lesson was applied. Consider also pinning a handful of regression seeds so known-interesting cases run every time in addition to fresh randomness.

## Verified clean (my pass)

- **In-grace survival is structural.** `Service.Forget` writes only `SoftDeleteSnapshot` to the catalog and never touches the vault, so the vault manifest remains and the mark phase treats the snapshot as live by construction. The safety property cannot be broken by forgetting to special-case it.
- **Grace enforcement is real — mutation-tested.** Making prune eligibility ignore the cutoff fails `TestM5_GraceWindowSurvivesPrune`. Eligibility requires `deleted_at` at or before `now - grace`.
- **Grace × min-age compose correctly:** catalog-level grace (7 d) and content-level min-age (R2-2, 24 h) are independent gates; both must permit deletion, and neither was weakened to make tests pass.
- **Clock discipline:** `ComputeKeepSet(snapshots, policy, now)` is pure with `now` as a parameter; the fake clock is test-only with no env override, and `TestProductionUsesSystemClock` asserts the production wiring. A backup server on a wrong clock expires the wrong snapshots — this is the right guard.
- **Property tests assert the real invariant:** after random forget/prune interleavings, every surviving snapshot is walked and every file object opened — restorability, not return codes. In-grace soft-deleted snapshots are asserted restorable too. Seed printed on failure.
- **Agent boundary intact:** `TestM5_AgentHasNoForgetOrPrunePath` — no forget/prune reachable from `:9443`. The append-only ransomware boundary holds.
- **Audit:** `retention.forget` / `.undelete` / `.prune_run` recorded, with a `mass_forget` flag above threshold, per PLAN's taxonomy.
- **Scheduler:** cron used for parsing only, evaluation on the injected clock; windows gate job *start* (a running job is not killed at window close — documented); missed-window catch-up runs once rather than replaying every occurrence.
- **Notify:** queue + bounded retry, fake sender in tests (no network), credentials never logged.
- 90-day time-warp harness runs schedules + retention and asserts the keep-set (fires=91, final keep=21 under Standard Server defaults).

## Suggested fix order

1. M5-F2 — scrub to shared lease + the two availability tests (this one silently degrades backups; fix first).
2. M5-F1 — gate the destructive subset behind an explicit opt-in or separate token; document the pre-auth limitation.
3. Nit — pin regression seeds alongside the random ones.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore tools
cd server && go vet ./... && go test ./... -count=1 -short -race -timeout 15m
go test ./internal/retention/ -count=5 -race -v
go test ./internal/scheduler/ -count=1 -race -v
go test ./internal/web/ -count=1 -race -v      # destructive-endpoint gating tests
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
```

---

## Disposition (fix round, append-only)

| ID | Status | Notes |
|----|--------|-------|
| M5-F2 | ✅ Fixed | Scrub uses `scheduler.Shared`; package comment documents why shared suffices (prune exclusion structural; exclusive would starve backups under S2-F3 writer preference). Tests: `TestM5F2_ScrubSharedDoesNotBlockBackup`, `TestM5F2_PruneBlockedWhileScrubRunning`, `TestM5F2_ScrubCompatibleWithConcurrentShared`. |
| M5-F1 | ✅ Fixed | `--enable-destructive-api` defaults **off**; forget/undelete/prune/retention/scrub return 403 when disabled; reads still work. Decision recorded in PROGRESS as known pre-auth limitation until M6. Audit actor plumbing unchanged. Tests: `TestM5F1_DestructiveAPIDisabledByDefault`, `TestM5F1_DestructiveAPIEnabledPassesGate`. |
| Nit | ✅ Fixed | `propertyRegressionSeeds` run every pass; fresh `time.Now().UnixNano()` seed still logged for new coverage. |
