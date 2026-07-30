# Breakwater M2 Stage 1 Review

**Reviewed:** commit `bc65f8a` ("feat(M2-s1): protocol swap (M13), audit chain, HTTPS web, vault dataDir"), diff `39ded26..bc65f8a`, on 2026-07-30.
**Method:** full verification suite re-run locally (all green: gofmt/vet, short+race, enroll e2e over protobuf, audit tests, vault tests, reduced gate, **full 10 GiB gate 124 s**, codec-gone grep). Line-by-line review of the gateway rewrite, audit package, and vault diff. Key negative claim verified empirically (F3 below).

## Verdict

**Good stage — fix three findings before stage 2.** M13 is genuinely done (codec deleted, generated stubs, enrollment service untouched so every review-hardened behavior carried over verbatim), the M1 demo test rewrite is *stronger* than the original (adds audit-chain and R2-5 storage assertions), the test-only DataService probe is properly gated (`TestDataService` nil in production main), M4/M11 are clean, and the audit chain core (serialized tip-read inside the tx, pinpointing verifier, tamper + 32-way concurrency tests) is sound. The findings are all in the new audit/strictness code, and two of them are format/policy surfaces that get expensive to change once rows/data accumulate — fix them now while both are one commit old.

## Findings

### S1-F1 · High — audit events can be silently dropped via client-controlled context
`server/internal/agentgw/gateway.go:161` and `:186` (`_ = g.Auditor.Append(ctx, …)`), `:277` (`auditEnroll` uses request ctx)
All security-boundary audit appends run on the REQUEST context with the error discarded (interceptors) or merely logged (enroll handler). The R3-3 mechanism applies verbatim: modernc sqlite implements no context-aware driver interfaces, so a done context makes the append fail deterministically before executing. A client that cancels its RPC immediately (or sets a ~0 deadline) probes the port and leaves **no `auth.fail` row and no log line** — an attacker-controllable hole in the product's hash-chained audit trail, which PLAN treats as a headline trust feature.
**Fix:** run every audit append on `context.WithoutCancel(ctx)` (Go 1.21+; preserves values, drops cancellation) — or Background+timeout — and never discard the error: log it with the action/actor at minimum. Apply in both interceptors and `auditEnroll`.
**Test first (red-first):** enroll where the vault stub cancels the request ctx before failing (reuse the `cancelThenFailVault` pattern from R3-3) → assert the rejected `machine.enroll` row still lands and the chain verifies; a direct interceptor-path test with a pre-canceled ctx → `auth.fail` row still lands. Both must FAIL on `bc65f8a`.

### S1-F2 · Medium — canonical audit encoding is ambiguity-prone and about to freeze
`server/internal/audit/audit.go:150-160` (`CanonicalEncoding`)
Fields are concatenated raw with `\n` terminators, so two different tuples collide if any field ever contains a newline (`actor="a\nb", action="c"` vs `actor="a", action="b\nc"` hash identically). Today no writer puts `\n` in these fields (fingerprints are hex; detail is JSON-escaped), so this is not currently exploitable — but the package comment declares the encoding a compatibility surface that must never change without a migration, and rows exist only from this commit's tests. This is the cheapest moment it will ever be to make it injection-proof.
**Fix:** length-prefix each field in the canonical encoding (e.g. `"<decimal-len>:<bytes>"` per field, keeping the field order), update the package doc and `TestCanonicalEncodingSurface`, and add an ambiguity regression test (two tuples that collide under the old scheme must produce different row hashes).

### S1-F3 · Medium — `strictJSONDecode` accepts trailing garbage (weaker than the Unmarshal it replaced)
`server/internal/vault/kopia.go:452-457`
`json.Decoder.Decode` reads exactly one value and ignores everything after it. **Empirically confirmed:** `{"v":1,"entries":[]}TRAILING-GARBAGE-BYTES` passes `strictJSONDecode` while the previous `json.Unmarshal` rejected it (`invalid character 'T' after top-level value`). So the R3-1 "must decode as the kind's format" contract regressed on the trailing-data axis at BOTH the write boundary and the mark phase: a corrupt root that is a valid JSON prefix now validates and marks as an empty/partial tree instead of failing closed.
**Fix:** after `Decode`, require EOF (a second `Decode` into `json.RawMessage` must return `io.EOF`, or check `dec.More()` and reject). One shared helper, both call sites already go through it.
**Test first (red-first):** a file-kind root of `{"v":1,"entries":[]}` + trailing bytes → `PutSnapshotRecord` must reject; must FAIL on `bc65f8a` (currently accepted).

## Notes (not gating)

- TLS-handshake-level rejections (no client cert at all) never reach an RPC and are unaudited — acceptable; there is no identified actor. Documented here for the threat model doc later.
- `audit.Writer` interceptors are pass-through placeholders by design (method-level audit arrives with stage 2's new RPCs); fine.
- Enroll-success-but-audit-failed currently proceeds with a log line. Acceptable for stage 1; revisit as an explicit policy decision when method-level audit lands (stage 2 contract will state it).

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore        # nothing
cd server
go vet ./...
go test ./... -count=1 -short -race -timeout 10m
go test ./internal/audit/ -count=1 -v
go test ./internal/agentgw/ -count=1 -run 'TestM1_|TestEnroll_' -v
go test ./internal/vault/ -count=1 -run 'Root|Strict|Trailing' -v
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
cd ../pkg && go test ./... -count=1
```

(The full 10 GiB gate is not required for this fix round — the mark/sweep algorithm is untouched by F1/F2, and F3's change tightens validation only; the reduced gate covers it.)

**Standing rules unchanged.** Proto stays frozen; no new deps (context.WithoutCancel is stdlib).

---

## Disposition (fix round)

| ID | Status | Notes |
|----|--------|-------|
| S1-F1 | ✅ Fixed | Unary/stream interceptors + `auditEnroll` use `context.WithoutCancel(ctx)`; append errors always logged. Red-first: cancel enroll + white-box canceled interceptor both left zero rows on `bc65f8a`; both green after. |
| S1-F2 | ✅ Fixed | `CanonicalEncoding` is length-prefixed `<decimal-len>:<bytes>` per field (same order). Package doc notes no migration needed (no real deployments had audit rows). Ambiguity regression proves old newline encoding collides and new does not. |
| S1-F3 | ✅ Fixed | `strictJSONDecode` requires second `Decode` → `io.EOF`. Trailing-garbage Put test red on `bc65f8a`, green after. |

Verification re-run locally after fixes: gofmt/vet clean, short+race green, audit/agentgw/vault Root\|Trailing green, reduced gate green, pkg green.

---

## Reviewer verification (fix round, 2026-07-30)

Independently verified `1e58de5`: full verification suite green (gofmt/vet, short+race,
audit, enroll e2e, vault Root/Strict/Trailing, reduced gate, pkg). Diffs match the
prescriptions exactly. Mutation battery — all three killed: WithoutCancel reverted →
both cancel-audit tests fail; EOF requirement dropped → trailing-garbage test fails;
newline encoding restored → surface + ambiguity tests fail. **Stage 1 closed.**
