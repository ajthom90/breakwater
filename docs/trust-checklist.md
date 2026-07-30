# Backup Trust Checklist

All items must be green before production points at Breakwater as a sole or primary backup.

Copied from [PLAN.md](../PLAN.md). Status as of M1:

| # | Check | Status |
|---|--------|--------|
| 1 | Full C: with SQL running captures locked + SYSTEM-only files (comparer-verified) | ❌ M3+ |
| 2 | Restore byte+ACL+ADS+timestamp identical | ❌ M4 |
| 3 | Cross-machine restore works | ❌ M4 |
| 4 | kill -9 fuzz ≥500 iterations, repo always consistent | ❌ M5–M6 |
| 5 | Prune property tests green; grace period enforced | ❌ M5 |
| 6 | Injected corruption detected + alerted | ❌ M5 |
| 7 | ENOSPC non-destructive + alerted | ❌ M5–M6 |
| 8 | mTLS both directions proven | ✅ M1 (enrollment + wrong-cert rejection) |
| 9 | Append-only proven from agent credentials | ⏳ structural port design; full proof M2+ |
| 10 | Missed/failed backup emails within the window | ❌ M5 |
| 11 | Zero VSS shadow-copy leaks across 100 runs | ❌ M3 |
| 12 | Server-loss drill: metadata rebuilt from repo dir alone | ❌ M6 |
| 13 | Runbooks executed cold following only the doc | ❌ M6 |

**Do not trust Breakwater with sole copies until this checklist is all green.**
