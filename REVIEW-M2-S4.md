# Breakwater M2 Stage 4 Review

**Reviewed:** commit `37e5fc3` ("feat(M2-s4): Windows agent service, WiX MSI, golden dataset generator"), diff `8bd2bfa..37e5fc3`, on 2026-07-30.
**Method:** full verification re-run locally (gofmt/vet/short+race across server, pkg, agent, tools/golden all green; `GOOS=windows` cross-build OK; M2S4 demo tests pass with explicit skip accounting). Line-by-line review of the agent control loop, state/identity persistence, enrollment client, and MSI authoring. **S4-F1 confirmed empirically** with the race detector.

## Verdict

**Good stage with one confirmed concurrency bug and one secret-handling gap — fix before stage 5.** The honesty discipline this stage most needed is present: the untested-on-Windows list is thorough and accurate, golden-dataset skips are explicit and counted (`matched=10 created=9 skipped_gen=7 skipped_cmp=2`), `pkg/backup`/`pkg/contentid` are reused rather than forked, no kopia in `agent/`, identity persistence is atomic with cert/key-then-manifest ordering and rollback, and enrollment pins the server FP from inside the token before dialing (zero TOFU) and cross-checks the response. The problems are in the control loop's concurrency and in how the enrollment token is carried by the installer.

## Findings

### S4-F1 · High — concurrent `stream.Send` on the control channel: confirmed data race
`agent/internal/control/control.go:299` (heartbeat), `:401` (JobResult), `:439` (InventoryReport), `:484` (JobProgress), `:373` (idempotent re-ack); `a.mu` (`:75`) guards only the `active` map
gRPC explicitly forbids concurrent `SendMsg` on one stream, but job goroutines send progress/results while the main loop's heartbeat ticker sends independently. With two concurrent jobs, or one job's progress overlapping a heartbeat, sends race — corrupting the control stream or crashing the agent.

