# Security Policy

Breakwater is backup and disaster-recovery software. Security issues may affect the integrity or confidentiality of protected data. Please treat vulnerability reports with care.

## Supported versions

| Version | Supported |
|---------|-----------|
| pre-0.1 (main) | Best-effort during development |
| ≥ 0.1.x | Security fixes for the latest minor |

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Please report privately via one of:

1. **GitHub Security Advisories** — use “Report a vulnerability” on the repository’s Security tab (preferred once the repo is public).
2. **Email** — if a security contact is listed in the repository’s GitHub organization profile, use that address. Include:
   - Description of the issue and impact
   - Steps to reproduce (PoC if available)
   - Affected component (server, agent, protocol, storage, UI)
   - Your preferred contact method for follow-up

We aim to acknowledge reports within **72 hours** and provide an initial assessment within **7 days**.

## Scope

In scope (non-exhaustive):

- Authentication / authorization bypass (web, API tokens, enrollment, mTLS)
- Ability for an agent or unauthenticated client to delete, prune, or mutate retention (append-only boundary)
- Cryptographic failures (repo encryption, key custody, fingerprint pinning)
- Path traversal or arbitrary file write during restore
- Remote code execution on server or agent
- Cross-client data leakage between machine repositories

Out of scope:

- Denial of service against a self-hosted instance you control
- Issues requiring physical access to the server host without privilege boundary crossing
- Vulnerabilities solely in third-party dependencies that are already fixed in a newer release we have not yet upgraded to (please still note them)

## Safe harbor

We will not pursue legal action against researchers who:

- Make a good-faith effort to avoid privacy violations, data destruction, and service disruption
- Do not access data belonging to others beyond what is needed to demonstrate the issue
- Report findings promptly and do not exploit them beyond the PoC
- Do not publicly disclose before a coordinated release date agreed with maintainers

## Hardening notes (operators)

- Agents never hold decryption keys; the server holds the master key under `/data/keys/`.
- Protect `/data` and `/repos` with host-level access control and ZFS snapshots (outside Breakwater’s trust domain).
- Keep a Recovery Kit offline; it is required for total-loss DR.
- Never expose `:9443` (agent gRPC) without TLS; production always uses mTLS with fingerprint pinning.
- The agent-facing port is structurally append-only — do not proxy destructive admin APIs onto it.
