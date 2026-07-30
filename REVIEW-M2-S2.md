# Breakwater M2 Stage 2 Review

**Reviewed:** commit `70e26a2` ("feat(M2-s2): control plane — Channel, job engine, per-repo serialization"), diff `df2c1e9..70e26a2`, on 2026-07-30.
**Method:** full race-enabled verification re-run locally (all green), line-by-line review of `repolock.go`/`engine.go`/`channel.go`/`registry.go`/`types.go`, independent subreview of the catalog layer + demo-test quality. The catalog jobs state machine (conditional-UPDATE transitions, no terminal resurrection, idempotent duplicates), machines status writers, inventory upsert, and the demo test's assertions were all verified clean — the demo test polls with fail-on-timeout, no vacuous passes.

## Verdict

**Strong core, fix before stage 3.** The RepoLocks condvar design is correct (no lost wakeups, ctx cancellation handled, red-first overlap evidence is real), Channel identity binding and the append-only rejection of server-only job types are enforced and tested, and supersession deliberately preserves running jobs so a network-blip agent can still report results — the right call. The findings cluster around teardown edges (blast radius, lost messages, restarts) and two policy gaps that must be pinned down before stage 3 binds backup jobs — several are individually "medium" but compose into retention-wedging or job-integrity failures.

## Findings

### S2-F1 · High — one malformed inventory item tears down the channel and collaterally fails every running job
`server/internal/catalog/inventory.go:27-30` → `server/internal/scheduler/engine.go:309-314` → `server/internal/agentgw/channel.go:245-248,162-178`
`ReplaceMachineInventory` hard-errors the whole report if any item has an empty `Kind`/`ExternalID`; the error propagates through `HandleInventory` as `codes.Internal`, which the reader loop treats as stream-fatal → channel dies → `OnAgentDisconnect` force-fails **every running job on that machine**. `VolumeInfo.id`/`VMInfo.id` are plain proto3 strings — one agent-side enumeration quirk (a volume without a stable GUID) nukes the machine's whole control plane. Same pattern: ANY application-level error from `HandleProgress`/`HandleResult` (e.g. transient DB error, agent echoing a bogus job id) kills the stream.
**Fix (both layers):** (a) `ReplaceMachineInventory` skips-and-logs malformed items, committing the rest; (b) in `handleAgentMsg`, application-level errors are logged and DON'T kill the stream — only protocol violations (duplicate Hello, mismatched machine) stay stream-fatal.
**Test first:** inventory report with one empty-id volume among good ones → good items persist, channel stays up, a concurrently running job is unaffected. FAILS on `70e26a2`.

### S2-F2 · High (stage-3-critical) — JobStart queued but never flushed is lost on supersession; job wedges in `running` with its lease held
`server/internal/agentgw/registry.go:133-148` (send = buffered enqueue), `channel.go:101-123` (writer), `engine.go:165-205`
`SendJobStart` returning true means *queued to the session buffer*, not delivered. The engine transitions pending→running on queue success. If the agent reconnects (supersede) before the writer flushes, the buffered JobStart is dropped, supersession deliberately skips `OnAgentDisconnect`, and DeliverPending won't re-send running jobs — the job is wedged in `running` forever. In stage 3 a wedged backup job holds a **shared lease forever → prune blocked forever** (retention wedged, the R2-2 failure class by another door). Related: a momentarily-full send queue hard-FAILS the job (`engine.go:186-192`) instead of leaving it pending.
**Fix:** the writer records which messages were actually `stream.Send`-delivered; on session close (supersede OR death), undelivered JobStarts are handed back to the engine → conditional transition running→pending (+ lease release) → next DeliverPending re-dispatches. Queue-full at dispatch → revert to pending (like the offline path), never fail.
**Test first:** dispatch a job into a session whose writer is blocked/never drains, supersede with a new channel, assert the job returns to pending and is delivered on the new channel. FAILS on `70e26a2` (job stays running, never delivered).

