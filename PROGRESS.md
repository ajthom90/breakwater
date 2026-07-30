# Breakwater — Implementation Progress

Single tracking file for milestone status, decisions, and deviations from [PLAN.md](PLAN.md).

## Current milestone

**Phase 1 — M1 (weeks 1–2)** — ✅ **COMPLETE** (2026-07-30; round-2 review fixes applied same day)

Addressed [REVIEW-M1.md](REVIEW-M1.md) (round 1) and [REVIEW-M1-ROUND2.md](REVIEW-M1-ROUND2.md) (round 2) against commits `f92837f` → `755f417` → this fix set. All 15 round-2 findings (R2-1…R2-15) fixed; engine gate re-proven at full 10 GiB with recursive mark, min-age guard, and on-disk byte reclamation.

### M1 deliverables

| Item | Status | Evidence |
|------|--------|----------|
| Git monorepo layout | ✅ | Matches PLAN.md repo layout |
| Legal/docs (MIT, notices, SECURITY, CONTRIBUTING/DCO, README banner) | ✅ | Root files |
| **Engine decision gate** (kopia vault) | ✅ **PASSED** (10 GiB + R2-13/R2-15) | See verification evidence below |
| Proto frozen + generated Go | ✅ | `proto/` + `pkg/proto/` (+ additive `hashing_algorithm`) |
| SQLite catalog + **incremental** migrations | ✅ | v1→v2 upgrade path (R2-8) |
| Enrollment tokens + mTLS pin | ✅ | TLS peer bind (B2); typed errors (R2-11); compensate (R2-9) |
| Fake Linux client enroll; wrong-cert rejected | ✅ | `TestM1_EnrollmentAndWrongCertRejection` |
| Dockerfile | ✅ | `packaging/docker/` |
| CI (Linux + race + pkg + reduced gate; full gate nightly + dispatch) | ✅ | `.github/workflows/ci.yml` `on:` has schedule + workflow_dispatch (R2-7) |
| PROGRESS.md | ✅ | This file |

### Engine gate (honest claim)

PLAN criterion: write chunked data → restore → verify → retention + **GC that reclaims space**.

Implementation (`server/internal/vault/kopia.go` `Prune`):

1. **Mark (recursive, R2-1):** live `bw-*` snapshot manifests → kind-keyed walk:
   - file: decode `TreeObject`, mark root + recurse dirs / `VerifyObject` files+ADS
   - image: decode `ImageManifest`, mark root + every block `ContentID`
   - decode failure / unknown kind → fail closed (R2-3)
2. **Sweep (R2-2):** `IterateContents` → `DeleteContent` unmarked unprefixed IDs **older than MinContentAge** (default 24h; tests/gate pass `WithMinContentAge(0)`)
3. Commit session, then `DropDeletedContents(SafetyNone)` + `maintenance.Run(ModeFull, SafetyNone)`

Assertions in `TestEngineGate_Kopia`: forgotten contents absent, `UserContentCount`/`UserSizeBytes` shrink, **on-disk bytes shrink (R2-13)**, live object checksum-verifies after prune and re-open. Prune-survival tests cover indirect trees + image manifests (R2-15).

```
# Reduced (CI / local loops)
BW_GATE_BYTES=268435456 go test ./internal/vault/ -run TestEngineGate_Kopia -v
# Full 10 GiB
go test ./internal/vault/ -run TestEngineGate_Kopia -timeout 45m -v
```

### M1 demo

```
go test ./internal/agentgw/ -run 'TestM1_EnrollmentAndWrongCertRejection|TestEnroll_BodyCertMismatch' -v
```

---

## Decisions made (M1)

1. **Storage engine = kopia bottom half** (v0.19.0) behind `server/internal/vault`.  
2. **PutContent** via `object.Writer` + FIXED-4M; hard max **4MiB**; VerifyObject error propagated (H2).  
3. **Prune = recursive mark-and-sweep** as PLAN specifies (trees/image manifests, not flat roots only); min-age default 24h; two write sessions so delete markers commit before index refresh (B1 / R2-1 / R2-2).  
4. **Enrollment identity** bound to **TLS peer cert FP** (`PeerFromContext`); body PEM if present must match (B2).  
5. **Hashing key + algorithm** from `ContentFormat().GetHmacSecret()` / `GetHashFunction()` after vault create — on the wire (`hashing_algorithm = 5`) and persisted in keystore (H1 / R2-5).  
6. **Module path** `github.com/ajthom90/breakwater/...`.  
7. **kopia pin v0.19.0** — works on Go 1.23; v0.23.x requires Go ≥1.25.8. Upgrade when toolchain allows (M1 deferred).  
8. **Master key** 32-byte file; per-repo password + hashing key sealed with NaCl secretbox.  
9. **Catalog migrations** are versioned incremental from v2 onward (R2-8); full `schema.sql` for fresh installs.

