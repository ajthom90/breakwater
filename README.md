# Breakwater

> ⚠️ **PRE-RELEASE SOFTWARE — DO NOT TRUST WITH SOLE COPIES**
>
> Breakwater has not yet completed the [Backup Trust Checklist](docs/trust-checklist.md).
> Until a stable release ships with public CI evidence, treat every backup as experimental.
> Keep your existing backup system (e.g. Barracuda) as the authoritative second copy.
> See [PLAN.md](PLAN.md) for the full roadmap and verification gates.

**Breakwater** is a fully open-source (MIT), self-hosted backup and disaster-recovery platform designed for appliance-grade simplicity: install the agent → it appears on the server → sensible defaults protect it immediately.

## Status

| Item | State |
|------|-------|
| License | MIT |
| Phase | 1 — MVP (Windows file backup) |
| Milestone | M2 complete on Linux/darwin (see PROGRESS.md); Windows demo gated |
| Production use | **Not ready** |

## What it is

A single-container server (`breakwaterd`) plus a Windows agent (`breakwater-agent`) that:

- Backs up Windows file servers with VSS (crash-consistent, writer-involved)
- Stores data in content-addressed, encrypted, deduplicated repositories (one per machine)
- Enforces **append-only** agent rights — ransomware cannot destroy backups from a compromised client
- Provides file-level restore, retention (GFS), scrub, and multi-admin web UI

## Architecture (high level)

```
Windows agent  ──mTLS gRPC (:9443)──►  breakwaterd  ──►  /repos/<machine>/  (kopia CAS)
                     dial-out only          │
                                            ├── SQLite catalog (/data)
                                            └── Web UI + REST HTTPS (:8443)
```

See [PLAN.md](PLAN.md) for the complete design, research findings, and phased roadmap.

## Repo layout

```
breakwater/
├── proto/          # gRPC protobuf definitions
├── pkg/            # shared Go module (formats, content-ID helpers)
├── server/         # breakwaterd
├── agent/          # breakwater-agent (Windows)
├── restore/        # breakwater-restore TUI
├── cli/            # bwctl
├── web/            # React UI (embedded via go:embed)
├── packaging/      # Docker, MSI, systemd, TrueNAS
└── docs/           # runbooks, threat model, format spec
```

## Quick start (development)

```bash
# Requires Go 1.23+
go work sync
cd server && go test ./...
```

Docker image (after M1):

```bash
docker build -f packaging/docker/Dockerfile -t breakwater:dev .
```

## License

MIT — see [LICENSE](LICENSE). Third-party notices in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). We use DCO (Developer Certificate of Origin), not a CLA.
