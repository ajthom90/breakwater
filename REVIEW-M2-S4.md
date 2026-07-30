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