## Deviations from PLAN.md

| Deviation | Rationale |
|-----------|-----------|
| Enrollment JSON codec + hand-written service | M1 demo; swap to generated `breakwater.v1` first in M2 (**M13**) |
| kopia not vendored | Pinned in go.mod; vendor before v0.1.0 |
| kopia v0.19.0 not v0.23.x | Go 1.23 toolchain; see decision #7 |
| `breakwater.config`/`.cache` under repo path | M4 deferred to M2 (move under `/data`) |
| Web port plain HTTP | M1 healthz only; HTTPS required before auth UI (M11) |

---

## REVIEW-M1 disposition (round 1)

| ID | Status | Notes |
|----|--------|-------|
| B1 prune mark-sweep | ✅ Fixed (round 2 completed) | Round 1 flat-only mark was incomplete — see R2-1; recursive mark + survival tests now green at 10 GiB |
| B2 TLS peer bind | ✅ Fixed | + `TestEnroll_BodyCertMismatch` |
| H1 hashing key | ✅ Fixed (round 2 completed) | Secret from ContentFormat; algorithm now on wire + keystore (R2-5); ID round-trip (R2-14) |
| H2 PutContent >4MiB | ✅ Fixed | Guard + test |
| H3 CI/gofmt/race/pkg | ✅ Fixed | Reduced gate on PR; full gate nightly + workflow_dispatch (R2-7) |
| M3 Close nil-guard | ✅ Fixed | |
| M5 server cert leaf | ✅ Fixed | Not CA |
| M6 enroll error codes | ✅ Fixed (R2-11) | Typed errors → InvalidArgument/PermissionDenied/Internal |
| M7 enroll ordering | ✅ Fixed (R2-9) | Compensate: release token + delete keystore on failure |
| M8 ListSnapshot Timestamp | ✅ Fixed | + R2-12 fail on GetManifest error |
| M9 enroll_tokens UNIQUE | ✅ Fixed | + incremental migration index (R2-8) |
| M10 docs/actor_type | ✅ Fixed | README + column + migration |
| M12 drop pkg/errors direct dep | ✅ Fixed | |
| M1 kopia version pin | ⏳ Deferred | Documented; upgrade with Go 1.25+ |
| M2 OpenObject vs prune | ⏳ Deferred M2 | Documented on Vault interface (+ backup-vs-prune serialization R2-2) |
| M4 config/cache under /data | ⏳ Deferred M2 | |
| M11 web HTTPS | ⏳ Deferred M2 | |
| **M13 ForceServerCodec JSON** | ⏳ **Must fix first in M2** | Proto clients will fail until removed |

---

## REVIEW-M1-ROUND2 disposition

| ID | Status | Evidence |
|----|--------|----------|
| R2-1 recursive mark | ✅ Fixed | `markTreeObject` / `markImageManifest` in `kopia.go`; `TestPruneSurvivesIndirectTreeReferences` + image test green |
| R2-2 min-age + serialization contract | ✅ Fixed | `DefaultPruneMinContentAge=24h`; `WithMinContentAge`; Vault interface docs; `TestPruneMinAgeProtectsInFlightBackup` |
| R2-3 kind validation + fail-closed | ✅ Fixed | `PutSnapshotRecord` rejects unknown kinds; mark enumerates all `bw-*` and fails unknown |
| R2-4 RootObjectID at write | ✅ Fixed | `object.ParseID` in `PutSnapshotRecord`; mark errors name manifest ID |
| R2-5 hashing_algorithm wire+persist | ✅ Fixed | proto field 5; gateway; keystore column + Set/Get |
| R2-6 ErrHashingKeyNotSet | ✅ Fixed | empty unseal → sentinel; test |
| R2-7 CI schedule + dispatch | ✅ Fixed | `on.schedule` + `on.workflow_dispatch` in `ci.yml` |
| R2-8 incremental migrations | ✅ Fixed | v1→v2; `TestUpgradeFromV1` |
| R2-9 enroll compensate | ✅ Fixed | `ReleaseEnrollToken` + `DeleteRepo`; `TestEnroll_CompensateOnVaultFailure` |
| R2-10 manager closed cache | ✅ Fixed | Open/Create evict closed; `Manager.Close`; `TestManager_ReopenAfterClose` |
| R2-11 typed enroll errors | ✅ Fixed | `mapEnrollError` in gateway |
| R2-12 ListSnapshot error | ✅ Fixed | GetManifest error propagated |
| R2-13 on-disk bytes | ✅ Fixed | reclaim + engine gate assert disk shrink |
| R2-14 ID reproduction | ✅ Fixed | `TestHashingKeyReproducesContentIDs` PASS |
| R2-15 prune-survival | ✅ Fixed | tree + image tests PASS after R2-1 (failed red-first) |

