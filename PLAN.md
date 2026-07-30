# Breakwater — Open-Source Backup & DR Platform (Design + Implementation Plan)

> **Note for implementing agents:** This document is the single source of truth for building Breakwater. It was produced from a planning session with deep research; the implementer (human or AI, any model) should NOT need the original conversation. Everything required — requirements, validated research findings, architecture decisions, phase gates, and verification drills — is in this file. Where the document says "vendor X's code" or "reference Y", the license has already been verified as MIT-compatible. Do not substitute AGPL/GPL code sources.

## Context

The project owner runs a Barracuda Backup 390 appliance that is out of space; the next appliance tier is prohibitively expensive. They have a TrueNAS box with 66TB usable and want a **fully open-source, self-hosted replacement** for Barracuda Backup — running in Docker on TrueNAS SCALE (bare-metal Linux also supported), published on GitHub under MIT.

**North star: appliance-grade simplicity.** Existing OSS backup tools (UrBackup, Bareos, Amanda) are "half-baked and hard to use." Breakwater must be simple out of the box like Barracuda: install agent → it appears on the server → sensible defaults protect it immediately.

### Decisions made with the project owner
- **Name:** Breakwater (GitHub-clean, coastal-defense metaphor; nods to Barracuda/TrueNAS ocean theme)
- **License:** MIT (all dependencies must be MIT/Apache-2.0/BSD-compatible — this excludes AGPL codebases like UrBackup/Bareos as code sources)
- **Approach:** Custom server + custom Windows agent (Go), embedding a battle-tested content-addressable dedup storage engine rather than inventing a storage format
- **Scale target:** ~5-15 servers, <20TB protected data (single-instance server)
- **Timeline:** MVP protecting production in ~3 months; Hyper-V, bare-metal restore, replication phased after

### Required features
1. Windows agent using VSS — reads EVERY file regardless of ACLs (SYSTEM + SeBackupPrivilege + backup semantics)
2. Hyper-V host backup (guest VM backup)
3. Full restore — to original or different server
4. Bare-metal restore — bootable ISO restores a machine exactly
5. Retention schedules — suggested defaults + customization
6. Audit log of all admin actions
7. Multi-admin user support
8. Replication to another Breakwater instance
9. Docker-on-TrueNAS or bare-metal deployment

### User environment & priorities (confirmed)
- **Workloads:** Windows file/app servers + **Active Directory domain controllers** (no SQL Server, no Exchange). Implication: NTDS-writer-aware system-state backup matters early (safe DC restore, USN-rollback avoidance); SQL/Exchange component backup + log truncation deferred to late phase.
- **All four proposed extras confirmed wanted:** ransomware immutability (append-only + delayed delete), alerts + daily digest email, automatic restore verification (scrub + test restores), cross-snapshot file search.
- **No test lab — production only.** Testing strategy must rely on: GitHub Actions CI (Windows runners for VSS unit/integration; Linux for server), Windows test VMs hosted on the TrueNAS box itself (SCALE runs KVM VMs — can host a throwaway Windows Server VM; nested Hyper-V inside KVM is possible for Hyper-V module testing), and careful staged pilots on production machines (backup-only, restore to VM, never overwrite production).

### Additional features (all confirmed wanted; prioritization in roadmap)
- Ransomware protection: append-only/immutable retention, delayed deletion
- Email/webhook alerts + daily backup health digest (Barracuda-style report)
- File-level restore browser (browse any snapshot, restore single files) + client-side download
- Automatic backup verification (scrub/checksum, periodic test restores)
- Encryption in transit (mTLS) and at rest
- Bandwidth throttling + backup windows
- Application-aware backup via VSS writers (SQL Server, Exchange log truncation) [late phase]
- Prometheus metrics endpoint + healthcheck integration
- Agent auto-update from server
- Storage trend/capacity forecasting reports, dedup ratio stats
- REST API + CLI for automation
- Off-instance replication also to S3-compatible targets (B2/Wasabi/MinIO) [stretch]
- Linux agent [post-MVP]

## Research inputs

### Windows/Hyper-V internals research (validated)

