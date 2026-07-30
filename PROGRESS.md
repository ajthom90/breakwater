# Breakwater — Implementation Progress

Single tracking file for milestone status, decisions, and deviations from [PLAN.md](PLAN.md).

## Current milestone

**Phase 1 — M1 (weeks 1–2)** — ✅ **COMPLETE** (2026-07-30)

### M1 deliverables

| Item | Status | Evidence |
|------|--------|----------|
| Git monorepo layout (go.work; pkg/server/agent/restore/cli; proto/web/packaging/docs/.github) | ✅ | Root tree matches PLAN.md “Repo layout” |
| MIT LICENSE, THIRD_PARTY_NOTICES, SECURITY, CONTRIBUTING (DCO), README pre-release banner | ✅ | Root files |
| **Engine decision gate** (kopia vault) | ✅ **PASSED** | `TestEngineGate_Kopia`: 10 GiB write → restore checksum → VerifyObject (1963 contents) → forget + `maintenance.Run` (SafetyNone) → re-open; ~122s on darwin/arm64 |
| `proto/breakwater/v1/breakwater.proto` frozen | ✅ | Generated Go in `pkg/proto/breakwater/v1/` |
| SQLite catalog migrations | ✅ | `server/internal/catalog` + `schema.sql`; default “Standard Server” policy seeded |
| Enrollment tokens + mTLS fingerprint pinning | ✅ | `TestM1_EnrollmentAndWrongCertRejection` |
| Fake Linux client enroll; wrong-cert rejected | ✅ | Same test (M1 demo) |
| Dockerfile | ✅ | `packaging/docker/Dockerfile` (distroless) |
| CI: Linux build/test + Windows cross-compile | ✅ | `.github/workflows/ci.yml` |
| PROGRESS.md | ✅ | This file |

### M1 demo (PLAN.md)

> *Demo: fake client enrolls; wrong-cert client rejected live.*

```
go test ./internal/agentgw/ -run TestM1_EnrollmentAndWrongCertRejection -v
# → machine enrolled; Echo OK; wrong-cert PermissionDenied; bad server pin fails handshake
```

### Engine gate (PLAN.md)

> *from a Go test on Linux, write 10GB of chunked data into a repo, restore it, verify it, and run retention + GC.*

```
go test ./internal/vault/ -run TestEngineGate_Kopia -v -timeout 45m
# Override size: BW_GATE_BYTES=104857600 (for quick local loops)
# Skip in CI unit step: -short
```

**Decision locked:** kopia (`github.com/kopia/kopia` v0.19.0) low-level packages implement `server/internal/vault.Vault`. All kopia imports confined to `server/internal/vault`. Fallback to restic repository layer **not needed**.

---

## Decisions made (M1)

1. **Storage engine = kopia bottom half** — `repo`, `repo/content`, `repo/object`, `repo/manifest`, `repo/maintenance` proven ergonomic for the vault interface. Gate closed.
2. **PutContent public path** — kopia’s `WriteContent` takes `internal/gather.Bytes` (not importable outside the kopia module). Vault implements PutContent via `object.Writer` + `FIXED-4M` and surfaces the backing content ID via `VerifyObject`. Documented in code; no format invention.
3. **Maintenance ownership** — prune sets maintenance params owner to `breakwater@breakwaterd` and runs `ModeFull` with `SafetyNone` (server is sole writer; per-repo RW lock).
4. **Enrollment wire for M1** — hand-written gRPC service (`breakwater.enroll.Enrollment`) with JSON codec for tests/demo; full `breakwater.v1` proto is frozen and code-generated for M2+ agent binding. Both method names allowed without prior pin.
5. **Module path** — `github.com/ajthom90/breakwater/{pkg,server,agent,restore,cli}` (matches GitHub repo).
6. **Master key** — 32-byte file at `/data/keys/master.key`; per-repo password + hashing key sealed with NaCl secretbox.

## Deviations from PLAN.md

| Deviation | Rationale |
|-----------|-----------|
| Enrollment RPC uses JSON codec + hand-written types for M1 live demo instead of only generated protobuf service | Proto is frozen and generated; wiring agentgw to `breakwater.v1.EnrollmentService` is a pure swap in M2 when the Windows agent lands. Demo criterion met either way. |
| kopia not yet `go mod vendor`’d | Pinned in `server/go.mod` (v0.19.0). Vendoring deferred to reduce first commit noise; will vendor before v0.1.0 or on first CI flake from proxy. |
| `web/` empty (no React scaffold) | PLAN M1 does not require UI shell (that is M2). Web port serves `/healthz` + `/version` only. |
| Echo test RPC on agent port | Test-only, registered only when `Gateway.TestEcho` is set; not compiled into production registration path beyond the type existing in the package. |

No one-way-door decisions outside PLAN.md were taken.

---

## Next: M2 (weeks 3–4)

- Windows agent service (SYSTEM) + WiX MSI
- Persistent dial-out + keepalives
- Server-dispatched jobs
- Plain-directory backup (chunk → have/want → append-only upload → manifest)
- UI shell against fake API
- Golden-dataset generator + comparer

*Demo: MSI install → appears in UI in 10s → backup → second run shows dedup ratio.*

---

## Trust Checklist status

See [docs/trust-checklist.md](docs/trust-checklist.md). **Not production-ready.** Item 8 (mTLS) partially green from M1.
