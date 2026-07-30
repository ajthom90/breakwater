# Breakwater — Implementation Progress

Single tracking file for milestone status, decisions, and deviations from [PLAN.md](PLAN.md).

## Current milestone

**Phase 1 — M2 (weeks 3–4)** — 🔄 **IN PROGRESS** (stages 1–3 complete; see [M2 progress](#m2-progress) below)

**Phase 1 — M1 (weeks 1–2)** — ✅ **COMPLETE** (2026-07-30; closed at `161fb18`)

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

#### Decisions (stage 3)

1. **kopia confinement amendment (PLAN-sanctioned):** `pkg/contentid` may import
   `repo/hashing` + `repo/splitter` only (pure-Go). Enforced by
   `TestKopiaConfinement_PkgOnlyContentID`. Vault still owns content/object/manifest/maintenance.
2. **MaxPutContentBytes = 8 MiB** (DYNAMIC-4M max segment), up from 4 MiB FIXED-4M.
   gRPC MaxRecv/SendMsgSize = 16 MiB on :9443. Image fixed blocks remain 4 MiB.
3. **ObjectFromContents** via vault `ConcatenateObjects` for multi-chunk files after
   have/want PutContents (no payload re-upload). Wire: ephemeral PutTreeObject
   sentinel entry name `.bw-object-from-contents` (not stored as a tree) —
   freezes no new proto field under the frozen contract.
4. **Backup library in `pkg/backup`** (not `agent/internal` only) so server demo
   tests share it; agent/internal/fileback re-exports for stage-4 discoverability.
5. **Cancel for vault-writing jobs:** running → `cancelling`, JobCancel sent, lease
   held until JobResult or disconnect (S2-F6 stage-3 contract).

#### Red-first (security boundaries)

```
=== RUN   TestM2S3_CrossMachineIsolation
    … B with A's job → PermissionDenied; B CheckContents of A's content id → absent
--- PASS (after structural machine+lease checks)

=== RUN   TestM2S3_IDMismatchRejected
    … content id mismatch: client=0000… server=<real>
--- PASS

=== RUN   TestM2S3_OversizedBatchAndContentRejected
    … batch 4097 exceeds max 4096; payload 8388609 exceeds max 8388608
--- PASS

=== RUN   TestM2S3_LeaseRequiredForVaultAccess
    … no vault lease held for job (FailedPrecondition)
--- PASS

=== RUN   TestS2F4 … failure path
    ignoring JobResult for non-running job state=pending  (success then failure)
--- PASS
```

#### Demo numbers

```
run1 uploaded=10485783 (~10 MiB multi-chunk file + small files)
run2 uploaded=28  (mutate hello.txt + add new.txt only)
dedup ratio run2/run1 ≈ 0.0003%  (≪ 5% criterion)
After forget snap1 + prune min-age 0: snap2 fully restorable
```

#### Verification (stage 3)

```
gofmt -l server pkg agent cli restore   # empty
go vet ./...                            # clean
go test ./... -short -race              # all ok
go test ./internal/scheduler/ ./internal/agentgw/ -race  # PASS incl. M2S3_*
BW_GATE_BYTES=268435456 TestEngineGate_Kopia  # PASS
full 10 GiB TestEngineGate_Kopia              # PASS (~130s)
pkg + agent tests                             # PASS
```

---

## Next: M2 remaining (weeks 3–4)

Stages 1–3 done. Remaining:

- Windows agent service (SYSTEM) + WiX MSI (stage 4 — bind to Channel + DataService + pkg/backup)
- UI shell against fake API — stage 5
- Golden-dataset generator + comparer
- Cron schedules / windows / retry (M5; dispatch core is stage 2)
- Vendor kopia; consider v0.23 when on Go 1.25+
- Optional: backfill `hashing_algorithm` from vault for pre-eea1a46 keystore rows (R3-4)
- Directory sharding vs `MaxMarkObjectBytes` (16 MiB ≈ max entries per single TreeObject)

*Demo (later): MSI install → appears in UI in 10s → backup → second run shows dedup ratio.*

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
