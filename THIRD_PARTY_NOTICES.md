# Third-Party Notices

Breakwater is MIT-licensed. This file retains notices required by third-party components and lists major dependencies. All listed licenses are MIT-compatible (MIT, Apache-2.0, BSD-2/3, ISC, etc.). **No AGPL or GPL libraries are linked into Breakwater binaries.**

GPL tools (e.g. wimlib, partclone, ntfs-3g) may appear only as separate processes on the Linux restore ISO and do not affect the MIT license of Breakwater code.

---

## Direct dependencies (planned / in use)

| Component | License | Notes |
|-----------|---------|-------|
| [kopia](https://github.com/kopia/kopia) | Apache-2.0 | Storage engine packages under `server/internal/vault` only; pinned + vendored |
| [grpc-go](https://github.com/grpc/grpc-go) | Apache-2.0 | Agent/server transport |
| [protobuf](https://github.com/protocolbuffers/protobuf-go) | BSD-3 | Generated wire types |
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | BSD-3 | Pure-Go SQLite catalog (`CGO_ENABLED=0`) |
| [golang.org/x/sys](https://golang.org/x/sys) | BSD-3 | OS primitives |
| [golang.org/x/crypto](https://golang.org/x/crypto) | BSD-3 | Crypto helpers |
| [oklog/ulid](https://github.com/oklog/ulid) | Apache-2.0 | Primary keys |
| [robfig/cron](https://github.com/robfig/cron) | MIT | Schedule parsing (later milestone) |
| [pquerna/otp](https://github.com/pquerna/otp) | Apache-2.0 | TOTP (later milestone) |
| [wneessen/go-mail](https://github.com/wneessen/go-mail) | MIT | SMTP (later milestone) |
| [klauspost/compress](https://github.com/klauspost/compress) | Apache-2.0 / BSD | Compression |
| go-ole / microsoft-wmi | MIT | Hyper-V WMI (Phase 3) |

---

## Vendored / copied code

### restic VSS requester (planned for agent)

- **Source:** `restic` project, `internal/fs/vss_windows.go` and related helpers
- **License:** BSD-2-Clause
- **Location (when vendored):** `agent/internal/vss/`
- **Requirement:** Copyright notice and license text retained in the vendored file(s)

BSD-2-Clause summary: Redistribution in source and binary forms permitted with copyright notice retention; no warranty.

---

## Apache-2.0 NOTICE propagation (kopia)

Kopia is licensed under the Apache License, Version 2.0. When distributing Breakwater binaries that include kopia packages, the Apache-2.0 license terms apply to those components. See:

- https://github.com/kopia/kopia/blob/master/LICENSE
- https://github.com/kopia/kopia/blob/master/NOTICE (if present in the pinned version)

A copy of the Apache-2.0 license text is available at:
https://www.apache.org/licenses/LICENSE-2.0

---

## Generating a full SBOM

Release builds will publish an SBOM (e.g. via GoReleaser / Syft) listing exact module versions. Until then, `go list -m all` in each module directory is authoritative for the build.