### S2-F3 · Medium — RepoLocks has no writer preference: continuous shared traffic starves prune forever
`server/internal/scheduler/repolock.go:112-127`
Shared acquisition succeeds whenever no exclusive holder is present — exclusive waiters are invisible. Overlapping backups/replication/restore streams (stage 3+) keep `shared > 0` indefinitely → prune/verify never acquire → retention never runs. Retention-never-runs is a silent core-guarantee failure (disk fills; PLAN's whole forget/prune model idles).
**Fix:** track `exclusiveWaiters`; new Shared acquisitions block while an exclusive waiter exists (classic writer preference). Exclusive-starves-shared is not a concern at this job mix.
**Test first:** with a continuous chain of overlapping shared holders, an exclusive Acquire must complete within bounded time once current shareds drain. FAILS on `70e26a2` (blocks until the chain fully stops).

### S2-F4 · Medium — results are accepted for never-dispatched `pending` jobs
`server/internal/scheduler/engine.go:283-296,363-371,391-398` (`pending` in the allowed-from lists)
An agent can terminal-ize its own *queued* jobs it never received (`HandleResult` applies to pending). Once job history feeds M5's watchdog/digest, a compromised agent silently "succeeds" its queued backups without uploading a byte — backup-history falsification (chaos drill #7 territory).
**Fix:** results apply only to `running` jobs; a result for a pending job is logged and ignored (the job re-dispatches on reconnect; re-running a backup is dedup-safe). Remove `pending` from `completeJob`/`failJob`-via-result paths (keep pending→failed for server-initiated failure paths like dispatch errors, and pending→cancelled for Cancel).
**Test first:** JobResult for a still-pending job → state stays pending, later delivery still works. FAILS on `70e26a2`.

### S2-F5 · Medium — no startup reconciliation: `running` rows from a dead server process are orphaned forever
`server/internal/scheduler/engine.go` (leases + runningByMachine are in-memory only)
After a server restart, jobs left in `running` have no lease, no tracking, no channel — they sit in `running` forever (phantom UI entries; and once stage 3 acquires leases at dispatch, a crash between transition and restart leaves state inconsistent with lock reality).
**Fix:** an explicit `Engine.RecoverOnStartup(ctx)` called from `main.go`: conditional-transition all `running` → `failed("server restarted")`. Channels can't survive the process; this is always correct.
**Test first:** seed a running row, construct a fresh engine, run recovery, assert failed + re-submittable.

### S2-F6 · Medium — `Cancel` never notifies the agent, and releases the vault lease while the agent may still be working
`server/internal/scheduler/engine.go:317-348`; `registry.go:121-131` (`SendJobCancel` is dead code)
`Engine.Cancel` flips the DB row and releases the lease but sends nothing — the agent keeps running the job (and in stage 3, keeps writing to the vault) while prune can now acquire exclusive: the lease no longer reflects reality. Harmless for inventory/noop; unacceptable semantics to carry into backup jobs.
**Fix now:** wire `SendJobCancel` into `Cancel` (extend the `Dispatcher` interface); for stage 2 keep releasing the lease immediately but DOCUMENT on `Engine.Cancel` that stage 3 must move lease release for vault-touching job types to agent-confirmation (JobResult cancelled) or channel teardown — put that in the stage-3 contract, not just a comment.
**Test:** Cancel of a dispatched running job delivers a JobCancel on the live channel.

### S2-F7 · Medium — `JOB_TYPE_UNSPECIFIED` + hidden `params_json.kind` on the wire
`server/internal/scheduler/types.go:57-76` (`WireJobType`), PROGRESS deviation note
Overloading UNSPECIFIED as "parse params_json to find out what this job is" bakes a wart into the wire exactly one stage before the Windows agent binds to it, and makes UNSPECIFIED permanently ambiguous. Additive enum values are wire-compatible proto3 and match the R2-5 additive-under-freeze precedent.
**Fix:** add `JOB_TYPE_INVENTORY = 6;` and `JOB_TYPE_NOOP = 7; // diagnostics/tests` to the frozen enum (additive, documented in the proto comment), regenerate, map them in `WireJobType`, drop the `params_json.kind` discriminator from the wire contract (keep `kind` in catalog params if useful server-side). Update the channel doc comment for the stage-4 agent.

### S2-F8 · Low — supersede race can leave a live machine marked offline until it reconnects
`server/internal/agentgw/channel.go:83-92`
Old handler: `IsOnline` false → new session registers + `SetMachineOnline` → old handler's late `SetMachineOffline` wins. Status lies until the next reconnect.
**Fix:** heartbeat handler re-asserts online status (idempotent conditional UPDATE — self-heals within ≤30 s); keep the teardown check as-is.

## Notes (not gating)

- `RepoLocks.repos` map never shrinks — irrelevant at ≤15 machines; revisit if repo churn ever matters.
- `Submit`'s pending-bound check is read-then-insert (can overshoot by concurrent submitters) — anti-runaway bound only, fine.
- `TypeUpdate` is agent-dispatchable but UpdateService is later-phase — unreachable today, fine.
- Stage-3 contract items queued from this review: vault handles for jobs must be obtainable ONLY via a lease (structural, not doc-convention); Cancel lease-release-on-confirmation for vault-touching types; dispatch-loop lease acquisition must be non-blocking/timeout (a blocked exclusive must not stall DeliverPending for the machine's other jobs).

## What was verified clean

- RepoLocks condvar core: no lost wakeups (broadcast requires the mutex the waiter holds), ctx cancellation correct, TryAcquire/Held test surface honest; red-first overlap evidence genuine (stub peak=2 captured, serialized peak=1).
- Channel identity binding (Hello machine_id vs cert machine) enforced + tested; server-only job types structurally unreachable from the channel (Submit rejects; nothing maps prune to the wire).
- Supersession semantics for running jobs (survive reconnect; duplicate JobResult no-op) — deliberate and correct; disconnect fails running jobs and releases leases (tested).
- Catalog: conditional-UPDATE state machine (no TOCTOU, no terminal resurrection), machines status writers respect the status enum, inventory replace is transactional; demo-test assertions are real (DB-level asserts, fail-on-timeout polling).
- gRPC keepalive 30 s wired + client contract documented for the stage-4 agent.

## Required fix order

1. **Red-first tests:** S2-F1 (bad item, channel survives), S2-F2 (supersede loses queued JobStart), S2-F3 (exclusive starvation), S2-F4 (result-for-pending ignored).
2. **Blast radius + liveness:** S2-F1, S2-F2 (incl. queue-full → pending).
3. **Lock + lifecycle:** S2-F3, S2-F4, S2-F5, S2-F6.
4. **Wire:** S2-F7 (additive enum values, regenerate, update agent contract docs).
5. **Polish:** S2-F8; PROGRESS.md stage-2 row updated with fix evidence (replace the params_json deviation note with the additive-enum decision).

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore        # nothing
cd server
go vet ./...
go test ./... -count=1 -short -race -timeout 10m
go test ./internal/scheduler/ -count=1 -race -v
go test ./internal/agentgw/ -count=1 -race -run 'TestM1_|TestEnroll_|TestM2S2_' -v
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
cd ../pkg && go test ./... -count=1
```

**Standing rules:** unchanged, except the proto freeze explicitly admits the S2-F7 additive enum values (precedent: R2-5's `hashing_algorithm`). Everything else frozen.
