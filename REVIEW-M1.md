# Breakwater M1 Code Review

**Reviewed:** commit `f92837f` ("Initial M1: monorepo, kopia vault, enroll/mTLS, catalog, CI"), 2026-07-30.
**Scope:** Phase 1 / Milestone M1 deliverables only (see PLAN.md → Phase 1 → M1). Windows agent, VSS, backup pipeline, UI, retention engine, replication are out of scope (M2+).
**How findings were verified:** all server tests re-run locally (`go test ./... -short`, with `-race`, plus the M1 demo test — all green; `go vet` clean); kopia confinement and license hygiene verified by grep; the prune finding was verified **empirically** by running the engine gate at `BW_GATE_BYTES=256MB` and observing repo stats before/after prune; kopia v0.19.0 public API surface for the proposed fixes verified via `go doc`.

## Summary

**Ready with fixes — do not close M1 yet.** The scaffold quality is genuinely good: layout matches PLAN.md, kopia is fully confined to the vault, the agent port is structurally append-only, mTLS/enrollment has real negative-path tests, and everything builds and passes `go vet` and `-race` locally. But the engine gate is **overclaimed**: prune reclaims nothing (contents went *up* 58→59 after forget+prune) because the PLAN-specified mark-and-sweep was never implemented — `DeleteContent` is never called anywhere. Two further security/design gaps (enrollment binds the request-body cert instead of the TLS peer cert; the "hashing key" handed to agents is random bytes unrelated to kopia's actual content-ID keying) are cheap to fix now and expensive after M2 freezes agent behavior.

## Blockers (must fix before calling M1 done)

- [ ] **B1 — Prune is a space-reclamation no-op; the engine gate claim "forget + prune ✅ PASSED" is not real.**
  `server/internal/vault/kopia.go:375-409` runs `maintenance.RunExclusive(ModeFull, SafetyNone)` but nothing ever marks contents deleted — there is no mark phase and no `DeleteContent` call in the codebase. Kopia's `repo/maintenance` only drops *already-deleted* contents and orphaned blobs, so forgotten snapshots leak storage forever.
  **Evidence:** `BW_GATE_BYTES=268435456 go test ./internal/vault/ -run TestEngineGate_Kopia -v` → `stats before prune: contents=58 size=237927192` / `stats after prune: contents=59 size=237927510`. The gate test (`kopia_test.go:192-230`) never asserts reclamation, which is how this passed.
  **Fix (proven feasible via public API in pinned v0.19.0):** `DirectRepositoryWriter.ContentManager().DeleteContent(ctx, id)` is public. Implement PLAN's design: mark = walk all live snapshot records → `VerifyObject` each root → live content-ID set; sweep = `IterateContents` → `DeleteContent` every unmarked ID → then `maintenance.Run`. Add gate assertions: the forgotten object's contents are absent after prune, `Stats` shrinks, and the live 10GiB object still checksum-verifies (the existing check at `kopia_test.go:213-224` — keep it).
  **Note:** live data provably survives the current prune (checksum re-verify is real), so this is a missing feature, not data loss.

- [ ] **B2 — Enrollment binds the certificate from the request body, never checking it against the TLS connection.**
  `server/internal/enroll/service.go:67-71` fingerprints `req.ClientCertPEM`; the gateway already extracts the connection peer's FP (`server/internal/agentgw/gateway.go:120-138`) but Enroll never uses it. An enrollee therefore never proves possession of the key being registered — a token holder can bind a third party's public cert to a machine row (identity confusion). PLAN.md says the server binds the *presented* agent cert.
  **Fix:** in the Enroll path, take the fingerprint from `agentgw.PeerFromContext` (or require body cert == connection cert and reject mismatch). Add a test enrolling with body cert ≠ connection cert and assert rejection (the current demo test passes the same identity for both, so it cannot catch this).

## High priority

- [ ] **H1 — The enrollment `HashingKey` is 32 random bytes with no relationship to the repo's content-ID keying.**
  `server/internal/keystore/keystore.go:63-66` generates it independently, but kopia computes content IDs with an HMAC secret derived from repo format — an M2 agent using this key with kopia's hashing package will produce IDs the server never matches, breaking the have/want design (PLAN: "IDs are bit-identical").
  **Fix path (public API):** `WriteManager.ContentFormat()` → `format.Provider` embeds `hashing.Parameters` (algorithm name + HMAC secret) — return *that* to the agent at enrollment. It is a hashing-only secret, consistent with PLAN's "hashing key only, never encryption keys". Fix the sourcing before M2 builds on the wrong key.
- [ ] **H2 — `PutContent` returns a wrong/ambiguous ID for payloads > 4MiB and silently falls back to object-ID form.**
  `server/internal/vault/kopia.go:139-173`: with `FIXED-4M`, >4MiB data yields multiple contents but `ids[0]` is returned as "the" content ID; when `VerifyObject` errors, the code silently returns the object-ID string instead, so the have/want ID space is mixed (`HasContents`/`GetContent` currently paper over this with dual parsing). **Fix:** hard size guard (error on >4MiB) and propagate the `VerifyObject` error. Add a >4MiB test.
- [ ] **H3 — CI gaps undermine the "CI gates" deliverable.**
  `.github/workflows/ci.yml`: never runs `pkg` module tests (Makefile `test-short` does; CI doesn't), never runs `-race` (PLAN verification requires it), no gofmt/lint step — while **five files are currently unformatted** (`gofmt -l`: `server/cmd/breakwaterd/main.go`, `server/internal/enroll/service.go`, `server/internal/enroll/token.go`, `server/internal/keystore/keystore.go`, `pkg/format/snapshot.go`). Also the full 10GB gate runs on every push on `ubuntu-latest` (~14GB disk — borderline, slow). **Fix:** gofmt everything; add pkg tests + `-race` + a gofmt check to CI; run a `BW_GATE_BYTES`-reduced gate on PRs and the full 10GB gate nightly/on-demand.

## Medium / nits

- [ ] M1 — kopia pinned at **v0.19.0** but PLAN verified the API against **v0.23.x** (~18 months of upstream fixes behind); PROGRESS.md doesn't justify the pin. Upgrade, or record the reason in PROGRESS.md.
- [ ] M2 — `OpenObject` (`kopia.go:253-267`) releases the read lock at return while the reader stays outstanding — once prune actually deletes data, a restore stream can overlap prune's exclusive section. Document the invariant on the Vault interface at minimum; the scheduler's per-repo job serialization must cover it in M2+.
- [ ] M3 — `kopiaVault.Close` nils `rep` (`kopia.go:128-137`); any later call panics on nil deref. Guard with an error.
- [ ] M4 — `breakwater.config` and `.cache` are created inside `repoPath` itself (`kopia.go:35-40`) — foreign files inside kopia's blob storage root, and cache travels with any `zfs send`/sneakernet copy of `/repos`. Move both under `/data`.
- [ ] M5 — Server identity cert sets `IsCA: true` + `KeyUsageCertSign` (`server/internal/mtls/certs.go:64-67`) — unnecessary for a pinned leaf; drop both (and `KeyEncipherment`, meaningless for ed25519).
- [ ] M6 — All enroll failures map to `codes.InvalidArgument` and echo internal error text (`gateway.go:213`) — split validation vs internal errors; stop leaking DB errors to clients.
- [ ] M7 — Enroll ordering burns the token before machine insert (`service.go:85-113`); failure in keystore/vault/insert leaves a consumed token and possibly an orphan repo. Acceptable for M1 — reorder or clean up in M2.
- [ ] M8 — `SnapshotMeta.Timestamp` never populated in `ListSnapshotRecords` (`kopia.go:355-360`) — only `ModTime`.
- [ ] M9 — `enroll_tokens.secret_hash` has no UNIQUE/index (`server/internal/catalog/schema.sql:128-136`).
- [ ] M10 — Doc inconsistencies: README status table says "M1 in progress" while PROGRESS.md says complete; PROGRESS decision #5 claims module path `github.com/breakwater-backup/...` but every go.mod uses `github.com/ajthom90/...`; `audit_events` lacks PLAN's `actor_type` column.
- [ ] M11 — Web port is plain HTTP (`server/cmd/breakwaterd/main.go:114-118`) — fine for `/healthz` in M1, but must be HTTPS before any authenticated surface exists (M2 UI shell).
- [ ] M12 — `github.com/pkg/errors` is archived and the repo's own dominant idiom is `fmt.Errorf("%w")` — drop the dep.
- [ ] M13 — `grpc.ForceServerCodec(jsonCodec{})` (`gateway.go:73`) forces JSON for *all* services on the gateway. This is the deviation-#1 protocol debt: the M2 swap to generated `breakwater.v1` protobuf must remove it or proto clients will fail. Track explicitly in PROGRESS.md's M2 list.

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
