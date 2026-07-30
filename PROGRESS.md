# Breakwater — Implementation Progress

Single tracking file for milestone status, decisions, and deviations from [PLAN.md](PLAN.md).

## Current milestone

**Phase 1 — M1 (weeks 1–2)** — ✅ **COMPLETE** (2026-07-30; review fixes applied same day)

Addressed [REVIEW-M1.md](REVIEW-M1.md) against commit `f92837f`. Blockers B1/B2 and high-priority H1–H3 resolved; medium items fixed or deferred below.

### M1 deliverables

| Item | Status | Evidence |
|------|--------|----------|
| Git monorepo layout | ✅ | Matches PLAN.md repo layout |
| Legal/docs (MIT, notices, SECURITY, CONTRIBUTING/DCO, README banner) | ✅ | Root files |
| **Engine decision gate** (kopia vault) | ✅ **PASSED** (with reclamation) | See verification evidence below |
| Proto frozen + generated Go | ✅ | `proto/` + `pkg/proto/` |
| SQLite catalog + migrations | ✅ | `server/internal/catalog` |
| Enrollment tokens + mTLS pin | ✅ | Includes body≠connection cert rejection (B2) |
| Fake Linux client enroll; wrong-cert rejected | ✅ | `TestM1_EnrollmentAndWrongCertRejection` |
| Dockerfile | ✅ | `packaging/docker/` |
| CI (Linux + race + pkg + reduced gate; full gate nightly) | ✅ | `.github/workflows/ci.yml` |
| PROGRESS.md | ✅ | This file |

### Engine gate (honest claim)

PLAN criterion: write chunked data → restore → verify → retention + **GC that reclaims space**.

Implementation (`server/internal/vault/kopia.go` `Prune`):

1. **Mark:** live `bw-*` snapshot manifests → `VerifyObject(root)` → live content-ID set  
2. **Sweep:** `IterateContents` → `ContentManager().DeleteContent` for unmarked **unprefixed** IDs  
3. Commit session, then `DropDeletedContents(SafetyNone)` + `maintenance.Run(ModeFull, SafetyNone)`

Assertions in `TestEngineGate_Kopia`: forgotten contents absent (`HasContents` + `OpenObject` fail), `UserContentCount`/`UserSizeBytes` shrink, live object checksum-verifies after prune and re-open.

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
3. **Prune = mark-and-sweep** as PLAN specifies; two write sessions so delete markers commit before index refresh (B1).  
4. **Enrollment identity** bound to **TLS peer cert FP** (`PeerFromContext`); body PEM if present must match (B2).  
5. **Hashing key** from `ContentFormat().GetHmacSecret()` + algorithm name after vault create — never random, never encryption keys (H1).  
6. **Module path** `github.com/ajthom90/breakwater/...`.  
7. **kopia pin v0.19.0** — works on Go 1.23; v0.23.x requires Go ≥1.25.8. Upgrade when toolchain allows (M1 deferred).  
8. **Master key** 32-byte file; per-repo password + hashing key sealed with NaCl secretbox.

## Deviations from PLAN.md

| Deviation | Rationale |
|-----------|-----------|
| Enrollment JSON codec + hand-written service | M1 demo; swap to generated `breakwater.v1` first in M2 (**M13**) |
| kopia not vendored | Pinned in go.mod; vendor before v0.1.0 |
| kopia v0.19.0 not v0.23.x | Go 1.23 toolchain; see decision #7 |
| `breakwater.config`/`.cache` under repo path | M4 deferred to M2 (move under `/data`) |
| Web port plain HTTP | M1 healthz only; HTTPS required before auth UI (M11) |
| Enroll burns token before machine insert | M7 acceptable for M1; reorder/cleanup in M2 |

---

## REVIEW-M1 disposition

| ID | Status | Notes |
|----|--------|-------|
| B1 prune mark-sweep | ✅ Fixed | Two-session prune; gate asserts reclamation |
| B2 TLS peer bind | ✅ Fixed | + `TestEnroll_BodyCertMismatch` |
| H1 hashing key | ✅ Fixed | From vault ContentFormat |
| H2 PutContent >4MiB | ✅ Fixed | Guard + test |
| H3 CI/gofmt/race/pkg | ✅ Fixed | Reduced gate on PR; full gate nightly |
| M3 Close nil-guard | ✅ Fixed | |
| M5 server cert leaf | ✅ Fixed | Not CA |
| M8 ListSnapshot Timestamp | ✅ Fixed | |
| M9 enroll_tokens UNIQUE | ✅ Fixed | |
| M10 docs/actor_type | ✅ Fixed | README + `actor_type` column |
| M12 drop pkg/errors direct dep | ✅ Fixed | |
| M1 kopia version pin | ⏳ Deferred | Documented; upgrade with Go 1.25+ |
| M2 OpenObject vs prune | ⏳ Deferred M2 | Documented on Vault interface |
| M4 config/cache under /data | ⏳ Deferred M2 | |
| M6 enroll error codes | ⏳ Partial | Still InvalidArgument; refine M2 |
| M7 enroll ordering | ⏳ Deferred M2 | |
| M11 web HTTPS | ⏳ Deferred M2 | |
| **M13 ForceServerCodec JSON** | ⏳ **Must fix first in M2** | Proto clients will fail until removed |

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
- Enroll token/order cleanup (M7); config/cache under `/data` (M4)  
- Per-repo job serialization covering open restore streams (M2)  
- Vendor kopia; consider v0.23 when on Go 1.25+  

*Demo: MSI install → appears in UI in 10s → backup → second run shows dedup ratio.*

---

## Verification evidence (REVIEW-M1 block)

Run 2026-07-30 after review fixes (darwin/arm64, Go 1.23.12):

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

=== M1 enroll tests ===
--- PASS: TestM1_EnrollmentAndWrongCertRejection
--- PASS: TestEnroll_BodyCertMismatch

=== reduced gate 256MB ===
stats before prune: user_contents=54 user_size=237922990
stats after prune:  user_contents=52 user_size=237922667
forgotten object unreadable; live checksum OK; re-open OK
--- PASS: TestEngineGate_Kopia (reclamation OK)

=== full 10GB gate ===
engine gate: writing 10737418240 bytes (10.00 GiB)
hashing key: algo=BLAKE2B-256-128 secretLen=32
WriteObject done in ~1m27s; restore verified ~14s; VerifyObject: 1963 pieces
stats before prune: user_contents=1964 user_size=9516952652
stats after prune:  user_contents=1962 user_size=9516952329
forgotten object unreadable; live checksum OK; re-open OK
reclamation OK: user contents 1964→1962 user size shrink
--- PASS: TestEngineGate_Kopia (128.80s)
```

---

## Trust Checklist status

See [docs/trust-checklist.md](docs/trust-checklist.md). **Not production-ready.** Item 8 (mTLS) green from M1 (including body/connection mismatch).
