# Breakwater — Implementation Progress

Single tracking file for milestone status, decisions, and deviations from [PLAN.md](PLAN.md).

## Current milestone

**Phase 1 — M4 (restore path; PLAN wk7–8, pulled forward)** — ✅ **COMPLETE** on portable Linux/darwin evidence (2026-07-30)

**Phase 1 — M3 (VSS; PLAN wk5–6)** — ⏸️ **BLOCKED** on Windows VM availability (not available yet). M4 does not depend on VSS: it restores plain-directory backups M2 already produces.

**Phase 1 — M2 (weeks 3–4)** — ✅ **COMPLETE** on Linux/darwin evidence (stage 5 + S5-F1/F2; Windows demo subset still gated — see [M2 closeout](#m2-closeout))

**Phase 1 — M1 (weeks 1–2)** — ✅ **COMPLETE** (2026-07-30; closed at `161fb18`)

### Milestone reorder (M3 ↔ M4)

M2 closed green at `4a0bf14`. M3 (VSS) is almost entirely Windows-runtime work and is blocked until a Windows VM is available. **M4 was pulled forward** so the product can restore through its own API on Linux/darwin now — the single biggest gap for a backup product, and almost all of it is portable. Recorded here so PLAN week numbers stay as written while execution order is honest.

Addressed [REVIEW-M1.md](REVIEW-M1.md), [REVIEW-M1-ROUND2.md](REVIEW-M1-ROUND2.md), and [REVIEW-M1-ROUND3.md](REVIEW-M1-ROUND3.md) against `f92837f` → `755f417` → `eea1a46` → this fix set. Round-3 closed remaining fail-open mark heuristic, canceled-ctx compensation, empty-algorithm upgrade hole, and related mediums.

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

1. **Mark (recursive, R2-1 / R3-1):** live `bw-*` snapshot manifests → kind-keyed walk:
   - file: decode `TreeObject` (write-boundary validated; **decode failure always fails prune** — no leaf heuristic), mark root + recurse dirs / `VerifyObject` files+ADS
   - image: decode `ImageManifest`, mark root + every block `ContentID`
   - unknown kind → fail closed (R2-3); root reads capped at `MaxMarkObjectBytes` 16 MiB (R3-2)
   - gate/tests store **TreeObject roots** wrapping payload objects (not flat raw-byte roots)
2. **Sweep (R2-2 / R3-5):** `IterateContents` → `DeleteContent` unmarked unprefixed IDs **older than MinContentAge** (default 24h; tests/gate pass `WithMinContentAge(0)`)
3. Commit session, then `DropDeletedContents` + `maintenance.Run` with `SafetyParameters` derived from min-age when >0; `SafetyNone` only for explicit zero min-age

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
| Enrollment JSON codec + hand-written service | ✅ Closed M2 stage 1 (M13) — generated `breakwater.v1` |
| kopia not vendored | Pinned in go.mod; vendor before v0.1.0 |
| kopia v0.19.0 not v0.23.x | Go 1.23 toolchain; see decision #7 |
| `breakwater.config`/`.cache` under repo path | ✅ Closed M2 stage 1 (M4) — under `<dataDir>/kopia-config` + `cache` |
| Web port plain HTTP | ✅ Closed M2 stage 1 (M11) — HTTPS via server identity leaf |

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
| M2 OpenObject vs prune | ✅ Fixed M2 stage 2 | `scheduler.RepoLocks` job-scoped shared/exclusive; restore holds shared until terminal |
| M4 config/cache under /data | ✅ Fixed M2 stage 1 | `<dataDir>/kopia-config/<id>.config` + `cache/<id>`; legacy migrate |
| M11 web HTTPS | ✅ Fixed M2 stage 1 | `ListenAndServeTLS` with server identity leaf |
| **M13 ForceServerCodec JSON** | ✅ Fixed M2 stage 1 | Generated `EnrollmentService`; codec deleted |

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

## REVIEW-M1-ROUND3 disposition

| ID | Status | Evidence |
|----|--------|----------|
| R3-1 fail-closed mark / TreeObject roots | ✅ Fixed | Write-boundary `validateSnapshotRoot`; `markTreeObject` always fails decode; deleted `looksLikeTreeJSON`/helpers; gate wraps payload in TreeObject; `TestPutSnapshotRecord_RejectsFlatFileRoot` + `TestMarkTreeObject_UndecodableRootFailsClosed` |
| R3-2 bounded root reads | ✅ Fixed | `MaxMarkObjectBytes=16MiB` + `io.LimitReader`; over-limit fail-closed |
| R3-3 compensate on fresh ctx | ✅ Fixed | `context.WithTimeout(Background, 5s)`; `TestEnroll_CompensateDespiteCanceledContext` |
| R3-4 ErrHashingAlgorithmNotSet | ✅ Fixed | empty algo with non-empty key → sentinel; `TestGetHashingKey_EmptyAlgorithmIsError` |
| R3-5 SafetyParameters from min-age | ✅ Fixed | `safetyForMinAge`; SafetyNone only for `WithMinContentAge(0)` |
| R3-6 Manager Close/Open race | ✅ Fixed M2 stage 2 | `RepoLocks.WithExclusive` for Close/Open; Manager docs reference lease requirement |
| R3-7 keystore row assert | ✅ Fixed | `failingVault.repoID` + COUNT=0 in compensate tests |
| R3-8 gRPC status tests | ✅ Fixed | `TestEnroll_gRPCStatusCodes` |
| R3-9 755f417 migration fixture | ✅ Fixed | `TestUpgradeFrom755f417` |

---

## M2 progress

### Stage 1 — protocol swap, audit, HTTPS, vault housekeeping (2026-07-30)

Server-side foundations buildable/testable on Linux/macOS. Wire contract uses real protobuf.

| Deliverable | Status | Evidence |
|-------------|--------|----------|
| **M13** retire JSON codec; serve generated `EnrollmentService` | ✅ | `jsoncodec.go` deleted; no `ForceServerCodec`; tests use `breakwaterv1` stubs |
| Audit middleware (`server/internal/audit`) | ✅ | Hash-chained writer; `VerifyChain` + tamper + concurrency tests; `machine.enroll` + `auth.fail` |
| HTTPS on :8443 (M11) | ✅ | `ListenAndServeTLS` with server leaf; plain HTTP gone |
| Config/cache under `/data` (M4) | ✅ | `kopia-config/<repoID>.config` + `cache/<repoID>`; legacy migrate test |
| Strict root validation (R3 note 1) | ✅ | `DisallowUnknownFields` at write boundary **and** mark phase; cross-kind test |
| **S1-F1…F3 fix round** (review `REVIEW-M2-S1.md`) | ✅ | WithoutCancel audit; length-prefix canonical; trailing-EOF decode — see below |

#### Fix round S1-F1…F3 (post-bc65f8a review)

| ID | Fix | Evidence |
|----|-----|----------|
| S1-F1 | `context.WithoutCancel(ctx)` for all audit appends; always log failures | `TestEnroll_AuditDespiteCanceledContext` + `TestAuthFail_AuditDespiteCanceledContext` PASS |
| S1-F2 | Length-prefixed canonical fields (`<len>:<bytes>`); no migration (no real deploys) | `TestCanonicalEncoding_NoAmbiguity` PASS |
| S1-F3 | `strictJSONDecode` requires EOF after first value | `TestPutSnapshotRecord_RejectsTrailingGarbage` PASS |

##### Red-first against unmodified `bc65f8a`

```
=== RUN   TestEnroll_AuditDespiteCanceledContext
    … expected machine.enroll audit row after canceled-ctx enroll failure; got none (S1-F1)
--- FAIL: TestEnroll_AuditDespiteCanceledContext
=== RUN   TestAuthFail_AuditDespiteCanceledContext
    … expected auth.fail audit row despite pre-canceled ctx; got none (S1-F1)
--- FAIL: TestAuthFail_AuditDespiteCanceledContext

=== RUN   TestCanonicalEncoding_NoAmbiguity
    old encoding collides as expected (len=26)
    length-prefixed encoding still collides: 1="id\nts\n…" 2="id\nts\n…"
--- FAIL: TestCanonicalEncoding_NoAmbiguity

=== RUN   TestPutSnapshotRecord_RejectsTrailingGarbage
    … must reject TreeObject with trailing garbage after JSON value (S1-F3)
--- FAIL: TestPutSnapshotRecord_RejectsTrailingGarbage
```

##### After fixes (review verification commands)

```
gofmt / go vet / short+race     # clean / all ok
go test ./internal/audit/ -v    # PASS incl. NoAmbiguity + Surface
go test ./internal/agentgw/ -run 'TestM1_|TestEnroll_' -v  # PASS (+ AuditDespiteCanceled)
go test ./internal/vault/ -run 'Root|Strict|Trailing' -v   # PASS trailing garbage
BW_GATE_BYTES=268435456 TestEngineGate_Kopia               # PASS
pkg tests                                                   # PASS
```

#### Red-first (strict root — loose decode must accept cross-kind)

Against temporary `json.Unmarshal` (pre-strict) the new test fails as required:

```
=== RUN   TestPutSnapshotRecord_RejectsCrossKindRoot
    root_strict_test.go:56: PutSnapshotRecord must reject TreeObject root under bw-image-snapshot (cross-kind)
--- FAIL: TestPutSnapshotRecord_RejectsCrossKindRoot (0.39s)
```

After `strictJSONDecode` (`DisallowUnknownFields`):

```
--- PASS: TestPutSnapshotRecord_RejectsCrossKindRoot
    cross-kind TreeObject-as-image rejected: … unknown field "entries"
    cross-kind ImageManifest-as-file rejected: … unknown field "block_size"
```

Mark-phase decoders use the same `strictJSONDecode` (same write-boundary contract).

#### Verification (stage 1)

```
gofmt -l server pkg agent cli restore   # empty
go vet ./...                            # clean
go test ./... -short -race              # all ok (incl. audit, agentgw)
go test ./internal/agentgw/ -run 'TestM1_|TestEnroll_' -v  # PASS (protobuf)
go test ./internal/audit/ -v            # PASS (tamper + concurrent chain)
go test ./internal/vault/ -run 'Prune|Reclaim|Root|Config|Migrate' -v  # PASS
BW_GATE_BYTES=268435456 … TestEngineGate_Kopia  # PASS (~4s)
full 10 GiB TestEngineGate_Kopia                # PASS (124s)
grep ForceServerCodec\|jsonCodec server/        # OK (gone)
```

---

### Stage 2 — control plane: Channel, scheduler, per-repo serialization (2026-07-30)

Server-side control plane, buildable/testable on Linux/macOS with a fake Go agent.
Stage-4 Windows agent will bind to this contract.

| Deliverable | Status | Evidence |
|-------------|--------|----------|
| **ControlService.Channel** (Hello/HB/Progress/Result/Inventory; JobStart/Cancel) | ✅ | `server/internal/agentgw/channel.go` + registry |
| Cert↔machine bind on Hello (cross-machine isolation) | ✅ | `TestM2S2_HelloMachineMismatch` |
| One live channel per machine; new supersedes old | ✅ | `Registry.Register` closes prior session |
| Online/offline + last_seen on machines table | ✅ | `SetMachineOnline`/`Offline`; demo asserts active→enrolled |
| gRPC keepalive 30s (server) + client expectation docs | ✅ | `KeepaliveServerParameters` + channel package comment |
| Idempotent reconnect (no re-JobStart; duplicate Result no-op) | ✅ | engine + `TestM2S2_ControlPlaneDemo` + engine unit tests |
| Job engine: create/dispatch/progress/complete/fail/cancel | ✅ | `server/internal/scheduler/engine.go` |
| Types: `inventory` (real), `noop` (test); open registry | ✅ | `types.go`; prune server-only rejected |
| Offline queue (bound 64, oldest-first) | ✅ | `MaxPendingJobsPerMachine`; offline deliver test |
| Inventory → machine_inventory | ✅ | demo + `HandleInventory` |
| Per-repo RW locks (shared backup/restore; exclusive prune) | ✅ | `repolock.go`; red-first + race tests |
| Lease released on fail/cancel/disconnect | ✅ | `TestEngine_Disconnect…` + `TestM2S2_DisconnectReleasesRunningLease` |
| Audit policy documented (no channel noise; job.* deferred) | ✅ | `server/internal/audit` package comment |
| **Demo** `TestM2S2_ControlPlaneDemo` | ✅ | PASS under `-race` |
| **S2-F1…F8 fix round** (review `REVIEW-M2-S2.md`) | ✅ | See below |

#### Fix round S2-F1…F8 (post-70e26a2 review)

| ID | Fix | Evidence |
|----|-----|----------|
| S2-F1 | Skip-and-log malformed inventory; app errors not stream-fatal | `TestS2F1_MalformedInventoryDoesNotKillChannel` PASS |
| S2-F2 | Undelivered JobStart → pending on session close; queue-full → pending | `TestS2F2_Supersede…` + `TestS2F2_QueueFull…` PASS |
| S2-F3 | Writer preference: Shared blocks while exclusiveWaiters > 0 | `TestS2F3_ExclusiveNotStarvedBySharedChain` PASS |
| S2-F4 | JobResult only for `running` (pending ignored) | `TestS2F4_ResultIgnoredForPendingJob` PASS |
| S2-F5 | `Engine.RecoverOnStartup` → fail orphaned running; called from main | `TestS2F5_RecoverOnStartup` PASS |
| S2-F6 | `Dispatcher.SendJobCancel` wired into `Cancel`; stage-3 lease doc | `TestS2F6_CancelSendsJobCancel` PASS |
| S2-F7 | Additive `JOB_TYPE_INVENTORY=6`, `JOB_TYPE_NOOP=7`; regenerate; agent docs | WireJobType + fake agent on type |
| S2-F8 | Heartbeat re-asserts `SetMachineOnline` | channel Heartbeat handler |

##### Red-first against unmodified `70e26a2` (+ compile stubs)

```
=== RUN   TestS2F1_MalformedInventoryDoesNotKillChannel
    … heartbeat: EOF
--- FAIL: TestS2F1_MalformedInventoryDoesNotKillChannel
    (persist inventory hard-error → stream-fatal → agent disconnected)

=== RUN   TestS2F2_SupersedeRevertsUndeliveredJobStart
    … undelivered JobStart after supersede: state=running want pending (job wedged in running)
--- FAIL: TestS2F2_SupersedeRevertsUndeliveredJobStart
=== RUN   TestS2F2_QueueFullRevertsToPending
    … queue-full hard-failed job (want pending): state=failed
--- FAIL: TestS2F2_QueueFullRevertsToPending

=== RUN   TestS2F3_ExclusiveNotStarvedBySharedChain
    … exclusive starved/failed: context deadline exceeded
--- FAIL: TestS2F3_ExclusiveNotStarvedBySharedChain

=== RUN   TestS2F4_ResultIgnoredForPendingJob
    … pending job terminal-ized by result: state=success want pending
--- FAIL: TestS2F4_ResultIgnoredForPendingJob

=== RUN   TestS2F5_RecoverOnStartup
    … orphaned running state=running want failed
--- FAIL: TestS2F5_RecoverOnStartup
```

##### After fixes

```
TestS2F1… TestS2F2… TestS2F3… TestS2F4… TestS2F5… TestS2F6…  # PASS
gofmt / go vet / short+race / scheduler -race / agentgw M1+M2S2 / gate 256MB / pkg  # all green
```

#### Red-first (serialization + reconnect — stage-2 original)

Unserialised stub allows backup+prune critical-section overlap; real `RepoLocks` forbids it:

```
=== RUN   TestRepoLock_RedFirst_UnserialisedOverlaps
    RED-FIRST evidence: unserialised stub peak overlap=2 (backup+prune concurrent)
--- PASS: TestRepoLock_RedFirst_UnserialisedOverlaps
=== RUN   TestRepoLock_BackupPruneNeverOverlap
    serialized backup+prune peak overlap=1 (want ≤1)
--- PASS: TestRepoLock_BackupPruneNeverOverlap
```

Idempotent reconnect: `DeliverPending` does not re-send JobStart for running jobs;
duplicate `JobResult` on terminal state is a no-op (`TestEngine_ReconnectDoesNotRedispatchRunning`,
demo section).

#### Deviations / notes (stage 2)

| Item | Note |
|------|------|
| `inventory` / `noop` JobType | ✅ Closed S2-F7 — additive `JOB_TYPE_INVENTORY=6` / `JOB_TYPE_NOOP=7` (R2-5 freeze precedent). Stage-4 agent branches on `JobStart.type`. Server still stores `kind` in params_json as catalog convenience only. |
| Enrollment Create vs RepoLocks | Create at enroll remains outside leases: brand-new repo ID, no concurrent job can exist. Documented on `Manager` and `scheduler` package. Subsequent Open/Close/Prune must take exclusive via `RepoLocks.WithExclusive`. |
| Server prune execution | Type registry + lock map ready; no server-side prune runner this stage (stage 3+). Submit of prune for agent dispatch is rejected and tested. |
| Cancel lease release | Stage 2 releases lease immediately on Cancel. Stage 3 must move vault-touching types to agent-confirmation / teardown (documented on `Engine.Cancel`). |
| UpdateOffer | Not sent; agents may ignore if received. |
| Job audit events | `job.run_manual` / `job.cancel` deferred until web surface; engine stores `initiator` in params_json. |

#### Verification (stage 2 + fix round)

```
gofmt -l server pkg agent cli restore   # empty
go vet ./...                            # clean
go test ./... -short -race              # all ok (incl. scheduler, agentgw)
go test ./internal/scheduler/ -race -v  # PASS (S2-F3/4/5/6 + locks + engine)
go test ./internal/agentgw/ -race -run 'TestM1_|TestEnroll_|TestM2S2_|TestS2F' -v  # PASS
BW_GATE_BYTES=268435456 TestEngineGate_Kopia  # PASS
pkg tests                               # PASS
```

---

### Stage 3 — data plane: DataService, content IDs, backup pipeline (2026-07-30)

Append-only agent data path on :9443; agent-side content IDs; portable plain-directory
backup proven by fake agent. Windows agent (stage 4) reuses `pkg/backup` as a library.

| Deliverable | Status | Evidence |
|-------------|--------|----------|
| **pkg/contentid** (keyed hash + DYNAMIC-4M-BUZHASH) | ✅ | `pkg/contentid`; unit tests; vault `TestPkgContentID_RoundTripWithVault` |
| **kopia carve-out** (hashing+splitter only in pkg) | ✅ | `TestKopiaConfinement_PkgOnlyContentID`; pin v0.19.0 |
| **DataService** CheckContents / PutContents / PutTree / PutImage / CommitSnapshot | ✅ | `server/internal/agentgw/data.go` |
| Machine binding + cross-client isolation | ✅ | `TestM2S3_CrossMachineIsolation` (B+A's job denied; CheckContents not oracle) |
| ID-mismatch / oversized batch / content rejection | ✅ | `TestM2S3_IDMismatchRejected`, `TestM2S3_OversizedBatchAndContentRejected` |
| Lease-only vault access (`VaultForJob`) | ✅ | `TestM2S3_LeaseRequiredForVaultAccess`; data plane never Open without lease |
| Append-only (no delete RPCs; PutContents dedup no-op) | ✅ | DataService methods Put/Has/Write/Commit only |
| **snapshot.commit** audit (not per-chunk) | ✅ | audit taxonomy + CommitSnapshot emitter |
| **pkg/backup** portable pipeline | ✅ | walk → CDC → have/want → tree → CommitSnapshot |
| Fake agent FILE_BACKUP | ✅ | demo `onJob` + fileback pipeline |
| Scheduler: non-blocking dispatch lease | ✅ | `TestS3_DispatchLeaseNonBlocking` |
| Scheduler: Cancel lease-on-confirm (vault types) | ✅ | `TestS3_CancelVaultJobHoldsLeaseUntilResult` (cancelling state) |
| Heartbeat pending retrigger | ✅ | Channel Heartbeat → DeliverPending |
| F4 failure-result hardening | ✅ | `TestS2F4_ResultIgnoredForPendingJob` covers Success+Failure |
| **Demo** `TestM2S3_BackupDedupDemo` | ✅ | PASS under `-race` |

#### Decisions (stage 3 + fix round)

1. **kopia confinement (PLAN carve-out, S3-F7 expanded):** `pkg/contentid` may import
   `repo/hashing` + `repo/splitter` only. Enforced across **pkg + agent + cli** by
   `TestKopiaConfinement` (CI wired). Vault still owns content/object/manifest/maintenance.
2. **H2 amendment — MaxPutContentBytes = 8 MiB** (DYNAMIC-4M max segment), up from
   4 MiB FIXED-4M. Named `SplitterFixed8M`. gRPC MaxRecv/SendMsgSize = 16 MiB.
   **Image blocks stay 4 MiB** — `PutImageManifest` rejects other `block_size` (S3-F4).
3. **ObjectFromContents wire (S3-F1):** additive `PutTreeObjectRequest.content_ids = 3`
   (mutually exclusive with `tree_json`). **Sentinel deleted** — no in-band magic names.
4. **Backup library in `pkg/backup`**; agent/internal/fileback re-exports.
5. **Cancel for vault-writing jobs:** running → `cancelling`, lease held until JobResult,
   disconnect, or **CancelConfirmTimeout (2 min, S3-F10)** force-fail + lease release.
6. **tryDispatch claim-before-lease (S3-F9):** pending→running CAS first; only the winner
   acquires Shared. Concurrent DeliverPending cannot leak leases.
7. **Per-file error policy (fail-loud):** I/O errors abort the job. Symlinks stored as
   `EntrySymlink`+`ReparseData`. Unsupported types recorded in `Stats.Skipped` (visible).
   A backup never reports success while silently omitting data (S3-F5).

#### Fix round S3-F1…F10 (post-24300b1 review)

| ID | Status | Notes |
|----|--------|-------|
| S3-F1 | ✅ Fixed | additive `content_ids`; sentinel deleted; `TestS3F1_*` |
| S3-F2 | ✅ Fixed | per-message lease re-check in PutContents |
| S3-F3 | ✅ Fixed | `ComputeContentID` then write; mismatch leaves Stats unchanged |
| S3-F4 | ✅ Fixed | `SplitterFixed8M`; H2 amendment doc; image 4 MiB enforce |
| S3-F5 | ✅ Fixed | symlink entries + Skipped; pkg/backup unit tests |
| S3-F6 | ✅ Fixed | 10 MiB multi-chunk case asserts `len(ids)>1` |
| S3-F7 | ✅ Fixed | confinement walks pkg/agent/cli; CI step |
| S3-F8 | ✅ Fixed | `TestS3F8_SplitterBoundaryIdentityWithWriteObject` |
| S3-F9 | ✅ Fixed | CAS before lease; concurrent + stress tests |
| S3-F10 | ✅ Fixed | CancelConfirmTimeout force-fail |

##### Red-first against unmodified `24300b1` (fix-round findings)

```
=== RUN   TestS3F1_SentinelNamedFileRestoresByteIdentical
    … missing restored path / decode tree … invalid character 'r'
--- FAIL (job success, restore broken)

=== RUN   TestS3F9_ConcurrentDispatchNoLeaseLeak
    … LEASE LEAK: after terminal shared=1 exclusive=0  (or no lease tracked while running)
--- FAIL

=== RUN   TestS3F5_SymlinksPresentInSnapshot
    … symlinks silently missing from tree
--- FAIL

=== RUN   TestS3F2_CancelMidPutContentsRejectsSubsequent
    … PutContents accepted after job terminal
--- FAIL

=== RUN   TestS3F3_MismatchLeavesRepoUnchanged
    … mismatch wrote to vault: user_contents before=N after=N+1
--- FAIL
```

##### After fixes

```
TestS3F1… TestS3F2… TestS3F3… TestS3F5… TestS3F9… TestS3F10…  # PASS
TestM2S3_BackupDedupDemo  # PASS
gofmt / go vet / short+race / gate 256MB / pkg / agent  # green
grep bw-object-from-contents server/ pkg/ agent/  # exit 1 (gone)
```

#### Demo numbers

```
run1 uploaded=10485783 (~10 MiB multi-chunk file + small files)
run2 uploaded=28  (mutate hello.txt + add new.txt only)
dedup ratio run2/run1 ≈ 0.0003%  (≪ 5% criterion)
After forget snap1 + prune min-age 0: snap2 fully restorable
```

#### Verification (stage 3 + fix round)

```
gofmt -l server pkg agent cli restore   # empty
go vet ./...                            # clean
go test ./... -short -race              # all ok
go test ./internal/scheduler/ ./internal/agentgw/ -race  # PASS incl. S3F* + M2S3_*
BW_GATE_BYTES=268435456 TestEngineGate_Kopia  # PASS
pkg + agent tests                             # PASS
grep bw-object-from-contents …                # exit 1
```

---

### Stage 4 — Windows agent service, WiX MSI, golden dataset (2026-07-30)

Real agent binds to stage-2 Channel + stage-3 DataService + `pkg/backup`. Built and
unit-tested on darwin; Windows syscalls/ACLs/service/MSI marked untested until
first `windows-latest` CI / VM run.

| Deliverable | Status | Evidence |
|-------------|--------|----------|
| Agent service core (`--console` + Windows SCM) | ✅ | `agent/internal/service`; cross-compile green |
| State dir + atomic identity (temp-then-rename) | ✅ | `agent/internal/state`; unit tests |
| Enrollment client (BW1 token, zero TOFU FP pin) | ✅ | `agent/internal/enroll` + `TestM2S4_*` |
| Persistent dial-out + keepalive 30s + jittered backoff | ✅ | `agent/internal/control` |
| Completed job_id record (reconnect idempotency) | ✅ | `TestAgent_ReconnectIdempotency` + `TestM2S4_ReconnectIdempotency` |
| JobCancel → terminal JobResult | ✅ | `TestM2S4_AgentCancelConfirmation` (state=cancelled) |
| Job types: INVENTORY / NOOP / FILE_BACKUP | ✅ | branches on `JobStart.type`; reuses `pkg/backup` |
| WiX v5 MSI authoring + CI build script | ✅ | `packaging/msi/`; first CI run validates toolchain |
| Golden generator + comparer (portable degrade) | ✅ | `tools/golden`; skip-with-record on non-Windows |
| Portable golden round-trip demo | ✅ | `TestM2S4_GoldenRoundTrip` PASS |
| CI: windows-latest agent+MSI artifacts; Linux golden | ✅ | `.github/workflows/ci.yml` |
| No kopia in agent (confinement) | ✅ | existing `TestKopiaConfinement` walks agent |

#### Golden dataset coverage

| Fixture | Linux/macOS CI | Windows CI | Notes |
|---------|----------------|------------|-------|
| empty-file | ✅ | ✅ | portable |
| small-text | ✅ | ✅ | portable |
| multi-mb (12 MiB, multi-chunk) | ✅ | ✅ | portable |
| multi-gb | skip (opt-in `LargeFiles`) | skip (opt-in) | reason recorded |
| unicode-names | ✅ | ✅ | portable |
| deep-path | ✅ | ✅ | portable nested |
| empty-dir | ✅ | ✅ | portable |
| symlink-file / symlink-dir | ✅ (darwin/linux) | try / skip-with-record | privilege may fail on Win |
| hardlink | ✅ | try / skip-with-record | portable where FS allows |
| long-path-gt260 | skip-with-record | ✅ attempt | Windows-only |
| acl-system-only | skip-with-record | ✅ attempt (icacls) | Windows-only |
| ads | skip-with-record | ✅ attempt | Windows-only |
| sparse | ✅ portable | ✅ | seek-past-end (S4-F9) |
| unicode-rtl / unicode-nfd | ✅ | ✅ | S4-F10 |
| long-path-gt260 | skip-with-record | ✅ attempt | Windows-only |
| acl-system-only | skip-with-record | ✅ attempt | Windows-only; compare via SDDL (S4-F7) |
| ads | skip-with-record | ✅ attempt | Windows-only |
| junction-symlink-loop | skip-with-record | ✅ attempt | Windows-only |
| deny-share-locked | skip-with-record | ✅ attempt (probe) | full hold-during-backup = CI integration later |

Every skip is explicit with a reason string (S3-F5 lesson). Comparer ACL/ADS
checks also skip-with-record on non-Windows.

#### Fix round S4-F1…F10 (post-37e5fc3 review)

| ID | Status | Notes |
|----|--------|-------|
| S4-F6 | ✅ Fixed | restored WalkDir error propagated; `TestS4F6_*` |
| S4-F1 | ✅ Fixed | `sendMu` on all Channel sends; realistic HB in existing test + race test |
| S4-F2 | ✅ Fixed | MsiHiddenProperties; token → state dir file; delete-not-blank |
| S4-F3 | ✅ Fixed | completed outcomes store success+err; replay real result |
| S4-F4 | ✅ Fixed | fsync temp + dir after rename; corrupt completed.json logged |
| S4-F5 | ✅ Fixed | persist-after-enroll error names recovery; MSI README |
| S4-F7 | ✅ Fixed | SDDL via GetNamedSecurityInfo; icacls detail only on mismatch |
| S4-F8 | ✅ Fixed | `NoWindowsFixtures` honors skip; dead IncludeWindows removed |
| S4-F9 | ✅ Fixed | portable sparse fixture |
| S4-F10 | ✅ Fixed | RTL + NFD unicode fixtures |

##### Red-first against unmodified `37e5fc3`

```
=== RUN   TestS4F6_ExtraBehindUnreadableDirMustNotEqual
    … Compare certified Equal=true diffs=0 skipped=0 err=<nil> while extra data sits behind unreadable dir
--- FAIL: TestS4F6_ExtraBehindUnreadableDirMustNotEqual

=== RUN   TestS4F1_ConcurrentSendUnderRace
WARNING: DATA RACE
  Write … CloseSend / Read … SendMsg (heartbeat vs job)
--- FAIL: TestS4F1_ConcurrentSendUnderRace

=== RUN   TestS4F3_FailedJobReplayMustNotClaimSuccess
    … failed job_id replayed as Success=true (error_message="already completed (idempotent)")
--- FAIL: TestS4F3_FailedJobReplayMustNotClaimSuccess
```

##### After fixes

```
TestS4F6… TestS4F1… TestS4F3…  # PASS under -race
gofmt / go vet / short+race / agent -race / golden -race / M2S4 -race / gate 256MB  # green
```

#### Windows CI vs still-unproven runtime

**Proven by `windows-latest` CI** (run `30581280420` on `78d8d33` — both jobs
success; MSI build step green after WiX fix):

| Item | CI evidence |
|------|-------------|
| Agent unit/integration tests (`agent/`) | `go test ./...` on windows-latest |
| Golden fixtures (Windows set) | ACLs, ADS, sparse, junction loops, long paths, deny-share-locked — `tools/golden` tests pass |
| Agent windows/amd64 build + `--version` | CI build step executes the binary |
| WiX v5 toolchain install | `dotnet tool install wix` + `wix --version` |
| **MSI product build** (`wix build`) | Build MSI + upload artifacts succeeded |

**Still unproven** (no `msiexec` / service runtime on a real box yet — a green MSI
*build* is not an MSI *install*):

1. **Service SCM lifecycle** — Start/Stop/Shutdown via `sc.exe` / services.msc; graceful job cancel on Stop; delayed auto-start after reboot; event-log source `BreakwaterAgent`.
2. **State dir ACL** — `SecureDir` sets SYSTEM+Administrators only, inheritance disabled; standard user cannot read `identity.json` / certs. Window between MSI CreateFolder and first SecureDir (inherited ACLs while folder empty / token write).
3. **Volume inventory** — `GetLogicalDrives` / serial IDs; fixed drives appear; empty CD does not panic; network drives excluded.
4. **MSI install/uninstall** — `msiexec /i … BWTOKEN=BW1:…` enrolls on first start; service starts as LocalSystem; silent install; uninstall stops service, removes files, **never** touches server-side backups.
5. **Token-at-rest path (S4-F2)** — deferred CA writes `pending-enroll.token` under ProgramData; `Property/@Hidden` redacts BWTOKEN from `/l*v` logs (WiX v5; not `MsiHiddenProperties`); agent deletes file after enroll; legacy HKLM migrate+delete on a real install.
6. **SD comparison path (S4-F7)** — GetNamedSecurityInfo + SDDL equality on NTFS after restore; SACL privilege fallback (unit path exercises in golden CI; full restore-from-backup still needs an install+backup cycle).
7. **Directory fsync after rename (S4-F4)** — FlushFileBuffers on a directory handle after identity.json rename survives hard power loss.
8. **SeBackupPrivilege / VSS** — not in stage 4 scope (plain-directory `pkg/backup` only); Phase later.

#### Decisions (stage 4)

1. **Public `agent` package** re-exports control/enroll/state so server integration tests can drive the real agent without importing `internal/` across modules.
2. **Identity write order:** cert+key first, `identity.json` last — LoadIdentity requires all pieces (half-written never loadable). fsync before rename (S4-F4).
3. **Completed job outcomes:** ring of 1024 stores success+error_message; re-JobStart replays the real outcome (S4-F3) — never synthesizes success.
4. **FILE_BACKUP** always uses `pkg/backup` — no forked pipeline, no kopia in agent.
5. **WiX is build-tool only** (MS-RL); output MSI is ours (PLAN pre-approved). Unsigned MVP; SHA256 in CI.
6. **Multi-GB golden** is opt-in (`LargeFiles=true`) — default CI uses multi-MB for multi-chunk coverage without multi-minute fixtures.
7. **Control send path (S4-F1):** single `sendMu` choke point for every Channel client Send/CloseSend.
8. **Enrollment token (S4-F2):** `Property Id="BWTOKEN" Secure="yes" Hidden="yes"` + file under SecureDir; never world-readable HKLM as the at-rest store. (WiX v5: do not author `MsiHiddenProperties` — WIX0070.)

#### Verification (stage 4 + fix round — darwin/arm64)

```
gofmt -l server pkg agent cli restore tools   # empty
cd server && go vet ./... && go test ./... -count=1 -short -race -timeout 10m  # PASS
cd pkg && go test ./... -count=1 -race                                        # PASS
cd agent && go test ./... -count=1 -race                                      # PASS (incl. S4-F1 under -race)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...                        # PASS
cd tools/golden && go test ./... -count=1 -race                               # PASS (S4-F6)
cd server && go test ./internal/agentgw/ -count=1 -race -run 'TestM2S4|Golden' -v  # PASS
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v  # PASS
```

Demo numbers after fix round (`TestM2S4_GoldenRoundTrip`):
```
golden created=12 (incl. sparse, unicode-rtl, unicode-nfd); skipped_gen=6
backup bytes_stored≈80 MiB (sparse logical size); compare matched=13
cancel confirmation: job state=cancelled err="cancelled"
```

---

### Stage 5 — UI shell + REST API (2026-07-30)

PLAN M2 line: **"UI shell against fake API"**. Delivered as a read-only REST
surface over the **real** catalog (not fakes) + React shell with live SSE,
behind a single dev-token middleware (sessions/argon2id/TOTP deferred to M6).

| Deliverable | Status | Evidence |
|-------------|--------|----------|
| REST skeleton on :8443 (`server/internal/web`) | ✅ | `GET /api/v1/{machines,machines/{id},jobs,snapshots,audit,summary,events}` |
| Real catalog data (not fakes) | ✅ | catalog `ListJobs`/`ListSnapshots`/`Summary`; audit `ListEvents`+`VerifyChain` |
| Dev API token middleware (all `/api/v1/*`) | ✅ | `<dataDir>/api-token`; `RequireAPIToken`; unauth 401 test; full token not logged |
| `/healthz` open | ✅ | unchanged |
| SSE `GET /api/v1/events` ↔ scheduler transitions | ✅ | `scheduler.EventHub` + `Engine.Events`; disconnect unsub tests |
| SSE no goroutine leak on disconnect | ✅ | `TestSSE_UnsubscribeOnDisconnect` + churn + hub unit |
| React 18 + TS + Vite + Tailwind + TanStack Router/Query | ✅ | `web/`; dark appliance shell |
| Screens: Dashboard, Machines(+detail), Activity | ✅ | real API data |
| Stubs: Restore, Settings | ✅ | explicit "Not implemented in M2" |
| Audit screen | ✅ | simple table over real audit API (cheap; not stubbed) |
| Placeholders visibly labeled | ✅ | capacity/dedup tiles + stub screens |
| `go:embed` + placeholder for backend-only | ✅ | `server/internal/web/dist/`; `make web` → embed path |
| CI: npm ci + tsc + build on Linux job | ✅ | `.github/workflows/ci.yml` |
| `package-lock.json` committed; no `node_modules` | ✅ | `web/.gitignore` |
| THIRD_PARTY_NOTICES UI deps (MIT/Apache) | ✅ | React/Vite/Tailwind/TanStack/Recharts |
| No mutating :8443 endpoints | ✅ | GET-only API |
| Proto untouched | ✅ | frozen |
| Demo `TestM2S5_UIDemo` | ✅ | enroll → API machines → backup×2 dedup |

#### Auth decision (M2 — document; do not "forget unauthenticated")

Every `/api/v1/*` route is behind **one** middleware (`RequireAPIToken`). Today it
enforces a single **dev-only** token at `<dataDir>/api-token` (generated on first
boot, mode 0600, log prints **preview only** `TokenPreview` — never full token).
This is the single attachment point for M6 sessions + argon2id + TOTP. Query
`?token=` is allowed solely because `EventSource` cannot set headers (dev-only;
cookies replace this in M6). `/healthz` and `/version` stay open.

#### Audit decision (M2-S5)

Read-only REST GETs are **not** audited (noise). Documented in `server/internal/audit`
package comment. Any future mutating endpoint on :8443 **must** be audited
(`job.run_manual`, `job.cancel`, policy/settings/user/enroll-token, etc.).

#### Build integration

```
make web              # npm ci && vite build → server/internal/web/dist
make build-server     # go build only (uses current embed tree)
make build            # web + server
```

Backend-only: a minimal `dist/index.html` placeholder is always present so
`go build ./...` never breaks without Node. Production/CI runs `make web` (or the
CI npm steps) before packaging.

#### Demo numbers (`TestM2S5_UIDemo`)

```
machine appeared in /api/v1/machines (status=active)
run1 bytes_stored=1048586
run2 bytes_stored=21
dedup ratio run2/run1 ≈ 0.0000  (≪ 5%)
snapshots=2; audit chain_ok=true; unauth API → 401
```

#### Verification (stage 5 — single run; superseded by S5-F1/F2 below)

```
gofmt -l server pkg agent cli restore tools   # empty
cd server && go vet ./... && go test ./... -count=1 -short -race -timeout 10m  # PASS
… (web, agentgw M2S5, gate) …
```

#### Fix round S5-F1 / S5-F2 (post-`9ab2831` review `REVIEW-M2-S5.md`)

| ID | Status | Notes |
|----|--------|-------|
| S5-F1 | ✅ Fixed | Root cause + stream-order IDs + deterministic seeded contract |
| S5-F2 | ✅ Fixed | Closeout re-run with `-count=10` (vault) / `-count=3` (pkg) |

##### Root cause (S5-F1) — not a splitter divergence

PLAN's bit-identical have/want contract **holds**. `pkg/contentid.ChunkAndID` and
kopia `object.Writer` (via `WriteObject(DYNAMIC)`) produce the **same chunk
boundaries and the same content IDs** for every seed tested (1–40, including the
review list). Evidence:

1. Whole-slice vs incremental `NextSplitPoint` (32 KiB `io.Copy` buffers): **0**
   size divergences for seeds 1–40.
2. `PutContent` of each pkg chunk reproduces the pkg ID (`putOK=true` for all).
3. Multiset of data IDs from `VerifyObject` equals the pkg ID sequence for all
   seeds — **true content divergence count = 0**.

What failed was **TestS3F8's comparison method**: it compared the pkg ID
*sequence* to `VerifyObject`'s return order. Kopia's `object.VerifyObject` uses
a `map[content.ID]bool` tracker and returns IDs in **map iteration order**
(non-deterministic). Same count, first index differs → looks like a first-boundary
bug; it is only reordering. Flake rate ~15% matches map-order collision chance.

Fix:

- `Vault.ObjectDataContentIDs` — walk the kopia **indirect index in stream order**
  (exported for tests / future re-chunk-sensitive paths).
- `TestS3F8` and new `TestS5F1_SeededSplitterSequenceIdentity` compare against
  stream order, with deterministic seeds (incl. 11, 16, 26, 28, 36, 39) and
  `len(chunk) <= MaxPutContentBytes` (DYNAMIC-4M max segment **equals** 8 MiB;
  PutContent guard is `>`, so exact-max is valid — seed 16 hits 8388608).
- Document on `VerifyObject`: not for sequence identity.

No kopia pin change; splitter code in `pkg/contentid` unchanged (was already correct).

##### Red-first (against `9ab2831` VerifyObject-ordered compare)

Probe (removed after capture) ordered-compared pkg IDs to filtered `VerifyObject`
output for seeds 11,16,26,28,36,39 then 1–40. **Must fail** (map order):

```
=== RUN   TestS5F1_RedFirst_VerifyObjectOrderedCompare
    SEED 28 DIVERGENCE at chunk 0: pkg=1cdccc58a2499287… server=e9d3ebe742592085… (pkgN=2 serverN=2)
    SEED 36 DIVERGENCE at chunk 0: pkg=fc1a8ee58cd70885… server=a016bb8b59942116… (pkgN=3 serverN=3)
    RED-FIRST evidence: 2 seed(s) failed ordered VerifyObject compare (S5-F1)
--- FAIL: TestS5F1_RedFirst_VerifyObjectOrderedCompare

=== RUN   TestS5F1_RedFirst_VerifyObjectOrderedCompare  (-count=3 run 2)
    SEED 7 DIVERGENCE at chunk 0: … (pkgN=3 serverN=3)
--- FAIL

=== RUN   TestS5F1_RedFirst_VerifyObjectOrderedCompare  (-count=3 run 3)
    SEED 26 DIVERGENCE at chunk 0: … (pkgN=4 serverN=4)
--- FAIL
```

(Which seeds fail varies by run — confirms non-determinism, not fixed boundary bugs.)

After fix, `TestS5F1_SeededSplitterSequenceIdentity` + `TestS3F8` use
`ObjectDataContentIDs` and pass every seed every run.

##### Repeat-count evidence (S5-F2)

```
# vault contract — must be green EVERY run
go test ./internal/vault/ -count=10 -run 'TestS3F8|ContentID|RoundTrip|TestS5F1' -v
# → PASS all 10 (incl. 40 seeds × 10 + MaxSegment + VerifyObjectOrder doc)

# full short+race
cd server && go test ./... -count=1 -short -race -timeout 10m   # PASS

# pkg
cd pkg && go test ./... -count=3 -race                         # PASS all 3

# agent, golden, web, engine gate
cd agent && go test ./... -count=1 -race                       # PASS
cd tools/golden && go test ./... -count=1 -race                # PASS
cd web && npm ci && npx tsc --noEmit -p tsconfig.app.json && npm run build  # PASS
cd server && go test ./internal/web/ -count=1 -race -v         # PASS
BW_GATE_BYTES=268435456 go test ./internal/vault/ -run TestEngineGate_Kopia -v  # PASS
```

---

## M2 closeout

**M2 is closed on Linux/darwin evidence after S5-F1/F2.** Stage-5 UI/API was
already good; the blocking issue was a latent stage-3 **test** defect that made
the suite non-reproducible. Windows half of PLAN's demo remains unproven
(see untested-on-Windows list).

### PLAN M2 deliverables (honest status)

| PLAN M2 item | Status | Evidence / caveat |
|--------------|--------|-------------------|
| Windows agent service (SYSTEM) + WiX MSI | ⚠️ **Code complete; Windows runtime unproven** | Service/MSI authored; see [untested-on-Windows list](#untested-on-windows--must-verify-on-first-real-run) — **do not claim MSI install works** until `windows-latest` / VM proves it |
| Persistent dial-out + keepalives | ✅ | `agent/internal/control` + server Channel; demos green on darwin/linux |
| Server-dispatched jobs | ✅ | scheduler Engine + control plane (stage 2–4) |
| Plain-directory backup (chunk → have/want → upload → manifest) | ✅ | `pkg/backup` + DataService; M2S3/M2S4/M2S5 demos |
| UI shell against API | ✅ | stage 5 REST + React shell (real catalog; placeholders labeled) |
| Golden dataset generator + comparer | ✅ | `tools/golden`; portable subset CI-green; Windows fixtures gated |
| **Demo:** MSI install → UI in 10s → backup → 2nd run dedup | ⚠️ **Partial** | See below |

### Demo: what is proven vs unproven

| Step | Proven on Linux/darwin | Unproven pending Windows run |
|------|------------------------|------------------------------|
| MSI install (`msiexec /i … BWTOKEN=…`) | ❌ | Yes — WiX/MSI path untested on real Windows |
| Service start as LocalSystem | ❌ | Yes — SCM lifecycle untested |
| Agent enroll + appears | ✅ via gRPC enroll | UI "in 10s after msiexec" not measured on Windows |
| Appears in `/api/v1/machines` | ✅ `TestM2S5_UIDemo` | — |
| File backup job | ✅ | VSS/SYSTEM/SeBackup not in M2 (plain dir only) |
| Second run reduced upload (dedup) | ✅ run2≪run1 | — |

**M2 is closed on the evidence that exists.** Declaring the full Barracuda-style
"MSI → UI in 10s" demo complete would be Windows evidence that does not exist yet.
The untested-on-Windows list (stage 4) remains the gating checklist for that claim.

### Carried forward (deferred, with reason)

| Item | Reason |
|------|--------|
| Multi-admin sessions, argon2id, TOTP | PLAN Phase 2 / M6; M2 ships dev API token middleware only |
| Mutating REST (job submit/cancel, mint token, policies) | M2 deliberately GET-only; mutating lives on :8443 later with audit |
| Full six screens (Restore/Settings depth) | Shell stubs; restore path is a later Phase 1 milestone |
| Capacity / fleet dedup tiles | Need vault stats aggregation — labeled placeholder |
| Cron schedules / windows / retry | M5; dispatch core exists (stage 2) |
| Vendor kopia; v0.23 on Go 1.25+ | M1 decision #7 |
| Optional keystore `hashing_algorithm` backfill | R3-4 pre-eea1a46 rows |
| Directory sharding vs `MaxMarkObjectBytes` | Scale note for huge trees |
| Windows **runtime** validation (VM required) | `windows-latest` CI is green (run `30581280420`) and closed the *build/test* items only. A build runner cannot prove `msiexec` install, SCM lifecycle, real-box ACLs, or volume inventory — those need a Windows VM with an actual install. **A green Windows CI run is not a proven MSI install.** |
| VSS / SeBackupPrivilege | Not stage 4/5 scope; later Phase 1 |

### Next after M2

- ~~**M4 restore path**~~ — **done** (pulled forward; see [M4](#m4--restore-path-portable-subset) below)
- **M3 (PLAN wk5-6): VSS wired in** — still blocked on Windows VM
- Windows **VM** validation of remaining untested-runtime items
- M6: real web auth replacing the dev token middleware

---

## M4 — restore path (portable subset)

**Closed 2026-07-30** on Linux/darwin. PLAN wk7–8 content delivered ahead of M3
because M3 is Windows-VM-gated and M4 does not need VSS.

### Deliverables

| Item | Status | Evidence |
|------|--------|----------|
| `RestoreService` on :9443 (ListSnapshots/GetSnapshot/GetObject/GetContentRange) | ✅ | `server/internal/agentgw/restore.go`; registered in gateway + `breakwaterd` |
| Authz: own-repo only by default | ✅ | cert → machine; cross-repo denied without job |
| Authz: cross-machine only via restore job + reachable set | ✅ | `TestM4_RedFirst_*` + `TestM4_CrossMachineRestore` |
| Shared lease on SOURCE repo for restore streams/jobs | ✅ | engine `SourceRepoFromParams`; `TestM4_RestoreLeaseBlocksPrune` |
| Per-stream lease revalidation (S3-F2 analog) | ✅ | `revalidateLease` before each chunk send |
| Audit `restore.browse` / `restore.file` (not per-chunk range) | ✅ | audit taxonomy + package docs |
| `pkg/restore` engine (walk, conflict, skips, symlinks) | ✅ | unit tests + E2E |
| Agent `JOB_TYPE_RESTORE` | ✅ | `agent/internal/control`; terminal JobResult always |
| Conflict policy overwrite/rename/skip | ✅ | `pkg/restore` + `TestM4_ConflictRenameAgent` |
| Alternate path + cross-machine restore | ✅ | round-trip + cross tests |
| `bwctl restore` / `bwctl rescan` (REST + API token) | ✅ | `cli/cmd/bwctl`; POST `/api/v1/jobs`, `/api/v1/rescan` |
| Server-loss drill (wipe snapshot index → rescan → restore) | ✅ | `TestM4_ServerLossDrill` |
| Golden round-trip via RestoreService | ✅ | `TestM4_RestoreRoundTrip` matched=13 |
| CI committed UI bundle guard | ✅ | `git diff --exit-code server/internal/web/dist` after npm build |
| `golden.Compare` nil-on-error contract | ✅ | returns partial result + error (no nil panic) |
| Windows ACL/ADS restore via BackupWrite | ⚠️ stub only | **untested-on-Windows** — do not claim |

### Security (red-first)

Two properties written before implementation; captured against a deliberately
permissive stub (any enrolled peer could open any vault object):

```
=== RUN   TestM4_RedFirst_CrossMachineWithoutJobDenied
    B GetSnapshot of A's snap succeeded without restore job (authz hole)
--- FAIL
=== RUN   TestM4_RedFirst_JobDoesNotAuthorizeOutsideReachableSet
    B GetObject of out-of-snapshot object succeeded under job for snap X
--- FAIL
```

After structural authz (own-repo default; restore job + precomputed reachable
object/content set for cross; no foreign vault open without job):

```
TestM4_RedFirst_CrossMachineWithoutJobDenied     PASS
TestM4_RedFirst_JobDoesNotAuthorizeOutsideReachableSet  PASS
  (out-of-reach → PermissionDenied)
```

### Authz design (summary)

- **Default:** Peer machine may list/get/stream only its own repo.
- **Cross-machine:** running/cancelling `JOB_TYPE_RESTORE` on the **target**
  machine with params `(source_snapshot_id, source_machine_id, …)`; Shared lease
  on the **source** repo; GetObject/GetContentRange only for IDs in the
  snapshot's reachable set (tree walk + `ObjectDataContentIDs` for file contents —
  not unordered `VerifyObject`).
- **No mutating RPC on :9443.** Restore is read-only.
- **Audit:** one `restore.browse` / `restore.file` per operation; GetContentRange
  is not audited per byte range.

### Untested-on-Windows (new for M4)

1. **ACL restore via BackupWrite / SeRestorePrivilege** — portable path records
   `acl_restore_untested_on_windows` skip when SD present; platform hook stubbed.
2. **ADS restore via BackupWrite** — same; `ads_restore_untested_on_windows` skip.
3. **Reparse points / junctions** — portable skip with record; Windows restore of
   reparse entries not claimed.
4. All prior stage-4 runtime items remain open (MSI install, SCM, etc.).

### Verification (M4 closeout)

```
gofmt -l server pkg agent cli restore tools   # empty
cd server && go vet ./... && go test ./... -count=1 -short -race -timeout 10m  # PASS
cd pkg && go test ./... -count=1 -race                                        # PASS (incl. restore)
cd agent && go test ./... -count=1 -race                                      # PASS
cd cli && go test ./... -count=1 -race                                        # PASS
cd tools/golden && go test ./... -count=1 -race                               # PASS
cd server && go test ./internal/agentgw/ -count=1 -race -run 'TestM4|Restore' -v  # PASS
BW_GATE_BYTES=268435456 go test ./internal/vault/ -run TestEngineGate_Kopia -v  # PASS
docker build -f packaging/docker/Dockerfile -t breakwater:m4test .              # PASS
```

Demo numbers (`TestM4_RestoreRoundTrip`):
```
golden created=12 portable fixtures; restore matched=13; files=11 bytes≈80MiB
cross-machine A→B bytes identical; prune blocked while restore shared lease held
server-loss: DeleteAllSnapshots → rescan added=1 → restore drill.txt OK
```


### Fix round M4-F1 / M4-F2 (post-`0d8057f` review `REVIEW-M4.md`)

| ID | Status | Notes |
|----|--------|-------|
| M4-F1 | ✅ Fixed | `format.MaxTreeDepth=4096` shared by prune mark + restore reachability; path-aware over-limit errors; depth=300 prune+restore green (old 256 would fail) |
| M4-F2 | ✅ Fixed | `Engine.OnJobTerminal` → `RestoreServer.EvictReachCache`; test asserts eviction after terminal |

**Decision:** tree-walk depth bound raised 256→4096 (runaway guard, not data-shape limit). See REVIEW-M4 disposition.

### bwctl transport note

`bwctl restore` / `bwctl rescan` use **REST on :8443** with the dev API token
(`Authorization: Bearer` / `X-API-Token`). Agent mTLS :9443 remains agent-only.
Documented in `cli/cmd/bwctl/main.go`.

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

## Verification evidence (REVIEW-M1-ROUND3)

Run 2026-07-30 after round-3 fixes (darwin/arm64, Go 1.23.x):

### Red-first against unmodified `eea1a46`

```
=== RUN   TestPrune_FailClosedOnUndecodableFileRoot
    … Prune must fail closed on undecodable file-kind root; got nil (looksLikeTreeJSON leaf heuristic still active)
--- FAIL: TestPrune_FailClosedOnUndecodableFileRoot

=== RUN   TestEnroll_CompensateDespiteCanceledContext
    … keystore row for failed enroll still present … — compensation no-op on canceled ctx
--- FAIL: TestEnroll_CompensateDespiteCanceledContext

=== RUN   TestGetHashingKey_EmptyAlgorithmIsError
    … expected error for empty algorithm, got keyLen=33 algo="" err=nil
--- FAIL: TestGetHashingKey_EmptyAlgorithmIsError
```

### After fixes

```
=== gofmt / go vet ===
(empty / clean)

=== short+race ===
ok  agentgw, catalog, enroll, keystore, mtls, vault

=== R3-1/2/5 prune ===
--- PASS: TestMarkTreeObject_UndecodableRootFailsClosed
--- PASS: TestPutSnapshotRecord_RejectsFlatFileRoot
--- PASS: TestPruneReclaimsForgottenContent
--- PASS: TestPruneSurvivesIndirectTreeReferences
--- PASS: TestPruneSurvivesImageManifestBlocks
--- PASS: TestPruneMinAgeProtectsInFlightBackup

=== R3-3/4/7 enroll+keystore ===
--- PASS: TestEnroll_CompensateDespiteCanceledContext
--- PASS: TestEnroll_CompensateOnVaultFailure (keystore COUNT=0)
--- PASS: TestGetHashingKey_EmptyAlgorithmIsError

=== R3-8/9 ===
--- PASS: TestEnroll_gRPCStatusCodes (InvalidArgument; Internal "enrollment failed")
--- PASS: TestUpgradeFromV1
--- PASS: TestUpgradeFrom755f417

=== reduced gate 256MB ===
rootTree=… (TreeObject); user_contents 56→53; disk shrink; ENGINE GATE PASSED

=== full 10GB gate ===
engine gate: writing 10737418240 bytes (10.00 GiB)
WriteObject ~1m29s; restore ~12s; rootTree=… (mark reads tiny JSON only)
stats before: user_contents=1966; after=1963; disk 9521924786→9518190352
--- PASS: TestEngineGate_Kopia (126.05s)

=== pkg ===
ok  pkg/format
```

**Engine gate: PASSED** at full 10 GiB after R3-1 (TreeObject roots; mark no longer materializes the 10 GiB payload).

---

## Trust Checklist status

See [docs/trust-checklist.md](docs/trust-checklist.md). **Not production-ready.** Item 8 (mTLS) green from M1 (including body/connection mismatch).
