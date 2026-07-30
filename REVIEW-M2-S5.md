# Breakwater M2 Stage 5 Review

**Reviewed:** commit `9ab2831` ("feat(M2-s5): UI shell + REST API; close M2 on Linux/darwin evidence"), diff `d4855f2..9ab2831`, on 2026-07-30.
**Method:** full verification re-run locally across all modules; line-by-line review of `server/internal/web` (auth middleware, SSE, route wiring) and the scheduler event hub. **S5-F1 confirmed empirically** with a seeded reproducer.

## Verdict

**M2 cannot close on this commit — one High finding, and it is not in stage 5's own code.** The stage-5 work itself is good: every `/api/v1/*` route is genuinely gated behind one middleware (sub-mux mount, not per-route opt-in), the dev token is constant-time compared and never logged in full, SSE unsubscribes on disconnect with a non-blocking hub that drops for slow consumers rather than stalling job transitions, placeholders are labeled, and the closeout text correctly refuses to claim the Windows half of PLAN's demo. But running the suite surfaced a **flaky test that is flaky because the underlying contract is actually violated** — agent-side and server-side chunk splitting disagree for roughly 15% of payloads. That is a defect in the M2 data-path foundation (stage 3), exposed now, and M2's headline guarantee ("IDs are bit-identical") is false as written.

## Findings

### S5-F1 · High — agent and server chunk splitting diverge for ~15% of payloads; the contract-lock test is flaky *because the contract is broken*
`pkg/contentid` (`ChunkAndID` / `NewSplitter`) vs `server/internal/vault` `WriteObject(SplitterDynamic)`; test at `server/internal/vault/contentid_roundtrip_test.go` (`TestS3F8_SplitterBoundaryIdentityWithWriteObject`)
The S3-F8 test — added in stage 3 specifically to lock the have/want boundary-identity contract — uses a fresh random 10 MiB payload each run. It **fails about 1 run in 6**, which was reported as "all verification green".

**Deterministic reproducer (probe, since removed):** iterating `math/rand` seeds 1–40 with a 10 MiB payload each, **6 of 40 seeds diverge** (11, 16, 26, 28, 36, 39). Signature is consistent: identical chunk *count*, but chunk **0** differs, e.g.
```
SEED 11 DIVERGENCE at chunk 0: pkg=8aceabe72c5424be… server=66d134cfb8b09e52… (pkgN=3 serverN=3)
SEED 16 DIVERGENCE at chunk 0: pkg=af2ebbd4fa578ec7… server=b63ae9d95723fa31… (pkgN=2 serverN=2)
```
So the two implementations place the first boundary differently while coincidentally landing on the same chunk count — pointing at an off-by-one or a buffer-boundary handling difference between `ChunkAndID`'s whole-slice `NextSplitPoint` loop and kopia's incremental object-writer loop, rather than a different splitter configuration.

**Impact, stated precisely:**
- **Not** backup-breaking today: in the production file path the *agent* does all splitting and the server hashes exactly the bytes it receives, so uploads succeed and agent-to-agent dedup is stable.
- **PLAN's stated invariant is false.** PLAN §Storage engine requires the agent to "import kopia's pure-Go `repo/hashing` + `repo/splitter` so IDs are bit-identical to the server's". They are not, for ~15% of inputs.
- **Anything that re-chunks server-side breaks dedup** — replication (PLAN: decrypt+re-encrypt per chunk), server-side ingest, or restore-side verification would produce a different chunk set for the same bytes.
- **Zero headroom at the size limit:** measured pkg chunk sizes include exactly `8388608` bytes (seed 16) — precisely `MaxPutContentBytes`. The guard is `len(data) > MaxPutContentBytes`, so it passes today by one byte. If the divergence ever pushes a boundary past the forced max split, `PutContents` rejects the chunk and the backup fails.

**Fix:** root-cause which side is wrong (compare `ChunkAndID`'s loop against kopia `repo/object`'s writer loop for the first-buffer case) and make them agree — do not "fix" the test by retrying or loosening it. Then make the contract test **deterministic**: seeded payloads including the known-diverging seeds above, asserting exact ID-sequence equality, plus an explicit assertion that no chunk exceeds `MaxPutContentBytes`.
**Test first:** the seeded reproducer — must FAIL on `9ab2831`.

### S5-F2 · Medium — "all verification green" was reported while the suite fails ~1 run in 6
Process, not code. The stage-5 report claimed every required command green; `go test ./... -short -race` fails intermittently on `internal/vault`. A flaky test in the suite means the reported evidence isn't reproducible, and it is exactly how S5-F1 stayed hidden for two stages.
**Fix:** after S5-F1's root cause, run the full suite with `-count=3` (or the vault package with `-count=10`) as part of the closeout evidence, and record that in PROGRESS.md rather than a single green run.