---

## Next: M2 (weeks 3–4)

**Do first (protocol debt):** swap enrollment to generated `breakwater.v1.EnrollmentService`, remove `grpc.ForceServerCodec(jsonCodec{})` and hand-written JSON service/codec (**M13**).

Then:

- Windows agent service (SYSTEM) + WiX MSI  
- Persistent dial-out + keepalives  
- Server-dispatched jobs  
- Plain-directory backup (chunk → have/want → append-only upload → manifest)  
- UI shell against fake API  
- Golden-dataset generator + comparer  
- Audit middleware (start with `machine.enroll`)  
- HTTPS on web port before authenticated surface  
- Per-repo job serialization covering open restore streams **and backup-vs-prune** (R2-2)  
- Vendor kopia; consider v0.23 when on Go 1.25+  
- Move `breakwater.config`/`.cache` under `/data` (M4)  

*Demo: MSI install → appears in UI in 10s → backup → second run shows dedup ratio.*

---

## Verification evidence (REVIEW-M1-ROUND2)

Run 2026-07-30 after round-2 fixes (darwin/arm64, Go 1.23.x):

### Red-first (R2-15 against unmodified `755f417` mark — must fail)

```
=== RUN   TestPruneSurvivesIndirectTreeReferences
    prune_survival_test.go: OpenObject …: content … not found: object not found
--- FAIL: TestPruneSurvivesIndirectTreeReferences
=== RUN   TestPruneSurvivesImageManifestBlocks
    prune_survival_test.go: block1 missing after prune: content not found
--- FAIL: TestPruneSurvivesImageManifestBlocks
=== RUN   TestPruneMinAgeProtectsInFlightBackup
    prune_survival_test.go: OpenObject …: content … not found: object not found
--- FAIL: TestPruneMinAgeProtectsInFlightBackup
```

(R2-14 hashing round-trip and R2-13 on-disk reclaim already green on 755f417 for flat snapshots.)

### After fixes

```
=== gofmt -l (must be empty) ===
(empty)

=== go vet ===
(clean)

=== short+race ===
ok  server/internal/agentgw
ok  server/internal/catalog
ok  server/internal/enroll
ok  server/internal/keystore
ok  server/internal/mtls
ok  server/internal/vault

=== pkg tests ===
ok  pkg/format

=== Prune|Reclaim ===
--- PASS: TestPruneReclaimsForgottenContent (disk 945010 → 17023)
--- PASS: TestPruneSurvivesIndirectTreeReferences
--- PASS: TestPruneSurvivesImageManifestBlocks
--- PASS: TestPruneMinAgeProtectsInFlightBackup

=== reduced gate 256MB ===
stats before prune: user_contents=54 user_size=241641026
stats after prune:  user_contents=52 user_size=237922667
disk bytes before=241706425 after=237980292
forgotten object unreadable; live checksum OK; re-open OK
--- PASS: TestEngineGate_Kopia

=== full 10GB gate ===
engine gate: writing 10737418240 bytes (10.00 GiB)
hashing key: algo=BLAKE2B-256-128 secretLen=32
WriteObject done in ~1m28s; restore verified ~14s; VerifyObject: 1963 pieces
stats before prune: user_contents=1964 user_size=9520670688
stats after prune:  user_contents=1962 user_size=9516952329
disk bytes before=9521915904 after=9518185675
forgotten object unreadable; live checksum OK; re-open OK
reclamation OK: user contents 1964→1962; disk shrink
--- PASS: TestEngineGate_Kopia (163.64s)

=== CI on: ===
schedule: cron "0 6 * * *"
workflow_dispatch:
```

**Engine gate: PASSED** at full 10 GiB with R2-13 on-disk and R2-15 survival coverage (recursive mark).

---

## Trust Checklist status

See [docs/trust-checklist.md](docs/trust-checklist.md). **Not production-ready.** Item 8 (mTLS) green from M1 (including body/connection mismatch).
