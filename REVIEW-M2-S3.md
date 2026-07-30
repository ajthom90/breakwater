# Breakwater M2 Stage 3 Review

**Reviewed:** commit `24300b1` ("feat(M2-s3): data plane — DataService, content IDs, backup pipeline"), diff `737c853..24300b1`, on 2026-07-30.
**Method:** full verification suite re-run locally (all green including the full 10 GiB engine gate, 124 s), line-by-line review of `data.go` / vault diff / engine lease surface, plus independent subreviews of `pkg/contentid`+`pkg/backup` and the scheduler changes. **S3-F1 was confirmed empirically** with a probe test against this commit.

## Verdict

**Do not build stage 4 on this commit — one blocker.** The stage delivers a lot that is right: machine binding and cross-machine isolation on every DataService RPC, `CheckContents` scoped to the caller's repo (no cross-repo oracle), lease-checked vault access via `Engine.VaultForJob`, strict JSON validation mirroring the vault's write-boundary discipline, `snapshot.commit` audit, a real dedup result (run2 uploaded 28 bytes vs run1's 10.5 MB), and the M1 prune-survival guarantee now exercised end-to-end through the real data path. But the multi-chunk-file wire hack introduces a **silent backup-corruption bug that is triggerable by an ordinary filename**, and the append-only data path has two structural gaps that must close before a real agent writes production data.

## Findings

### S3-F1 · BLOCKER — `.bw-object-from-contents` sentinel: a real filename silently corrupts the backup (reports success, restore broken)
`server/internal/agentgw/data.go:157-201` (sentinel interception in `PutTreeObject`), `pkg/backup/backup.go` (emits the sentinel tree)
To assemble multi-chunk files without a proto change, the agent sends a *fake* single-entry `TreeObject` named `.bw-object-from-contents` whose `ObjectID` field holds comma-separated content IDs; the server intercepts any such tree and calls `ObjectFromContents` instead of storing it. The interception is **content-based, not context-based** — so a genuine user directory containing exactly one entry named `.bw-object-from-contents` is intercepted too.

**Empirically confirmed on `24300b1`** (probe test, since removed): backing up a directory `userdir/` whose single file is named `.bw-object-from-contents` →
- the backup job reports **`state=success`, no error**;
- the parent tree's `userdir` entry points at the *file's own object* instead of a directory tree object;
- restore fails: `decode tree 7307831a…: invalid character 'r' looking for beginning of value` (`r` = first byte of the file's contents).

A successful backup that cannot be restored is the exact failure class this product exists to prevent, and it is reachable by a user creating one dotfile (or an attacker who can write one filename into a protected tree).

**Fix:** delete the sentinel; carry content IDs in a first-class field. The proto is frozen but **additive changes are pre-approved by precedent** (R2-5 `hashing_algorithm`, S2-F7 job types): add to `PutTreeObjectRequest` a `repeated string content_ids = 3;` (when non-empty, the server materializes an object from those contents and `tree_json` must be empty — mutually exclusive, validated), or add a dedicated `PutObjectFromContents` RPC to DataService if you prefer a cleaner surface. The `backup.Client` interface already has `PutObjectFromContents` — only the wire encoding changes. No in-band magic names anywhere in the data path.
**Test first:** the probe above — directory whose only entry is named `.bw-object-from-contents` → backup succeeds AND every file restores byte-identical. Must FAIL on `24300b1`. Also add a general "adversarial filenames" case to the pipeline tests (leading dots, names colliding with any reserved token, unicode, very long names).

### S3-F2 · High — the vault lease is checked once per PutContents stream, not per message
`server/internal/agentgw/data.go:86-113` (binds `v` on first message; no re-check), `server/internal/scheduler/engine.go:485-493`
`PutContents` resolves the job, validates the lease, opens the vault, and then streams for as long as the agent keeps sending. If the job reaches a terminal state mid-stream — cancel, agent-disconnect handling, `RecoverOnStartup`, dispatch failure — the engine releases the shared lease while this stream keeps writing. Prune can then take the exclusive lock and run concurrently with an active writer: exactly the R2-2 racing-writer scenario that stage 2's lease discipline was built to make structurally impossible. The 24 h min-age guard is the only remaining protection (defense in depth doing load-bearing work).
**Fix:** re-validate the lease per received message (`VaultForJob` is an in-memory map lookup — negligible cost), or hold a stream-scoped lease reference obtained at bind time and released at stream end, so the engine cannot drop the last reference while a writer is live. On lease loss: stop accepting, send `accepted=false`, end the stream cleanly.
**Test first:** start a `PutContents` stream, cancel the job mid-stream, assert subsequent messages are rejected and the stream terminates. Must FAIL on `24300b1`.

### S3-F3 · Medium — PutContents writes to the vault *before* verifying the client-supplied content ID
`server/internal/agentgw/data.go:125-140`
The handler calls `v.PutContent(ctx, data)` first and compares the client ID to the server ID afterwards. PLAN specifies the server "re-computes ID on write and **rejects** mismatches (free integrity check)" — here the mismatch is only *reported*: the payload is already persisted (under its true hash). Transfer corruption or a buggy agent therefore writes garbage into the repo that is unreferenced but occupies space for at least the 24 h min-age window, and the have/want discipline can be bypassed by an agent that simply uploads whatever it likes.
**Fix:** compute the expected ID **before** writing and reject mismatches without touching the vault — expose a `ComputeContentID(data []byte) (ContentID, error)` on the Vault interface (it already owns the repo's hashing params via `HashingKey`) and use it in the handler; only call `PutContent` once the client ID matches (or when the client omitted an ID).
**Test:** a mismatched-ID PutContents leaves the repo content count unchanged (assert via `Stats`), and still returns `accepted=false`.

### S3-F4 · Medium — `MaxPutContentBytes` doubled to 8 MiB with a hardcoded splitter string; image 4 MiB invariant now unenforced
`server/internal/vault/kopia.go:24-28` (constant), `:236` (`Splitter: "FIXED-8M"` literal)
The H2 guard (round 1) established a hard 4 MiB `PutContent` limit matching PLAN's "4MiB aligned blocks"; this commit raises it to 8 MiB to accommodate DYNAMIC-4M-BUZHASH max segments — defensible for the *file* path, but: (a) the splitter is a bare string literal rather than a named constant alongside `SplitterFixed4M`/`SplitterDynamic`; (b) nothing enforces that **image** blocks stay 4 MiB, so PLAN's fixed-block invariant (which RCT extent mapping depends on in Phase 3/4) is now only a comment; (c) the change is not called out in the M1 decisions list where H2's limit was recorded.
**Fix:** add a named `SplitterFixed8M` constant used by `PutContent`; document the 8 MiB CDC rationale in PROGRESS decisions (amending H2 explicitly); and enforce the 4 MiB block invariant where image manifests are validated (`PutImageManifest` should reject `block_size != 4 MiB` for now, or record why not).

## Verified clean (my pass)

- Machine binding and cross-machine isolation on every DataService RPC (`vaultForJobRPCFull`: peer → machine → job ownership → vault-writing type → active state → lease → repo match). `CheckContents` cannot be used as a cross-repo content-ID oracle — it only ever consults the caller's own vault.
- Append-only: no delete/overwrite surface; existing-ID `PutContents` is a dedup no-op; `CheckContents` batch cap enforced; oversized payload rejected per-message without stream teardown (the S2-F1 lesson applied).
- `CommitSnapshot` → vault `PutSnapshotRecord` (strict root validation from stages 1-2 still gates it) → catalog mirror → single `snapshot.commit` audit event on `WithoutCancel` (S1-F1 lesson applied). No per-chunk audit noise.
- `ObjectFromContents` verifies single-content presence before returning, parses every ID, and uses a write session consistent with the other writers.
- Full 10 GiB engine gate still green after the vault changes; `-race` suites green.

## Required fix order

1. **Red-first tests:** S3-F1 (sentinel-named directory), S3-F2 (cancel mid-stream), S3-F3 (mismatch leaves repo unchanged).
2. **S3-F1** — additive proto field/RPC, delete the sentinel path, adversarial-filename pipeline tests.
3. **S3-F2** — per-message lease validation (or stream-scoped lease hold).
4. **S3-F3** — compute-then-write.
5. **S3-F4** — named splitter constant, documented H2 amendment, image block-size enforcement.
6. Subreview findings (below) in their stated order; PROGRESS.md updated with red-first captures and the amended decisions.

## Verification (what "fixed" must look like)

```sh
gofmt -l server pkg agent cli restore        # nothing
cd server
go vet ./...
go test ./... -count=1 -short -race -timeout 10m
go test ./internal/agentgw/ ./internal/scheduler/ -count=1 -race -v 2>&1 | grep -E '^(--- |ok |FAIL)'
BW_GATE_BYTES=268435456 go test ./internal/vault/ -count=1 -run TestEngineGate_Kopia -v
cd ../pkg && go test ./... -count=1 -race
cd ../agent && go test ./... -count=1 -race
# No in-band magic names left in the data path:
grep -rn 'bw-object-from-contents' server/ pkg/ agent/ ; echo "sentinel grep exit=$? (want 1)"
```

**Standing rules:** unchanged, except the proto freeze admits S3-F1's additive field/RPC (precedent R2-5, S2-F7). The `pkg/contentid` kopia carve-out (repo/hashing + repo/splitter only) stands as specified in the stage-3 contract.
