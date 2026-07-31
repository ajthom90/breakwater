# Backup Trust Checklist

All items must be green before production points at Breakwater as a sole or
primary backup. This file is written for someone deciding whether to trust this
product with their only copy of their data.

**Status date:** 2026-07-31 (post M5 + Linux chaos matrix).  
**Do not trust Breakwater with sole copies until this checklist is all green.**

| # | Check | Status | Evidence / what still blocks |
|---|--------|--------|------------------------------|
| 1 | Full C: with SQL running captures locked + SYSTEM-only files (comparer-verified) | ❌ | **Windows/M3.** Needs VSS + SeBackupPrivilege on a real Windows target. Not Linux-provable. |
| 2 | Restore byte + ACL + ADS + timestamp identical | ⚠️ partial | **Portable half ✅:** `pkg/restore` + golden comparer on Linux/darwin (bytes, symlinks, timestamps). **Windows half ❌:** ACL/ADS via `BackupWrite` untested (platform stubs). |
| 3 | Cross-machine restore works | ✅ | `TestM4_CrossMachineRestore` + red-first authz tests (`TestM4_RedFirst_*`) in `server/internal/agentgw`. Job-scoped + reachability-scoped. |
| 4 | kill -9 fuzz ≥500 iterations, repo always consistent | ⚠️ partial | **Linux chaos #10 ✅ (reduced every CI; full 500 nightly):** `TestChaos10_Kill9Fuzz` (crash-equivalent `Manager.Close` mid concurrent backup+prune) + `TestChaos10_ProcessKill9` (true `SIGKILL` of worker). Asserts every surviving snapshot fully restores. Seed printed. Nightly: `CHAOS_FULL=1` job in `ci.yml`. |
| 5 | Prune property tests green; grace period enforced | ✅ | `TestProperty_RandomForgetPruneNeverOrphans`, `TestM5_GraceWindowSurvivesPrune` (mutation-killed), `TestProperty_GraceNeverPhysicallyDeletes` in `server/internal/retention`. Grace is structural (`Forget` never touches vault). |
| 6 | Injected corruption detected + alerted | ✅ | Chaos #8: `TestChaos08_BitFlipPack` — pack damage under `p/`, cache cleared, scrub full → `ContentsFailed>0`, non-empty `AffectedSnapshots`, `kind=corruption` notify. Scrub also audits `scrub.corruption`. |
| 7 | ENOSPC non-destructive + alerted | ✅ | Chaos #4: `TestChaos04_ENOSPC` — tiny FS (darwin ram disk / Linux tmpfs), fill near full, write fails with ENOSPC-class error, failure alert fires, **0 partial snapshots** after reopen. |
| 8 | mTLS both directions proven | ✅ | M1: `TestM1_EnrollmentAndWrongCertRejection` (enroll + wrong-cert rejected + audit). Chaos matrix #6 by reference. |
| 9 | Append-only proven from agent credentials | ✅ | Structural :9443 (no destructive RPCs) + `TestM5_AgentHasNoForgetOrPrunePath` (chaos #7 by ref). Agent cannot Submit prune/verify. |
| 10 | Missed/failed backup emails within the window | ⚠️ partial | **Mechanism ✅:** chaos #9 `TestChaos09_MissedBackupWatchdog` + `notify.Watchdog` with FakeSender. Failure alert path exercised in #4. **Production wiring ⏳:** `breakwaterd` does not yet schedule a periodic watchdog/digest loop (notify package is library-proven; M6 wires cron + SMTP config into main). |
| 11 | Zero VSS shadow-copy leaks across 100 runs | ❌ | **Windows/M3.** Chaos #1 Linux half only (`TestChaos01_AgentKilledMidUpload`); VSS leak half gated. |
| 12 | Server-loss drill: metadata rebuilt from repo dir alone; restore succeeds | ⚠️ partial | **Index rebuild ✅:** `TestM4_ServerLossDrill` (wipe catalog snapshots → rescan from repo → restore). **Full container replace ⏳:** still M6 (fresh container + recovery kit end-to-end). |
| 13 | Runbooks executed cold following only the doc | ❌ | **M6.** Runbooks not yet written/executed cold. |

## Chaos drill matrix (Linux-provable)

Implemented under `server/internal/chaos/`. Numbers match [PLAN.md](../PLAN.md) Verification.

| # | Drill | Test | Notes |
|---|--------|------|--------|
| 1 | Kill agent mid-upload | `TestChaos01_AgentKilledMidUpload` | Linux: no partial/duplicate. **VSS leak → Windows/M3.** |
| 2 | Server killed mid-upload | `TestChaos02_ServerKilledMidUpload` | Close mid-`PutContent`; reopen; no partial snapshot; resume OK. |
| 3 | Network partition mid-backup | `TestChaos03_NetworkPartitionMidBackup` | Injected fail counter; heal; no duplicate manifests/rows. |
| 4 | Server ENOSPC | `TestChaos04_ENOSPC` | Real tiny FS; alert; repo has 0 partial snaps. |
| 5 | Agent clock ±3d | `TestChaos05_AgentClockSkew` | Server clock governs vault + catalog; warn in log+audit. **Fixed defect CHAOS-F1.** |
| 6 | Token/cert pinning | *by ref* `TestM1_EnrollmentAndWrongCertRejection` | Matrix index asserts file still contains evidence. |
| 7 | Agent cannot prune | *by ref* `TestM5_AgentHasNoForgetOrPrunePath` | Same. |
| 8 | Bit-flip pack → scrub | `TestChaos08_BitFlipPack` | Affected snapshots + corruption alert. |
| 9 | Machine silent → watchdog | `TestChaos09_MissedBackupWatchdog` | Email kind=watchdog. |
| 10 | kill -9 fuzz backup+prune | `TestChaos10_Kill9Fuzz` + `ProcessKill9` | Flagship; 500 full / reduced CI. |

**CI:** reduced matrix on every push (`-short`); full (`CHAOS_FULL=1`) nightly + `workflow_dispatch` (mirrors engine-gate pattern).

## Defects found by this verification pass

### CHAOS-F1 · Agent clock could skew retention timestamps (fixed)

**Drill:** #5 (`TestChaos05_AgentClockSkew`).  
**Bug:** `CommitSnapshot` used agent `FinishedAt` as vault `SnapshotRecord.Timestamp`. A compromised or mis-set agent clock (±3 days) could distort GFS keep-set inputs if catalog ever mirrored that time. Catalog historically used SQL `now` (server), but vault timestamps and any future path that preferred vault time were agent-influenced.  
**Fix:** server `Clock` always sets vault Timestamp and catalog `created_at`; agent time kept only in `Extra` + audit; warn when `|skew| > 1h`.  
**Mutation:** temporarily trusting agent `FinishedAt` → drill **FAILS** (`catalog created_at` equals agent time); restore → **PASS**.

## How to re-run

```sh
# Reduced (every push / local)
cd server && go test ./internal/chaos/ -count=1 -short -race -timeout 15m -v

# Full flagship fuzz (nightly)
CHAOS_FULL=1 go test ./internal/chaos/ -count=1 -race -timeout 60m -v

# Single drill with fixed seed
CHAOS_SEED=42 go test ./internal/chaos/ -count=1 -run TestChaos10_Kill9Fuzz -v
```