**VSS agent — Go is proven viable:**
- restic's `internal/fs/vss_windows.go` (BSD-2-Clause) implements a full pure-Go VSS requester (COM vtable calls, no cgo): `CreateVssBackupComponents → InitializeForBackup → SetContext(VSS_CTX_BACKUP) → GatherWriterMetadata → StartSnapshotSet → AddToSnapshotSet → PrepareForBackup → DoSnapshotSet → read from \\?\GLOBALROOT\Device\HarddiskVolumeShadowCopyN → BackupComplete`. Vendor/extend this code.
- Do NOT use `mxk/go-vss` (what kopia uses) — it goes through WMI `Win32_ShadowCopy` and produces `ClientAccessible` snapshots that don't trigger SQL/Exchange VSS writers.
- Read-any-file: run as SYSTEM, enable `SeBackupPrivilege` via `AdjustTokenPrivileges`, open with `FILE_FLAG_BACKUP_SEMANTICS`; `BackupRead` streams data+ACLs+ADS (needs small manual syscall bindings; x/sys has the rest).
- App-consistent (component-based) backup with writer metadata/`AddComponent`/`SetBackupSucceeded` = the hard differentiator (log truncation for SQL/Exchange). No OSS Go precedent; AlphaVSS (MIT, C#) is a fallback side-car. Phase it after crash-consistent+writer-involved snapshots work.
- Platform limits: writer freeze ≤60s, provider commit ≤10s; needs admin/elevation.

**Hyper-V — on-host only (off-host needs SAN vendor VSS hardware providers; dead end):**
- WMI v2 (`root\virtualization\v2`): `Msvm_VirtualSystemSnapshotService.CreateSnapshot(SnapshotType=32768 recovery)` → `ConvertToReferencePoint` → next run: `QueryChangesVirtualDisk` (virtdisk.h, RCT) for changed extents. Requires WS2016+, VM config v8+.
- Full backups via WMI Export; incrementals via Win32 RCT (local-only is fine — agent runs on host).
- Must capture: .VMCX + .VMGS + .VMRS + all VHDX.
- App-consistent guests need Integration Services "Backup (volume snapshot)" + SCSI controller; falls back to crash-consistent.
- Reference impl: `cloudbase/rct-service` (Apache-2.0, Rust). Defer cluster/CSV support.

**Bare-metal restore:**
- WinPE ISOs CANNOT be redistributed (MS licensing). Ship a **WinPE builder** (runs on user's Windows box with ADK) AND a redistributable **Linux-based restore ISO** (wimlib+partclone/ntfs-3g are GPL but run as separate processes on the ISO — no MIT contamination of our code).
- Restore flow: recreate GPT/ESP → write volumes → offline `Dism /Add-Driver` for dissimilar hardware → `bcdboot /f UEFI` → `reagentc`. `bootrec` is MBR-only.
- Restore environment must DIAL OUT to the server like the agent does (UrBackup's server→client restore reachability requirement is a known trap).
- Volume CBT for physical machines: v1 = 512KB-4MB block hash-compare vs previous image manifest (UrBackup model; no kernel driver ever — driver signing/HVCI is the hardest thing in the whole space). NTFS USN journal assists file-level incrementals.

**Storage engine:**
- Agent-side chunking + dumb append-only server is the winning architecture (restic+rest-server, kopia+repo-server, PBS all prove it): "have/want" chunk-ID handshake, upload misses only.
- PBS's DIDX (CDC, file archives) + FIDX (fixed-block, disk images) split is the right primitive model: **CDC for files, fixed blocks for volume/VHDX images** (makes RCT extent mapping trivial).
- CRITICAL security note: shared multi-client repos leak cross-client data (content-ID confirmation attacks; kopia server only ACLs manifests). **One repo per client machine**; cross-client dedup sacrificed deliberately.
- Retention: GFS via keep-hourly/daily/weekly/monthly/yearly counts; forget (refs) vs prune (space) split; scrub via periodic read-data subsets.

**Transport/architecture synthesis:**
- Agent ALWAYS dials out (client-initiated; Bareos `Connection From Client To Director` precedent); one inbound TLS port on server; control+data multiplexed (gRPC/HTTP2); long-lived connection with keepalives so server can schedule jobs.
- mTLS with cert-fingerprint pinning (kopia model), one-time enrollment tokens provisioning per-client keypairs. No PKI to run.
- Append-only client rights; prune/GC/delete run server-side ONLY → ransomware'd client cannot destroy backups.
- Agent auto-update: server hosts signed installers, staged rollout, never mid-job (UrBackup model). Needs code-signing cert (budget item).

**Risk ranking:** (1) component-based app-consistent VSS, (2) never do kernel CBT driver, (3) dissimilar-hardware BMR long tail, (4) Msvm_* WMI verbosity, (5) WinPE licensing — designed around via builder+Linux ISO.

### OSS landscape survey (validated)

**Nobody has built Breakwater's shape.** Explicit gaps across ALL OSS projects: unified control plane (agent fleet mgmt + multi-admin RBAC + audit in one product), Hyper-V guest backup (only Bareos 25's brand-new AGPL plugin exists; UrBackup's is paid/closed), hardware-independent Windows BMR (no OSS project does driver injection; Veeam Recovery Media is the benchmark), immutable/ransomware-hardened repos, instant recovery, cross-client file search.

**Candidates evaluated and why they're not the foundation:**
- **Bareos 25** (AGPLv3): most complete natively (Hyper-V RCT plugin + "Barri" Windows BMR imager are AGPL source in-tree) — but AGPL blocks MIT, config-file-driven UX is hostile to appliance simplicity, trademarked. ARCHITECTURE REFERENCE only (Barri format doc + audit/ACL model are public).
- **UrBackup** (AGPLv3): closest UX analogue (auto-discovery, image+file, restore ISO, TrueNAS app) — but Hyper-V client and CBT driver are PAID+CLOSED, bus factor ~1, no audit log, weak RBAC, no replication. Reference for: appliance UX, internet-mode transport, block-hash incremental images.
- **PBS** (AGPLv3): best engine primitives (GFS prune, RBAC, sync jobs, verify jobs, FIDX/DIDX formats — all documented) but zero Windows/Hyper-V agent story, Docker-hostile. Reference for: control-plane feature design, DIDX/FIDX chunk model.
- **Permissive cluster** (restic BSD-2, kopia Apache-2.0, Duplicati MIT, Plakar ISC): engines only — none supply the appliance layer. **restic's VSS implementation is the single most reusable permissive asset** (copyable into an MIT product legally).
- Excluded: Bacula (Hyper-V+BMR are Enterprise-only upsell), Borg (no native Windows), BackupPC (no agent), Amanda (dormant), rustic (no VSS, beta).

**License conclusion:** MIT product ⇒ permissive-only dependencies (restic BSD-2 code vendoring OK, kopia Apache-2.0 libs OK, cloudbase/rct-service Apache-2.0 OK). NO code from AGPL projects (Bareos/UrBackup/PBS) — concepts and documented formats/protocols only. GPL tools (wimlib, partclone) may ship as separate processes on the Linux restore ISO without affecting MIT code. tizbac's Go PBS client is GPL-3.0 — reference only, no code reuse.

**Open verification items for implementation phase:** Windows ADK current EULA (WinPE builder approach); wimlib library license (GPL vs LGPL) for ISO composition.

## Architecture

### Storage engine — hybrid: kopia's low-level repo layers + Breakwater-native snapshot formats

**Verified (pkg.go.dev, kopia v0.23.x, Apache-2.0):** `repo` (Initialize/Open/WriteSession), `repo/content` (full CAS: WriteContent/GetContent/DeleteContent/IterateContents — pack files, indexes, per-content zstd + AES-256-GCM, caching), `repo/object` (NewWriter with named splitters incl. `FIXED-4M` and `DYNAMIC-4M-BUZHASH`, VerifyObject returns component content IDs), `repo/manifest` (arbitrary labeled JSON records in-repo), `repo/maintenance` (Run/DeleteUnreferencedPacks/index compaction — usable WITHOUT kopia snapshots) are all public importable packages. We use kopia's bottom half (battle-tested since ~2016) and NOT its snapshot/policy/uploader/VSS top half.

- **One kopia-format repo per machine** at `/repos/<machine-ulid>/`, server-generated per-repo password.
- **File snapshots (DIDX-analog):** per-directory tree objects (JSON; entries = name, type, size, timestamps, NTFS attrs, raw security descriptor, ADS list [each ADS its own object], reparse data, child object ID). File contents via `DYNAMIC-4M-BUZHASH` splitter. Unchanged-subtree short-circuit: dir entries unchanged vs agent's local metadata cache → reuse previous tree object ID without re-reading. Snapshot record via `manifest.Put` labels `{type: bw-file-snapshot, machine, ts}` + mirrored to catalog.
- **Image snapshots (FIDX-analog) — physical volumes + VHDX:** fixed **4MiB aligned blocks**, each a content via WriteContent; all-zero blocks → one well-known zero content; image manifest = ordered array of `{contentID, xxhash64}` (1TB ≈ 262k entries ≈ 10MB object). **Why fixed-block: RCT changed-extent lists and hash-compare diffs map trivially onto block indexes** — incremental = copy previous manifest, overwrite entries intersecting changed extents, upload only changed blocks. Neither restic nor kopia model this natively; here it's ~200 lines over a proven CAS.
- **Have/want protocol with server-side keys:** content ID = keyed hash (kopia `BLAKE2B-256-128`). Agent gets its repo's **hashing key only** (never encryption keys) and imports kopia's pure-Go `repo/hashing` + `repo/splitter` so IDs are bit-identical to the server's. Agent batches `CheckContents([]ID) → bitmap`, uploads misses; server re-computes ID on write and **rejects mismatches** (free integrity check). Compression/encryption happen server-side in the content layer; wire compression via gRPC zstd. Server CPU is ample at ≤15 machines.
- **GC/prune (server-only):** mark = walk live snapshot records → trees/manifests → VerifyObject → live content-ID set; sweep = DeleteContent + maintenance.Run (with kopia's safety window). Server is the ONLY writer to any repo → per-repo RW lock (backup/replication shared, prune/verify exclusive) satisfies kopia's delete-race caveat.
- **Scrub:** rotating deterministic 1/Nth slice per window via IterateContents + GetContent (auth verified on read) + VerifyObject over live manifests; verify state in catalog + UI.
- **Rejected alternatives:** full kopia embed (image backups would masquerade as giant files — no block index for RCT; fights its policy/scheduler layers); fully custom PBS-style store (violates "proven engine" decision — we'd own pack crash-consistency and format hardening for years).
- **One-way doors (flagged):** on-disk repo format (kopia) and per-repo hashing algorithm. Escape hatch: stock `kopia` CLI + recovery-kit password can read raw contents of a Breakwater repo; `bwctl` ships an offline extractor. Everything else is behind the vault interface and reversible. Mitigation for kopia v0.x API churn: pin exact version + `go mod vendor` + confine ALL kopia imports to `server/internal/vault` exposing a narrow interface (PutContent/HasContents/WriteObject/OpenObject/PutSnapshotRecord/Prune/Verify).

### Components

**`breakwaterd` (server — single Go binary, single process, single container):**
| Subsystem | Role |
|---|---|
| Agent gRPC gateway | THE one inbound TLS port (:9443) for agents + replication peers; mTLS, fingerprint pinning, per-role method ACLs — the append-only enforcement point |
| Web/REST server | :8443 — embedded UI (go:embed), REST for bwctl/automation, /metrics, /healthz; sessions + API tokens |
| Scheduler | cron + backup windows (robfig/cron MIT for parsing), dispatches jobs down agent control streams, per-repo job serialization, retry/backoff, missed-window catch-up |
| Vault manager | owns per-machine kopia repos; ONLY code importing kopia; per-repo RW locks; prune/verify/stats |
| Catalog | SQLite WAL at /data/catalog.db via `modernc.org/sqlite` (BSD-3, pure Go → CGO_ENABLED=0). System of record for policy/users/audit; **rebuildable index** for snapshots (`bwctl rescan` rebuilds from in-repo manifests). SQLite over Postgres firmly: thousands of rows/year, one writer goroutine, no sidecar containers. Schema behind thin store interface (portable later) |
| Keystore | master key (/data/keys/master.key, optional passphrase seal) encrypting per-repo passwords + hashing keys; Recovery Kit generation |
| Auditor | append-only hash-chained audit rows (SHA-256 chain: prev_hash/row_hash) |
| Notifier | SMTP (wneessen/go-mail MIT) + webhooks; failure alerts, missed-backup watchdog, daily digest |
| Update host | serves signed agent MSIs + staged rollout state over :9443 |

**`breakwater-agent` (Windows service, SYSTEM, single Go binary, MSI via WiX):**
- Service core: `x/sys/windows/svc`; one outbound gRPC conn, keepalives + jittered backoff; state in `C:\ProgramData\Breakwater\` (metadata cache, previous image-manifest block hashes, logs).
- VSS module: vendored restic `vss_windows.go` (BSD-2, header retained) extended for multi-volume snapshot sets + writer-status surfacing. Component-based mode phased later.
- File pipeline: SeBackupPrivilege + FILE_FLAG_BACKUP_SEMANTICS; `BackupRead` parsed into data/SD/ADS via WIN32_STREAM_ID framing (structured entries, not opaque blobs — restore browser shows individual files/streams); split → keyed hash → CheckContents/PutContents → tree objects → snapshot record.
- Image pipeline: raw volume read from shadow device → 4MiB blocks → xxhash64 vs cached previous manifest (UrBackup-model CBT, no kernel driver EVER) → upload changed; NTFS allocation bitmap skips unallocated clusters on fulls.
- Hyper-V module: WMI v2 via go-ole/microsoft-wmi (MIT): CreateSnapshot(32768) → ConvertToReferencePoint; incrementals via QueryChangesVirtualDisk (virtdisk.dll syscalls; local-only fine — agent on host); VMCX/VMGS/VMRS as file objects + VHDX through image pipeline.
- Restore module: BackupWrite (SeRestorePrivilege), offline volume rewrite, VHDX/VM re-import.
- Self-update: MSI over existing channel; verifies Authenticode AND a release-channel ed25519 signature baked into agent at build (server compromise can't push malicious agents); never mid-job; staged rollout.

**Restore environments:** (1) Linux restore ISO — Alpine, CI-built, redistributable; `breakwater-restore` Go TUI **dials out** with one-time restore token; GPL tools (partclone/ntfs-3g/wimlib/sgdisk) as separate processes; scope = exact-hardware BMR. (2) WinPE builder — PowerShell + Go helper on user's ADK-equipped Windows box; adds dissimilar-hardware restore (offline Dism /Add-Driver, bcdboot, reagentc). Both are server-driven wizards; boot media stays dumb. Document the capability split loudly — users must build WinPE media BEFORE needing dissimilar-hardware restore.

**`bwctl` CLI:** REST + API token; **offline mode** opens a repo directory read-only with the recovery kit and extracts files/volumes with no server — the total-loss DR story in one static binary.

### Transport & protocol (gRPC/HTTP2, proto3 at `proto/breakwater/v1/`)
- Services: `EnrollmentService.Enroll` (only tokenless-cert RPC); `ControlService.Channel` (long-lived bidi: server→agent JobStart/JobCancel/UpdateOffer; agent→server Hello/Heartbeat/JobProgress/JobResult/InventoryReport; 30s keepalive; idempotent reconnect/resume); `DataService.CheckContents` (batches of 4096) / `PutContents` (client-streaming, windowed acks) / `PutTreeObject` / `PutImageManifest` / `CommitSnapshot`; `RestoreService.GetSnapshot/GetObject/GetContentRange`; `ReplicationService` (agent data protocol verbatim + ListSnapshotsSince); `UpdateService`.
- **Enrollment (no PKI):** token = `BW1:<host:port>:<serverCertFP-sha256>:<secret>` — server fingerprint travels INSIDE the token (zero TOFU). Agent generates ed25519 keypair + self-signed cert, verifies server FP, enrolls; server binds agent cert FP → machine row, creates repo, returns machine ID + hashing key. Mutual fingerprint pinning thereafter; rotation = re-enroll with operator approval. Tokens single-use, 24h TTL.
- **Append-only is structural:** the :9443 interceptor exposes NO RPC that deletes, prunes, or mutates retention — those exist only on :8443 (human-authenticated). This is the ransomware boundary.

### Catalog data model (SQLite, ULID keys, one writer goroutine)
`machines` (cert_fp, os_info, agent_version, status, repo_id) · `machine_inventory` (volumes, Hyper-V VMs + rct_capable) · `policies` (schedule_cron, window, throttle, retention counts, app_aware, is_default) · `jobs` (type: file|image|hyperv|restore|prune|verify|replicate|update; state; bytes stats; log ref) · `snapshots` (kind, source, manifest_ref, root_object_id, gfs_tags, verify_state, deleted_at soft-delete, job_id) · `users` (argon2id, totp_secret enc, role) · `audit_events` (hash-chained) · `enroll_tokens` · `api_tokens` · `settings` · `replication_peers`/`replication_state` (cursor, lag) · `keystore` (repo_password_enc, hashing_key_enc). Chunk→pack indexes live in the repo (kopia index blobs), NEVER the catalog.

### Replication — logical, content-level, source-initiated push, agent protocol reused
Destination = just another Breakwater server on :9443; source enrolls with one-time token, gets `replication-peer` role (append-only RPC set scoped to mirrored repos + CheckContents). Only ONE side needs a reachable port; design is symmetric (flip who pushes if NAT dictates). Per-repo sync job: snapshot records since cursor → walk manifests → CheckContents → push missing → commit record → advance cursor; resumable at content granularity. Destination applies its OWN retention + prune + verify (PBS sync-vs-prune separation) and has DIFFERENT keys. Users/policies/audit do NOT replicate — replica is a data warehouse, not hot standby; failover = restore-from-replica via recovery kit. Why not blob-copy: raw sync needs delete rights on destination (breaks append-only); logical keeps it airtight. Cost: decrypt+re-encrypt per chunk (CPU is ample); initial multi-TB seed slower — documented sneakernet path (zfs send repos + import).

### Security model
- **Server-side key custody:** agents never hold decryption keys (stolen machine leaks nothing; restores need no per-client secret — the appliance requirement). Master key encrypts per-repo secrets. **Recovery Kit** (nagged at setup): master key + repo passwords + layout doc + bwctl offline instructions. Accepted trade-off: compromised server exposes plaintext — defense is host hardening + replica with different keys.
- **Ransomware layers:** (1) structural append-only port; (2) server-side-only prune; (3) delayed deletion — forget sets `deleted_at`, prune-eligible after 7-day soft-delete window, UI undelete, mass-forget requires admin + audit; (4) documented ZFS snapshots of repos dataset via TrueNAS (outside Breakwater's trust domain).
- **Web auth:** local users, argon2id, TOTP (pquerna/otp) + recovery codes, lockout, secure cookies. Roles: admin / operator / restore / viewer (MVP ships admin only; roles in Phase 2). First boot: one-time admin-setup token printed to container logs.
- **Audit taxonomy:** auth.login/.logout/.fail/.totp_fail · user.* · machine.enroll/.approve/.remove/.token_create · policy.* · job.run_manual/.cancel · restore.browse/.file/.volume/.bmr/.vm · retention.forget/.undelete/.prune_run · replication.* · settings.change · update.publish/.rollout. Restores are first-class audit events.

### Deployment
Single distroless container `ghcr.io/<org>/breakwater`, ports 8443 (web) + 9443 (agents); volumes `/data` (catalog+keys; ZFS recordsize=64K) and `/repos` (bulk; ZFS recordsize=1M, compression=lz4, **dedup=off** — kopia already compresses/encrypts). TrueNAS SCALE: compose as Custom App now, `truenas/` app manifest later. Bare-metal: one static binary + systemd unit, `/var/lib/breakwater/{data,repos}`, minimal `/etc/breakwater/config.yaml` (ports/paths/hostname only — few knobs by design). No sidecars ever.

### Repo layout (monorepo, Go workspace — modules keep Windows deps out of server builds)
```
breakwater/
├── go.work
├── proto/breakwater/v1/        # buf.yaml + protobuf; generated Go into pkg/
├── pkg/                        # shared module: wire types, snapshot/manifest formats,
│                               #   content-ID helpers wrapping kopia hashing/splitter
├── server/cmd/breakwaterd/ + internal/{vault,api,web,scheduler,catalog,keystore,audit,notify,replication}
├── agent/cmd/breakwater-agent/ + internal/{service,vss,fileback,imageback,hyperv,restore,update}
├── restore/                    # breakwater-restore TUI (linux+windows builds)
├── cli/                        # bwctl (own module)
├── web/                        # React+Vite → dist → go:embed
├── iso/                        # Alpine restore-ISO build (Dockerized, CI)
├── winpe/                      # WinPE builder scripts (output never redistributed)
├── packaging/{msi,docker,systemd,truenas}   # WiX v5 (MS-RL build tool only; output MSI is ours)
├── docs/                       # format spec, recovery runbook, threat model, runbooks
└── .github/workflows/
```

### Web UI
React 18 + TypeScript + Vite static build via go:embed (no SSR); TanStack Router+Query; SSE for live job progress; Tailwind + shadcn-style components; Recharts. Six MVP screens: **Dashboard** (fleet health tiles, last-24h strip, capacity+trend, dedup ratio, replication lag) · **Machines** (list → detail: inventory, policy, snapshot timeline, job history; Add-machine modal mints token + installer one-liner) · **Restore** (snapshot picker → lazy file-tree browser, download or restore-to-agent; image/VM wizards; BMR token generation) · **Activity** (live jobs, history, logs) · **Settings** (users/TOTP, policy editor with pre-filled defaults, alerts, replication peers, update rollout) · **Audit** (filters, chain-verify indicator, CSV export).

### Dependency ledger (all permissive, MIT-compatible)
grpc-go (Apache-2.0) · kopia (Apache-2.0, pinned + vendored) · modernc.org/sqlite (BSD-3) · x/sys, x/crypto (BSD-3) · go-ole, microsoft/wmi (MIT) · robfig/cron (MIT) · pquerna/otp (Apache-2.0) · klauspost/compress (Apache/BSD) · oklog/ulid (Apache-2.0) · wneessen/go-mail (MIT) · fxamacker/cbor (MIT, if tree objects outgrow JSON) · vendored restic `vss_windows.go` (BSD-2, notice retained) · cloudbase/rct-service consulted as Apache-2.0 reference. THIRD_PARTY_NOTICES.md from day 1.

## Phased roadmap

**MVP thesis:** a backup product with 4 features that provably restores beats one with 12 features that probably restores. MVP = scheduled Windows file backup with retention, alerting, and a finished restore path. Deferred categories are covered by the coexisting Barracuda during trust-building.

### Phase 1 — MVP (~13 weeks): production Windows file backup

**IN:** Docker server (single container; SQLite as *rebuildable index* — authoritative manifests live in the repo, so a dead server rebuilds from the repo directory alone); enrollment (one-time token → mTLS keypair → pinned fingerprints; machine appears in UI seconds after MSI install); VSS crash-consistent **writer-involved** file backup (vendored restic requester, `VSS_CTX_BACKUP` — writers freeze/thaw so databases are consistent-on-disk, no log truncation yet) with SYSTEM + `SeBackupPrivilege` + `FILE_FLAG_BACKUP_SEMANTICS` + `BackupRead` (data+ACLs+ADS); incremental via CDC + metadata-compare skip (no USN/CBT needed at this scale); at-rest AES-256-GCM per-repo encryption + mTLS transit; one repo per client, append-only agent rights, server-side-only forget/prune; schedules + GFS retention applied at enrollment; snapshot browser + file/folder restore (original path, alternate path, **different enrolled machine**, browser download; ACLs/ADS via `BackupWrite`); "flatten restore" runbook for full-machine recovery (fresh Windows + agent + push full snapshot); web UI core screens; multi-admin (no roles yet) + audit middleware on ALL endpoints from day 1; SMTP failure alerts + **missed-backup watchdog** + daily digest; scrub job (rotating chunk subset re-read + hash verify); `/healthz`.

**OUT (justified):** image-level backup (only pays off with BMR ISO; file-level + cross-machine restore satisfies "full restore" for MVP; ONE obligation now: repo format versioned so the fixed-block object type can be added later); system-state *restore claims* (files are captured; the claim needs System Writer work — Phase 5); Hyper-V (Phase 3); BMR ISO (Phase 4); replication (Phase 2); component-based VSS (Phase 5); RBAC roles/immutability UI (Phase 2); agent auto-update (hard-gated on code signing — unsigned auto-update is an RCE product); bandwidth throttling; Linux agent/S3/Prometheus/search (Phase 6).

**Engine decision gate (close by end of week 2):** 3-5 day spike — primary: embed kopia's repository packages driven by our own snapshotter (NOT kopia's VSS layer); fallback: vendor restic's repository layer (BSD-2, notice retention). Gate: from a Go test on Linux, write 10GB of chunked data → restore → verify → retention+GC. If neither is ergonomic in 5 days, escalate to the project owner before writing a bespoke format.

**Milestones (each ends in a demo):**
- **M1 (wk1-2):** monorepo + server in Docker on the real TrueNAS box + SQLite migrations + gRPC proto (enroll/control/data) frozen + enrollment tokens + mTLS against fake Linux client + engine gate closed. CI: Linux tests, Windows cross-compile. *Demo: fake client enrolls; wrong-cert client rejected live.*
- **M2 (wk3-4):** Windows agent service (SYSTEM) + WiX MSI + persistent dial-out with keepalives + server-dispatched jobs + plain-directory backup (chunk → have/want → append-only upload → manifest) + UI shell against fake API. *Demo: MSI install → appears in UI in 10s → backup → second run shows dedup ratio.* Also build golden-dataset generator + comparer (see Verification).
- **M3 (wk5-6):** VSS wired in; full C: from shadow device; exclusion defaults (pagefile/hiberfil/VSS store); snapshot cleanup guaranteed on every exit path; standalone `vsscheck.exe` diagnostics (list writers, create snapshot, read locked file, dump writer metadata). *Demo: with SQL Express running + SYSTEM-only ACL'd files: all captured; `vssadmin list shadows` shows zero leftovers.*
- **M4 (wk7-8):** restore is the product — snapshot tree browser; restore to original/alternate/different machine; ACL/ADS restore; conflict policy (overwrite/rename/skip). *Demo: corrupt a tree, restore, automated comparer proves byte+ACL+ADS+timestamp equality; cross-machine restore.*
- **M5 (wk9-10):** schedules + GFS engine + forget/prune split with 7-day prune grace + scrub + SMTP alerts + missed-backup watchdog. Time-warp harness (fake clock) drives 90 simulated days through retention in minutes. *Demo: expected keep-set exact; bit-flipped chunk caught by scrub → email; silent machine → watchdog email.*
- **M6 (wk11-12):** audit UI, multi-admin, daily digest (machine/last success/size/duration/status table), dashboard, chaos drills executed, server-loss drill, docs (TrueNAS quickstart, agent install, restore runbooks, security model), tag **v0.1.0** (unsigned MSI + published SHA256s). *Demo: full Trust Checklist executed live.*
- **Wk13 (buffer/pilot):** 2-3 non-critical production servers dual-run alongside Barracuda. Pre-agreed cut line if a milestone behind at M4: digest→M7, alt-machine restore UI (keep API)→M7, multi-admin→M7. **Never cut: scrub, alerts, ACL-correct restore, audit middleware.**

### Phase 2 — Second copy & hardening (~5-6 weeks)
Instance-to-instance replication (pull-free, source-push per §Replication), replica-side verify, immutable-retention windows + delayed deletion (admin undo window), RBAC roles (admin/operator/restore/viewer), webhooks, Prometheus endpoint, scheduled restore-drill feature (product periodically restores random files, reports in digest), code-signing cert acquired, agent auto-update **behind signing**.
*Entry:* v0.1 ran 2+ weeks in production; Trust Checklist green; replica target exists (VM on TrueNAS initially; offsite later). *Exit:* primary-loss drill — restore from replica with primary off; simulated-compromised-admin forget recoverable within delay window.

### Phase 3 — Hyper-V guest backup (~8-10 weeks)
Fixed-block image object type live; WMI v2 `Msvm_*` (`CreateSnapshot` type 32768 → `ConvertToReferencePoint`); fulls via WMI Export (VMCX+VMGS+VMRS+VHDX); incrementals via `QueryChangesVirtualDisk` (agent-on-host, local API fine); app-consistent when guest Integration Services "Backup" on, auto-fallback to crash-consistent surfaced per-VM in UI; restore to same or different enrolled host. Defer cluster/CSV explicitly. WS2016+/VM config v8+ floor documented.
*Entry:* Phase 2 replication live (images are big — prove the second-copy pipeline first); WMI spike harness done (≤1wk, during Phase 2); Hyper-V test capacity identified (see Verification). *Exit:* 2 weeks nightly full+RCT of 3+ running guests (incl. one Linux, one without IS); restored VM boots on a *different* host; broken RCT chain (host reboot) degrades to full WITH alert.

### Phase 4 — Physical image backup + BMR (~10-12 weeks)
Volume images of physical machines reusing the fixed-block store; v1 incrementals = block-hash compare vs previous manifest (no kernel CBT driver EVER); Linux restore ISO first (redistributable), ISO **dials out** like the agent; restore: recreate GPT/ESP → write volumes → `bcdboot /f UEFI` → `reagentc`; then WinPE **builder** (user-run, needs ADK; verify current EULA); dissimilar-hardware driver injection (offline `Dism /Add-Driver`) last, timeboxed, shipped with a supported-scenario matrix.
*Exit:* BMR drill matrix — same-hardware boots; physical→VM boots; VM→different-generation boots; one dissimilar target boots after driver injection; ISO-boot-to-login < 2h for 200GB.

### Phase 5 — App-consistent component-based VSS (~6-10 weeks)
Writer metadata parsing, `AddComponent`/`SetBackupSucceeded`; real "System State" claim (NTDS writer — matters for this fleet's domain controllers); SQL log truncation only if SQL enters the fleet. Freeze≤60s/commit≤10s telemetry. **Pre-declared fallback:** AlphaVSS (MIT, C#) side-car if pure-Go component mode intractable after a 3-week spike — decide at week 3, not week 9.

### Phase 6 — Breadth (backlog)
Linux agent, S3 replication targets, capacity forecasting, cross-client search (confirmed user want — pull forward if demand), instant recovery. Prioritize by community demand.

## Verification

**Lab topology (no dedicated lab — production-only environment):**
- Server: staging dataset on the real TrueNAS box + docker-compose on dev machine for fast loops.
- Windows targets: **KVM VMs hosted on the TrueNAS SCALE box itself** — WS2022 (primary: SQL Express for lock testing, ACL'd trees, ADS fixtures), WS2019 (version floor), Win11 (edge), each with snapshots for repeatable runs. Hyper-V testing (Phase 3): nested virtualization in a TrueNAS KVM VM (Hyper-V role inside a Windows VM) — validate early during Phase 2's WMI spike; if nested proves flaky, fall back to maintenance windows on a production Hyper-V host with throwaway guest VMs.
- **Golden dataset generator + comparer (build in M2, reuse forever):** Go tool fabricating SYSTEM-only ACLs, ADS, sparse files, >260-char paths, unicode names, junction/symlink loops, hardlinks, 0-byte + multi-GB files, deny-share-locked files; comparer asserts byte + ACL/SD + ADS + timestamp equality — used by every restore assertion in the project.

**CI (GitHub Actions):** ubuntu — engine/server unit + property tests (retention math: random forget/prune sequences never orphan referenced chunks; race detector on). windows-latest — agent unit tests + **real VSS smoke test** (hosted runners run elevated: create snapshot set, read file through shadow device, delete; highest-value CI test in the project). Self-hosted runner (later, on a TrueNAS VM) — nightly full integration: revert VM snapshot → fresh MSI → enroll → backup golden dataset → mutate → incremental → restore (alt-path + cross-machine) → comparer → retention time-warp → scrub with injected corruption. Release: GoReleaser (server binary + multi-arch Docker → GHCR), WiX MSI job, SBOM + checksums.

**Chaos drill matrix (automate from M5-M6):** (1) kill agent mid-upload → resume, no leaked VSS snapshot; (2) docker-kill server mid-upload → repo consistent (temp-then-rename pack writes, SQLite WAL), agent resumes; (3) 30s network partition → retry, no duplicate manifests; (4) server ENOSPC → clean fail + alert, repo untouched; (5) agent clock ±3d → server clock governs, warning surfaced; (6) token reuse/unknown cert/server-cert-swap all rejected (pinning proven); (7) compromised-agent simulation: agent creds attempt delete/overwrite → denied (append-only proven); (8) bit-flip a pack file → scrub identifies affected snapshots; (9) reboot during window → watchdog fires; (10) `kill -9` fuzz loop (≥500 iter) around backup+prune concurrency → prune never removes referenced data (scariest bug class; ALSO property-tested).

**Backup Trust Checklist (all green before production points at Breakwater):**
1. Full C: with SQL running captures locked + SYSTEM-only files (comparer-verified)
2. Restore byte+ACL+ADS+timestamp identical
3. Cross-machine restore works
4. kill -9 fuzz ≥500 iterations, repo always consistent
5. Prune property tests green; grace period enforced
6. Injected corruption detected + alerted
7. ENOSPC non-destructive + alerted
8. mTLS both directions proven
9. Append-only proven from agent credentials
10. Missed/failed backup emails within the window
11. Zero VSS shadow-copy leaks across 100 runs
12. Server-loss drill: metadata rebuilt from repo dir alone on fresh container; restore succeeds
13. Runbooks (file restore / flatten restore / server-loss) each executed once cold, following only the doc

## Project hygiene & OSS launch

- **Repo:** monorepo `breakwater` (layout above). Day 1: MIT `LICENSE` + `THIRD_PARTY_NOTICES.md` (restic BSD-2 vendored code requires notice retention; kopia Apache-2.0 requires NOTICE propagation), CONTRIBUTING, SECURITY.md (private vuln reporting — this is security software), DCO (not CLA).
- **Compatibility promises published at v0.1.0** (trust products themselves): (a) repo format — every future version reads ≥0.1.0 repos, migrate-forward-only; (b) protocol — server N supports agents N-1. Channels: edge (main) / beta (rc) / stable. SemVer 0.x through Phase 3.
- **Code signing:** MVP ships unsigned (SHA256s published + SmartScreen doc note). Acquire during Phase 2: **Azure Trusted Signing (~$10/mo)** if eligible, else OV cert (~$200-500/yr; check Certum's open-source cert ~€70/yr). 1-4 weeks validation — start early. Auto-update NEVER ships before signature-verified updates.
- **Docs (mkdocs-material → GitHub Pages):** TrueNAS quickstart first; Concepts (repo/trust model, append-only); restore runbooks; retention guide; security model (publish the Trust Checklist — it IS the marketing); VSS writer-failure troubleshooting page (will be the most-visited page — write it early); API reference.
- **Announcement:** repo public from day 1 with a "pre-release — do not trust with sole copies" banner. Do NOT announce (r/selfhosted, HN) until v0.2.x: replication shipped, Trust Checklist + CI evidence public, signed installers, quickstart proven by an outside tester (~month 5-6). Backup software gets one first impression.

## Migration & coexistence (Barracuda → Breakwater)

- **Month 0 (now, independent of dev):** buy runway — trim Barracuda retention to operational minimum, prune stale sources. It must stay healthy through month ~5 as the second copy.
- **Months 1-3:** Barracuda sole production backup; Breakwater in staging.
- **Month 3:** v0.1.0 dual-run pilot on 2-3 non-critical servers; success bar = 14 consecutive clean days + weekly restore drills.
- **Month 4:** full fleet file backups on Breakwater; step Barracuda file retention down (this relieves the disk crisis); Barracuda keeps Hyper-V + second-copy role. **Rule: no dataset ever has fewer than two independent backup systems until Breakwater replication exists.**
- **Month ~5 (post-Phase 2):** replica instance verified via primary-loss drill → retire Barracuda file jobs.
- **Month ~7-8 (post-Phase 3):** Hyper-V cutover after 2-week dual-run + boot drill. Barracuda cold but retained until needed retention points age out.
- **Post-Phase 4:** BMR closes the last gap; appliance disposed.

**Suggested retention defaults ("Standard Server" policy at enrollment):** keep-last 3, daily 14, weekly 8, monthly 12, yearly 2; nightly 20:00-06:00 window; prune grace 7 days (the oops/ransomware undo window); scrub subset daily + full read-back monthly. Offer a "Long Retention" variant (daily 30 / weekly 12 / monthly 24 / yearly 5) — 66TB makes space cheap: <20TB source at typical 2-4x dedup+zstd fits with years of headroom.

## Critical first files (M1)
- `server/internal/vault/vault.go` — the vault interface + kopia embedding; the single most load-bearing file (storage decision lives here)
- `proto/breakwater/v1/breakwater.proto` — the entire agent/replication wire contract
- `pkg/format/snapshot.go` — Breakwater-native tree-object and image-manifest formats (shared by agent, server, restore, bwctl)
- `agent/internal/vss/vss_windows.go` — vendored restic BSD-2 VSS requester + extensions
- `server/internal/catalog/schema.sql` — catalog schema
