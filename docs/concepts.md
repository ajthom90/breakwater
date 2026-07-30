# Concepts (draft)

See [PLAN.md](../PLAN.md) for the full design. Short summary for operators:

## Trust model

- **One repository per machine** under `/repos/<machine-id>/`. Cross-client dedup is deliberately sacrificed to avoid content-ID confirmation attacks.
- **Agents never hold decryption keys.** They receive only a hashing key for content-addressed have/want. Restores are authorized by the server (or offline via Recovery Kit + `bwctl`).
- **Append-only agent port (`:9443`).** No delete/prune/retention RPCs. Compromised agents cannot destroy history. Prune runs server-side only, with a soft-delete grace window (default 7 days).

## Enrollment

Token format: `BW1:<host:port>:<serverCertFingerprintSHA256>:<secret>`

The server fingerprint travels **inside** the token (zero TOFU). Tokens are single-use and expire after 24 hours.

## Storage

Breakwater uses kopia’s low-level content-addressed packages (Apache-2.0) behind `server/internal/vault`. Snapshot *policy* and VSS layers from kopia are **not** used. File trees and (later) fixed-block image manifests are Breakwater-native JSON objects.

## Recovery Kit

Created at first setup (nagged): master key + per-repo passwords + layout notes + offline `bwctl` instructions. Required for total server loss. Keep offline and offline-encrypted.