## Verified clean (my pass)

- **Auth gating is real, not per-route opt-in:** `/api/v1/` is mounted as `mux.Handle("/api/v1/", RequireAPIToken(token)(apiMux))`, so every current and future API route inherits the gate; `/healthz` and `/version` stay open by design. Tests cover unauthenticated rejection, header/query variants, and that the full token never reaches logs.
- **Token handling:** 32 random bytes, hex, `0600`, generated on first boot; `subtle.ConstantTimeCompare`; empty rejected; only an 8-char preview is logged. This is the right single attachment point for M6 sessions.
- **SSE:** unsubscribes via `defer unsub()`, exits on `r.Context().Done()`, heartbeats to survive proxies; the hub's `Publish` is non-blocking with drop-on-slow-consumer, so one stalled UI client cannot back-pressure job transitions. Leak tests exist for connect/disconnect churn.
- **Scope discipline:** read-only endpoints only; no mutating surface added on :8443 (the ransomware boundary is intact); read-only GETs deliberately unaudited and documented.
- **Build hygiene:** `package-lock.json` committed, `node_modules` ignored, `dist` embed has a placeholder so `go build ./...` works without a Node toolchain.
- **Honest closeout:** the report explicitly refuses to claim the MSI-install and service-start halves of PLAN's M2 demo, deferring to the untested-on-Windows list. That is the correct call and matches this project's standard.

## Required fix order

1. **Red-first:** seeded splitter-divergence test (must fail on `9ab2831`).
2. **S5-F1:** root-cause and fix the divergence; make the contract test deterministic and add the `MaxPutContentBytes` headroom assertion.
3. **S5-F2:** re-run the closeout evidence with repeat counts; update PROGRESS.md.
4. Re-state the M2 closeout only after 1–3 are green.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore tools
cd server && go vet ./... && go test ./... -count=1 -short -race -timeout 10m
go test ./internal/vault/ -count=10 -run 'TestS3F8|ContentID|RoundTrip' -v   # must be green every run
cd ../pkg && go test ./... -count=3 -race
cd ../agent && go test ./... -count=1 -race
cd ../tools/golden && go test ./... -count=1 -race
cd ../../web && npm ci && npx tsc --noEmit && npm run build
cd ../server && go test ./internal/web/ -count=1 -race -v
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
```

**Standing rules unchanged.** Proto frozen; kopia confined (`pkg/contentid` carve-out); no new deps.

---

## Disposition (append-only — fix round after `9ab2831`)

| ID | Status | Resolution |
|----|--------|------------|
| S5-F1 | ✅ Fixed | **Root cause revised:** agent/server CDC boundaries are bit-identical; `TestS3F8` compared pkg sequence to kopia `VerifyObject` **map-iteration order** (non-deterministic). Added `Vault.ObjectDataContentIDs` (stream-order via indirect index). `TestS3F8` + `TestS5F1_SeededSplitterSequenceIdentity` (seeds 1–40 incl. 11,16,26,28,36,39) assert sequence equality + `len(chunk) ≤ MaxPutContentBytes`. Splitter/`ChunkAndID` code unchanged. Red-first: VerifyObject-ordered probe FAILED on unmodified `9ab2831` (see PROGRESS). |
| S5-F2 | ✅ Fixed | Closeout re-run: vault contract `-count=10` green every run; pkg `-count=3`; full short+race; web/agent/golden/gate. Recorded in PROGRESS. |

### Red-first capture (VerifyObject ordered compare on `9ab2831`)

```
=== RUN   TestS5F1_RedFirst_VerifyObjectOrderedCompare
    SEED 28 DIVERGENCE at chunk 0: pkg=1cdccc58a2499287… server=e9d3ebe742592085… (pkgN=2 serverN=2)
    SEED 36 DIVERGENCE at chunk 0: pkg=fc1a8ee58cd70885… server=a016bb8b59942116… (pkgN=3 serverN=3)
--- FAIL  (failed seed set varies by run — map order)
```

### After fix

```
TestS5F1_SeededSplitterSequenceIdentity  # 40 seeds PASS
TestS3F8_SplitterBoundaryIdentityWithWriteObject  # PASS (stream order)
TestS5F1_MaxSegmentEqualsMaxPutContentBytes  # PASS (max==8MiB)
TestS5F1_VerifyObjectOrderIsNotStreamOrder  # documents map order
go test ./internal/vault/ -count=10 -run 'TestS3F8|ContentID|RoundTrip|TestS5F1'  # PASS ×10
```
