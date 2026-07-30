# Contributing to Breakwater

Thank you for your interest in contributing. Breakwater aims for appliance-grade simplicity and production trustworthiness — quality and safety over feature volume.

## License and DCO

All contributions are licensed under the [MIT License](LICENSE).

We use the **Developer Certificate of Origin (DCO)** instead of a CLA. Every commit must be signed off:

```bash
git commit -s -m "Your commit message"
```

This adds a `Signed-off-by:` line certifying that you have the right to submit the work under the project license. See [DCO 1.1](https://developercertificate.org/).

Git tip: `git config format.signOff true` to sign off automatically.

## Hard rules

1. **No AGPL or GPL code** in the Breakwater tree or as library dependencies. AGPL projects (Bareos, UrBackup, PBS) are concept references only. GPL tools may run only as separate processes on the Linux restore ISO.
2. **All kopia imports** stay inside `server/internal/vault`. No other package may import `github.com/kopia/kopia/...`.
3. **Agent gRPC port (`:9443`)** must never expose a destructive RPC (delete, prune, retention mutation). Append-only is structural.
4. **Dependency ledger** — new deps must be MIT / Apache-2.0 / BSD-compatible. Update `THIRD_PARTY_NOTICES.md`.
5. Follow [PLAN.md](PLAN.md) as the design source of truth. One-way-door decisions not covered there need maintainer discussion first.

## Development setup

- Go 1.23+
- Docker (for image builds)
- Optional: protoc + buf for regenerating protobufs

```bash
git clone https://github.com/<org>/breakwater.git
cd breakwater
go work sync
go test ./...
```

Windows agent code is in `agent/`; it is not required for server-only work.

## Pull requests

1. Fork and branch from `main`.
2. Keep PRs focused; match existing style.
3. Add or update tests for behavioral changes.
4. Ensure CI is green (Linux tests + Windows cross-compile).
5. Sign off all commits (`-s`).
6. Reference any related PLAN.md milestone or checklist item.

## Code of conduct

Be respectful. Assume good intent. Backup software mistakes can destroy livelihoods — prefer caution in reviews involving storage, crypto, or restore paths.

## Security

Do not report security issues in public PRs or issues. See [SECURITY.md](SECURITY.md).