**Empirically confirmed on `37e5fc3`** (probe, since removed): 12 concurrent inventory jobs with `HeartbeatInterval: 1ms` →
```
WARNING: DATA RACE
Write at 0x00c000177a50 by goroutine 9:
  control.go:284 … control.go:141
  control.go:299 … control.go:254 … control.go:237
--- FAIL: TestProbe_ConcurrentStreamSend
```
Every existing control test sets `HeartbeatInterval: time.Hour`, which is precisely why the suite is green — the tests avoid the condition a real agent lives in permanently (heartbeats every ≤30 s, backups running for minutes).
**Fix:** funnel ALL sends through one writer — a dedicated send goroutine reading a buffered channel (mirrors the server's own session writer in `agentgw/registry.go`), or a `sendMu` held across every `stream.Send`. A single choke point, no exceptions; document it at the Send call sites.
**Test first:** the probe above — concurrent jobs with a fast heartbeat under `-race`. Must FAIL on `37e5fc3`. Then set a realistic heartbeat interval (not `time.Hour`) in at least one existing control test so this can't regress silently.

### S4-F2 · Medium-High — the enrollment token is stored world-readable in HKLM and leaks into MSI logs
`packaging/msi/BreakwaterAgent.wxs:38` (`<Property Id="BWTOKEN" Secure="yes" />`), `:100-106` (writes `HKLM\Software\Breakwater\Agent\PendingEnrollToken`), `agent/internal/service/pending_token_windows.go:26-33` (`clearPendingEnrollToken` blanks instead of deleting)
Three compounding issues: (a) `Secure="yes"` only allows the property to cross the elevation boundary — it does **not** hide it, so `msiexec /l*v` verbose logs record the token in plaintext; (b) the token is persisted to `HKLM\Software\…`, which Authenticated Users can read by default, so any local unprivileged user can read a live enrollment token in the window between install and first successful enroll; (c) after use the value is set to `""` rather than deleted. An enrollment token is a bearer credential that binds a cert to a machine row and provisions a repo — PLAN treats enrollment as *the* security boundary (B2 exists for exactly this class), so leaking it locally is a real privilege gap even with single-use + 24 h TTL.
**Fix:** add `BWTOKEN` to `MsiHiddenProperties` so it is redacted from logs; ACL the registry key (or better, carry the token in a file under the state dir, which `SecureDir` already restricts) to SYSTEM/Administrators; and **delete** the value after successful enrollment rather than blanking it. Note in PROGRESS that the token-at-rest path is Windows-untested and must be verified on the first real run.

### S4-F3 · Medium — the completed-job replay claims success regardless of the real outcome
`agent/internal/control/control.go:370-380` (replay sends `Success: true`), `:407-411` (`MarkCompleted` runs for **failed** results too)
`MarkCompleted` is called after any terminal JobResult — success, failure, or cancellation — and the reconnect replay path then re-acks any known job id with a hardcoded `Success: true, "already completed (idempotent)"`. So a job that genuinely failed can later be reported as successful. Reachability is low today (the server ignores results for non-`running` jobs after S2-F4, and terminal jobs are never re-dispatched), but the lie points in the dangerous direction — a failed backup reported as successful is the S2-F4 falsification class, and M5's watchdog/digest will trust this data.
**Fix:** record the actual outcome alongside the job id (success bool + error message) and replay *that*; or only mark successful jobs completed and let failed ones re-run (backups are dedup-safe). Either way the agent must never synthesize a success it did not achieve.

### S4-F4 · Medium — no fsync before rename: a power loss can leave the agent unenrolled with its token already burned
`agent/internal/state/state.go:221-253` (`writeAtomic`)
The write path is temp-then-rename but never fsyncs the file before rename or the directory after it, so on power loss the rename can land with zero-length or garbage contents. For `identity.json` the consequence is severe: `LoadIdentity` fails → the agent believes it is not enrolled → but the enrollment token was single-use and is already consumed server-side, so the machine cannot re-enroll without operator action. PLAN's chaos-drill matrix explicitly includes crash/kill scenarios, and this is the agent-side equivalent of the repo-consistency guarantee the server already provides.
**Fix:** `f.Sync()` before `Close()`, and fsync the containing directory after `Rename` (no-op-guard it on Windows where semantics differ; document what was actually verified). Same for `completed.json` — a corrupt completed set silently resets idempotency (currently swallowed at `:181-184`, which is the right liveness call but should be logged loudly, not silently).

### S4-F5 · Low — enroll-then-persist failure has no documented recovery, and same-cert retry is blocked
`agent/internal/enroll/enroll.go:125-127`
If the Enroll RPC succeeds but `SaveEnrolled` fails (disk full, ACL problem), the server has burned the token and created the machine row while the agent holds nothing. The server's R2-9/R3-3 compensation covers *server-side* failures only. Retrying with the same cert hits `ErrAlreadyEnrolled`; recovery requires a fresh token **and** a fresh keypair, which is nowhere documented.
**Fix:** document the recovery path (new token; the stale machine row must be removed by an admin) in the MSI README/runbook, and make the agent's error message say exactly that instead of a bare persist error.

### Note (not gating)
The state-directory ACL currently relies solely on the agent's runtime `SecureDir`; the MSI's `util:PermissionEx` is commented out pending `WixToolset.Util.wixext` (`BreakwaterAgent.wxs:85-92`). That leaves a window between install and first service start where the folder has inherited permissions. Acceptable for MVP given the folder is empty until first enroll — but it belongs on the untested-on-Windows list explicitly, and pairs with S4-F2's fix if the token moves into that directory.

## Verified clean (my pass)

- Enrollment: FP pinned from inside the token before dialing (zero TOFU), response FP cross-checked, incomplete responses rejected, `already enrolled` guard, 10-year agent cert, correct keepalive params matching the documented server contract.
- Identity persistence: cert+key written first and `identity.json` last so a partial state is never loadable, with rollback of the cert/key pair if the manifest write fails; `LoadIdentity` validates every required field.
- Completed-job set is durable on disk (survives service restart) and ring-bounded at 1024 — the reconnect-idempotency contract is real, not in-memory-only.
- Cancellation: `JobCancel` cancels the job context and the goroutine always emits a terminal JobResult, satisfying the server's `CancelConfirmTimeout` contract from S3-F10.
- Platform separation is clean (`_windows.go` / `_other.go` build tags, no duplicated logic); `GOOS=windows` cross-build passes; no kopia imports in `agent/`; `pkg/backup` reused, not forked.
- Golden-dataset skips are explicit, counted, and reported per fixture — the S3-F5 lesson correctly applied.
- The untested-on-Windows list is honest and specific (8 items, including the WiX toolchain itself), which is exactly what this stage needed given no Windows box is available.

## Required fix order

1. **Red-first tests:** S4-F1 (concurrent sends under `-race`), S4-F3 (failed job replayed must not claim success).
2. **S4-F1** — single send path (writer goroutine or `sendMu`), plus a realistic heartbeat interval in at least one existing test.
3. **S4-F2** — hidden property, ACL'd/relocated token at rest, delete-not-blank after use.
4. **S4-F3** — record and replay real outcomes.
5. **S4-F4** — fsync before/after rename; log completed-set corruption.
6. **S4-F5** — document the recovery path; improve the error message.
7. Golden-dataset subreview findings (below), then PROGRESS.md updated with red-first captures and any new untested-on-Windows entries.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore tools
cd server && go vet ./... && go test ./... -count=1 -short -race -timeout 10m
cd ../agent && go test ./... -count=1 -race && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...
cd ../pkg && go test ./... -count=1 -race
cd ../tools/golden && go test ./... -count=1 -race
cd ../../server && go test ./internal/agentgw/ -count=1 -race -run 'TestM2S4|Golden' -v
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
```

**Standing rules unchanged.** Proto frozen; kopia confined; no new deps.

---

## Subreview — `tools/golden` (generator + comparer)

Independent adversarial pass over the permanent verification asset.

### S4-F6 · BLOCKER — the comparer reports `Equal()==true` while extra data goes undetected (walk error silently discarded)
`tools/golden/golden.go:425-438`
The "extra files in restored" pass — the half of the comparer that catches a restore producing *more* data than the source (stale leftovers, wrong-repo restore, merged cruft) — discards `filepath.WalkDir`'s error (`_ = filepath.WalkDir(...)`, and the callback returns `err` into a discarded result). The sibling walk over `original` propagates its error; this one does not. Any permission-denied subtree, transient I/O error, or ELOOP stops the scan silently: no diff, no skip record, no error.

**Empirically confirmed on `37e5fc3`** (probe, since removed):
- happy path works — a plain extra file is caught (`Equal=false diffs=1`);
- behind an unreadable subdirectory: `Equal=true diffs=0 skipped=0 err=<nil>` — **the comparer certifies a clean match while extra data sits unscanned.**

This is a verification tool that can lie, in the project whose entire trust model rests on "the comparer proves the restore." Every future restore assertion — file restore, cross-machine restore, BMR — inherits it. Compounding: `golden_test.go` has **no test that plants a file in `restored` absent from `original`**, so the whole extra-data path (bug included) is unexercised.
**Fix:** propagate the walk error — surface it as a `Diff{Field:"walk-error"}` or return it, matching the sibling walk's fail-loud behavior. Never let an incomplete scan render as equality.
**Test first:** both probes above — extra file caught; extra file behind an unreadable dir must NOT report Equal. Second must FAIL on `37e5fc3`.

### S4-F7 · High — ACL comparison diffs `icacls` text instead of the security descriptor
`tools/golden/windows.go:171-191`
PLAN binds this tool to byte + **ACL/SD** equality, but `compareACL` shells out to `icacls` and string-diffs the output. That is weaker in both directions: `icacls`'s coarse rendering (`(F)` covers many distinct access masks; SACL/audit ACEs are not rendered at all) can mask real SD differences → **false pass**; and locale, ACE ordering after restore, or cosmetic inheritance markers can differ textually for functionally identical SDs → **flaky false failures**. This is the sole ACL oracle for every future ACL-restore assertion.
**Fix:** compare the actual descriptor — SDDL via `ConvertSecurityDescriptorToStringSecurityDescriptor`, or raw SD bytes via `GetNamedSecurityInfo` (`golang.org/x/sys/windows` is already imported). Keep `icacls` output only as a human-readable detail on mismatch, never as the equality oracle.

### S4-F8 · Medium — `Options.IncludeWindows` is dead
`tools/golden/golden.go:72-74`, `:206-223`
`wantWin` is computed then discarded (`_ = wantWin`); on Windows the code calls `generateWindows` unconditionally. The documented knob ("attempts Windows-only fixtures") cannot be turned off. No caller sets it today, so blast radius is nil — but it is a live footgun for the first caller who trusts the doc.
**Fix:** honor it (skip-with-record when false) or delete the field and its doc claim.

### S4-F9 · Medium — the sparse-file fixture is Windows-only although the technique is portable
`tools/golden/golden.go:17-24`, `:209-216`
Sparse files are a PLAN-required fixture with no OS qualifier, and `writeLargeFile` in this same file already uses the portable seek-past-end technique that produces holes on ext4/APFS. Scoping `sparse` as Windows-only means the fixture is exercised only on the infrequent Windows job, not on the Linux job that runs every push/PR.
**Fix:** add a portable sparse fixture plus a portable sparseness assertion (`Stat_t.Blocks*512 < Size`), reserving `FSCTL_SET_SPARSE` for the Windows variant.

### S4-F10 · Medium — unicode fixture lacks RTL and combining-character (NFD) names
`tools/golden/golden.go:138-148`
Covers CJK, emoji, Greek, precomposed Latin — but no right-to-left script and no NFD/decomposed sequence (`"café"` vs the precomposed `"café"` used). NTFS preserves exact UTF-16 sequences with no normalization, so an NFD name must round-trip byte-for-byte; a normalization regression currently has nothing to fail against.
**Fix:** add one RTL-named and one NFD/combining-mark-named fixture as distinct paths.

### Verified clean (subreview)
- Fail-closed on **missing** data: every entry in `original` absent from `restored` produces an explicit `missing` diff.
- Skips are structured data (`Result.Skipped`, `CompareResult.SkippedChecks`) with reasons, not log lines; capability-unavailable (skip) is correctly distinguished from unexpected I/O error (hard fail) throughout fixture creation — no repeat of S3-F5 within this package.
- Fixture fidelity: real hardlinks via `os.Link` (not copies), genuine 0-byte file, genuine self-referencing junction loop with no walk-hang risk.
- Fully deterministic (no `math/rand`; content derived from `Options`), so failures reproduce without a seed.
- Build tags are mutually exclusive with no duplicated logic; timestamp comparison uses a sane 1 s default tolerance.
- CI genuinely runs the package on both Linux (portable) and `windows-latest` (full) — not merely cross-compiled.

## Consolidated fix order (supersedes the earlier section)

1. **Red-first tests:** S4-F6 (extra data behind a walk error), S4-F1 (concurrent sends under `-race`), S4-F3 (failed job must not replay as success).
2. **Blocker:** S4-F6 — propagate the walk error; add both extra-data tests.
3. **Agent concurrency:** S4-F1 — single send path + realistic heartbeat in an existing test.
4. **Secrets:** S4-F2 — hidden property, ACL'd/relocated token, delete-not-blank.
5. **Correctness:** S4-F3, S4-F4, S4-F5.
6. **Verification-asset quality:** S4-F7 (SD comparison), S4-F8, S4-F9, S4-F10.
7. PROGRESS.md: red-first captures; new untested-on-Windows entries (SD comparison path, token-at-rest).
