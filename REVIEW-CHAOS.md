# Breakwater Review — chaos drill matrix + Trust Checklist

**Reviewed:** commit `f58d5e3` ("feat(chaos): Linux drill matrix + honest trust checklist (CHAOS-F1 clock skew)"), diff `948390a..f58d5e3`, on 2026-07-31.
**Method:** full verification re-run (gofmt/vet, `-race` across all modules, chaos suite). Read every drill for fault-injection proof. **CHAOS-F1's fix mutation-tested by the reviewer.**

## Verdict

**Accept — the drills did their job and found a real defect.** One finding, and it is a gap the implementer disclosed rather than hid.

The drills are not theatre: each injects a fault and **proves it fired** (fault counters, injected-failure counts, real tiny filesystems), then asserts the invariant that matters — every surviving snapshot **fully restores**, not that an operation returned nil. The flagship kill-9 fuzz asserts baseline snapshots still restore with exact payloads after every crash iteration, and guards against a vacuous pass by requiring survivors to exist.

## Defect found by the drills — CHAOS-F1 (fixed in the same commit)

**Drill #5 found that `CommitSnapshot` used the agent-supplied `FinishedAt` as the vault `SnapshotRecord.Timestamp`.** Snapshot timestamps are the input to GFS retention. An agent with a wrong clock — or a compromised one choosing its own timestamps — could therefore influence which snapshots retention considers old: backdate to force early expiry, or post-date to evade it. The catalog already used server time, so impact was bounded, but the vault (the authoritative record, and the thing `bwctl rescan` rebuilds from) was agent-influenced.

**Fix:** the server `Clock` now always sets vault Timestamp and catalog `created_at`; agent time is retained only in `Extra` and audit, with a warning above 1 h skew.
**Verified by the reviewer:** re-introducing trust in agent `FinishedAt` fails `TestChaos05_AgentClockSkew`; restoring it passes.

This is a good catch and exactly why PLAN schedules these drills — it is the same class as the M2/M3 findings (a guarantee undermined by the bookkeeping beside it), reachable here by an agent simply having a bad clock.

## Finding

### CHAOS-F2 · High — alerting is fully implemented, tested, and **never runs in production**
`server/cmd/breakwaterd/main.go` (no reference to `notify`, `Watchdog`, or `Digest` — verified by grep)
`server/internal/notify` is real and covered (queue, bounded retry, fake sender, watchdog, digest), and chaos #9 proves the watchdog mechanism fires. But **nothing in `breakwaterd` ever constructs or schedules it.** In a deployed server today: no failure alerts, no missed-backup watchdog, no daily digest.

This matters more than a wiring oversight normally would. PLAN's whole M5 alerting rationale is that a backup product's worst failure is the *silent* one, and the Trust Checklist requires "missed/failed backup emails within the window". A watchdog that never runs is precisely the failure mode the watchdog exists to catch — and because the library is well-tested, a casual reader would reasonably assume it works.

**Credit where due:** this is disclosed accurately in `docs/trust-checklist.md` (#10 marked ⚠️ with "production wiring ⏳"), which is the right behavior. The finding is that it should be *closed*, not merely disclosed — it is small work and it is the difference between M5's headline feature existing and functioning.
**Fix:** wire `notify` into `breakwaterd` — SMTP config from settings, watchdog on a periodic tick using the injected clock, digest on a daily schedule, failure alerts from job terminal transitions. Add an end-to-end test through the real server wiring (fake sender) so this cannot silently regress.

## Verified clean

- **Faults are proven to fire.** #3 asserts an injected failure counter > 0 before healing; #4 uses a real tiny filesystem (darwin ram disk / Linux tmpfs) rather than a mocked error; #8 discovered and worked around kopia's content cache masking bit-flips (cache cleared so scrub genuinely re-reads packs) — that is the kind of self-skepticism these drills need; #10 counts crash injections.
- **Assertions are end-state invariants, not return codes:** survivors must fully restore; ENOSPC leaves **0 partial snapshots**; partition leaves no duplicate manifests or rows; bit-flip must produce a non-empty `AffectedSnapshots` set, not merely a detected bad chunk.
- **Honest crash model.** #10's dense 500-iteration loop uses `Manager.Close` as a session-death equivalent (a real process kill of the test harness is self-defeating), with a separate true-`SIGKILL` subprocess drill at lower iteration count. Both are documented as such in the checklist rather than being presented as 500 real SIGKILLs — the right call.
- **Trust Checklist rewrite is genuinely honest**, and written for its stated reader ("someone deciding whether to trust this product with their only copy"). Every ✅ names a specific test; every ⚠️ says which half is proven (#2 portable vs Windows ACL/ADS; #4 crash-equivalent vs true SIGKILL; #12 index rebuild vs full container drill); Windows items are plainly ❌. No item is green on evidence that does not exist.
- Seeded and reproducible: seeds printed for the fuzz drills, per the standard set after the M2 flaky-test episode.
- CI mirrors the engine-gate pattern: reduced matrix every push, `CHAOS_FULL=1` nightly + dispatch.

## Suggested next step

1. CHAOS-F2 — wire notify into `breakwaterd` with an end-to-end test.

Remaining gaps are correctly attributed and not actionable here: Trust Checklist #1, #11 (VSS) and the Windows half of #2 need the VM; #13 (cold runbooks) and the full container server-loss drill are M6.

---

## Disposition (CHAOS-F2, append-only)

| ID | Status | Notes |
|----|--------|-------|
| CHAOS-F2 | ✅ Fixed | `wireAlerting` in `breakwaterd`: SMTP from catalog settings (redacted logs; unconfigured → startup WARN + `LogSender`); failure alerts via `OnJobTerminal` (non-blocking enqueue); watchdog + daily digest on injected clock (`alertScheduler.RunOnce`); retention scrub shares Notifier. E2E through construction path: `TestCHAOS_F2_FailureAlertThroughWireAlerting`, `TestCHAOS_F2_WatchdogThroughWireAlerting`, `TestCHAOS_F2_DigestThroughWireAlerting`. `releaseLease` always fires terminal hooks (matches prior comment; enables alerts for all terminal paths). Trust Checklist #10 → ✅. |

### Mutation evidence (self-check)

```
# Unwire failure OnJobTerminal:
--- FAIL: TestCHAOS_F2_FailureAlertThroughWireAlerting
    no message kind=failure within 3s (have 0)

# No-op alertScheduler.RunOnce:
--- FAIL: TestCHAOS_F2_WatchdogThroughWireAlerting
    no message kind=watchdog within 3s (have 0)

# Restored wiring:
--- PASS: TestCHAOS_F2_FailureAlertThroughWireAlerting
--- PASS: TestCHAOS_F2_WatchdogThroughWireAlerting
```

## Status (after CHAOS-F2)

CHAOS-F1 ✅ · CHAOS-F2 ✅ — chaos matrix accepted; production alerting live in `breakwaterd`.
