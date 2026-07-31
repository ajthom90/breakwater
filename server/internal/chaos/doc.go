// Package chaos implements the PLAN Verification → Chaos drill matrix for the
// Linux-provable subset (M5–M6). This is verification work: evidence, not features.
//
// Drill numbering matches PLAN.md. Windows-only halves are marked explicitly.
//
//	#1  kill agent mid-upload → resume, no partial/duplicate (VSS leak: Windows/M3)
//	#2  server killed mid-upload → repo consistent, no partial snapshot
//	#3  network partition mid-backup → retry, no duplicate manifests/rows
//	#4  server ENOSPC → clean fail + alert, repo untouched
//	#5  agent clock ±3d → server clock governs timestamps + retention; warning
//	#6  token reuse / unknown cert / server-cert-swap (by reference → agentgw M1)
//	#7  agent cannot delete/prune (by reference → TestM5_AgentHasNoForgetOrPrunePath)
//	#8  bit-flip pack → scrub identifies affected snapshots + alert
//	#9  machine silent across window → watchdog email
//	#10 kill -9 fuzz (≥500) around backup+prune → never remove referenced data
//
// Iteration counts:
//
//	-short / default CI: reduced (CHAOS_ITERS or package defaults)
//	CHAOS_FULL=1 or nightly job: full count (500 for #10)
//
// Every drill must prove the fault was injected (vacuous green is a failure).
// Seeds are printed for deterministic reproduction.
package chaos
