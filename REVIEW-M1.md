# Breakwater M1 Code Review

**Reviewed:** commit `f92837f` ("Initial M1: monorepo, kopia vault, enroll/mTLS, catalog, CI"), 2026-07-30.
**Scope:** Phase 1 / Milestone M1 deliverables only (see PLAN.md → Phase 1 → M1). Windows agent, VSS, backup pipeline, UI, retention engine, replication are out of scope (M2+).
**How findings were verified:** all server tests re-run locally (`go test ./... -short`, with `-race`, plus the M1 demo test — all green; `go vet` clean); kopia confinement and license hygiene verified by grep; the prune finding was verified **empirically** by running the engine gate at `BW_GATE_BYTES=256MB` and observing repo stats before/after prune; kopia v0.19.0 public API surface for the proposed fixes verified via `go doc`.

## Summary

**Ready with fixes — do not close M1 yet.** The scaffold quality is genuinely good: layout matches PLAN.md, kopia is fully confined to the vault, the agent port is structurally append-only, mTLS/enrollment has real negative-path tests, and everything builds and passes `go vet` and `-race` locally. But the engine gate is **overclaimed**: prune reclaims nothing (contents went *up* 58→59 after forget+prune) because the PLAN-specified mark-and-sweep was never implemented — `DeleteContent` is never called anywhere. Two further security/design gaps (enrollment binds the request-body cert instead of the TLS peer cert; the "hashing key" handed to agents is random bytes unrelated to kopia's actual content-ID keying) are cheap to fix now and expensive after M2 freezes agent behavior.

## Blockers (must fix before calling M1 done)

- [x] **B1 — Prune is a space-reclamation no-op; the engine gate claim "forget + prune ✅ PASSED" is not real.**
  Fixed in round 1 (mark-sweep + DeleteContent) and **completed in round 2 (R2-1/R2-2/R2-15)**: recursive tree/image mark, min-age guard, survival tests, on-disk reclamation. Full 10 GiB gate PASSED with R2-13/R2-15 assertions.
  See [REVIEW-M1-ROUND2.md](REVIEW-M1-ROUND2.md) and PROGRESS.md verification evidence.

- [x] **B2 — Enrollment binds the certificate from the request body, never checking it against the TLS connection.**
  Fixed: identity from `PeerFromContext`; body PEM must match; `TestEnroll_BodyCertMismatch`.

## High priority

- [x] **H1 — The enrollment `HashingKey` is 32 random bytes with no relationship to the repo's content-ID keying.**
  Fixed: vault `ContentFormat` secret + algorithm; round 2 (R2-5) puts `hashing_algorithm` on the wire and in keystore; R2-14 ID round-trip test green.
- [x] **H2 — `PutContent` returns a wrong/ambiguous ID for payloads > 4MiB and silently falls back to object-ID form.**
  Fixed: hard 4MiB guard + VerifyObject error propagated + test.
- [x] **H3 — CI gaps undermine the "CI gates" deliverable.**
  Fixed: gofmt + pkg + race + reduced gate on PR; full gate on schedule/workflow_dispatch (R2-7).

## Medium / nits

- [ ] M1 — kopia pinned at **v0.19.0** but PLAN verified the API against **v0.23.x** (~18 months of upstream fixes behind); PROGRESS.md doesn't justify the pin. Upgrade, or record the reason in PROGRESS.md. **Documented (Go 1.23); upgrade deferred.**
- [ ] M2 — `OpenObject` releases the read lock at return while the reader stays outstanding — once prune actually deletes data, a restore stream can overlap prune's exclusive section. Document the invariant on the Vault interface at minimum; the scheduler's per-repo job serialization must cover it in M2+. **Documented (+ backup-vs-prune R2-2); enforce in M2 scheduler.**
- [x] M3 — `kopiaVault.Close` nils `rep`; any later call panics on nil deref. Guard with an error. **+ R2-10 Manager eviction on close.**
- [ ] M4 — `breakwater.config` and `.cache` under repo path — move under `/data` (M2).
- [x] M5 — Server identity cert leaf (not CA).
- [x] M6 — Enroll error codes (R2-11 typed errors).
- [x] M7 — Enroll compensation on failure (R2-9).
- [x] M8 — `SnapshotMeta.Timestamp` populated (+ R2-12 GetManifest errors).
- [x] M9 — `enroll_tokens.secret_hash` UNIQUE (+ R2-8 migration index).
- [x] M10 — Docs/actor_type.
- [ ] M11 — Web port plain HTTP — HTTPS before auth UI (M2).
- [x] M12 — drop pkg/errors direct dep.
- [ ] M13 — `grpc.ForceServerCodec(jsonCodec{})` — **Must fix first in M2.**

## PLAN.md / standing-rule compliance

| Rule | Status | Notes |
|------|--------|-------|
| No AGPL/GPL deps | ✅ PASS | All direct deps Apache-2.0/BSD/MIT; go.sum spot-checked (rollinghash MIT, GoLLRB BSD, lz4 BSD, modernc BSD-3); THIRD_PARTY_NOTICES.md present and accurate |
| kopia only in vault | ✅ PASS | grep clean — imports only in `server/internal/vault/{kopia.go,kopia_test.go}` |
| append-only :9443 | ✅ PASS | Only Enrollment registered (+ Echo gated behind `TestEcho`, never set in `main.go`); proto defines no destructive RPC on any agent-facing service. Watch item: `RestoreService.ListSnapshots(machine_id)` needs authz scoping to the caller's own repo when implemented |
| Engine gate real | ⚠️ PARTIAL | Write/restore/verify/re-open genuinely proven (re-run during review); **GC/reclamation not real** — see B1. Cannot be called "PASSED" until reclamation is implemented and asserted |
| M1 demo real | ✅ PASS | `TestM1_EnrollmentAndWrongCertRejection` re-run green; covers enroll, post-enroll RPC, wrong cert, bad server pin, plaintext rejection, token reuse |

## Test gaps

- No assertion that forgotten data is reclaimed (the gap that let B1 through).
- No test that a body-cert ≠ connection-cert enrollment is rejected (B2 is invisible to the current suite).
- No concurrent-enroll race test — `ConsumeEnrollToken`'s conditional `UPDATE ... WHERE used_at IS NULL` looks correct but is never exercised in parallel.
- No expired-token test (TTL branch at `enroll_tokens.go:61`).
- No `PutContent` >4MiB test (would have caught H2).
- `mtls` package has no unit tests (identity load/roundtrip, fingerprint stability).
- `pkg/format` tests exist but never run in CI (H3).

## Recommended follow-ups (M2, not M1 blockers)

1. Swap the enrollment wire to the generated `breakwater.v1.EnrollmentService`, remove `ForceServerCodec`, delete the hand-written JSON service + codec — *first* in M2, before the Windows agent binds to the JSON shape.
2. Vendor kopia (`go mod vendor`) as PROGRESS promises, ideally after resolving M1 (the v0.19 vs v0.23 question).
3. Enforce authz scoping on `RestoreService`/`DataService` when implemented: agent role limited to its own repo (PLAN's cross-client isolation); validate `job_id` against dispatched jobs.
4. Wire audit middleware before any additional RPC lands (PLAN: audit on ALL endpoints from day 1 — nothing writes `audit_events` yet; `machine.enroll` should be the first event).
5. Add the `bwctl rescan` skeleton early — the catalog-as-rebuildable-index property is untestable until it exists.

## Verification commands (what "fixed" must look like)

```sh
# From repo root:
gofmt -l server pkg agent cli restore        # must print nothing
cd server
go vet ./...                                  # clean
go test ./... -count=1 -short -race -timeout 10m
go test ./internal/agentgw/ -count=1 -run TestM1_EnrollmentAndWrongCertRejection -v
# Reduced gate MUST now show reclamation (contents/size decrease after prune,
# forgotten object unreadable, live object checksum-verified):
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
# Full gate before re-closing M1:
go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -timeout 45m -v
cd ../pkg && go test ./... -count=1
```

**Bottom line:** fix B1 and B2 (plus H1 while it's a one-line sourcing change), correct the engine-gate test to assert reclamation, update PROGRESS.md to match reality, and M1 is honestly closeable. The foundation is good — the issues are concentrated exactly where PLAN.md predicted the risk would be.
