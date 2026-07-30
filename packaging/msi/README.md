# Breakwater Agent MSI (WiX v5)

## Silent install with enrollment token

```bat
msiexec /i breakwater-agent.msi /qn BWTOKEN=BW1:backup.example.com:9443:<server-fp>:<secret>
```

- `BWTOKEN` is in `MsiHiddenProperties` so verbose logs (`/l*v`) redact it (S4-F2).
- At install, a deferred SYSTEM custom action writes the token to
  `C:\ProgramData\Breakwater\pending-enroll.token` (not world-readable HKLM).
- The service starts as **LocalSystem**, auto-start **delayed**, enrolls on first
  start, then **deletes** the pending token file (not blanked).

## Uninstall

```bat
msiexec /x breakwater-agent.msi /qn
```

Stops the service, removes binaries and agent state under `C:\ProgramData\Breakwater\`.
**Server-side backups are never touched.**

## Enroll-then-persist recovery (S4-F5)

If the Enroll RPC succeeds but the agent cannot persist identity (disk full, ACL
problem), the single-use token is already burned server-side and a machine row
exists. The agent error message states this explicitly.

**Recovery:**

1. Admin removes the stale machine row on the server (UI/CLI once available).
2. Mint a **fresh** `BW1:` token.
3. Reinstall or re-run the agent with the new token — the agent generates a **new**
   keypair (retrying with the same cert hits already-enrolled).

## Build

See `build.ps1`. Requires WiX v5 (`wix` CLI) on Windows. CI `windows-latest` job
produces the MSI artifact + SHA256 (unsigned for MVP).

## First real Windows run must verify

See PROGRESS.md § stage 4 untested-on-Windows list (includes token-at-rest path,
SD comparison, directory fsync after rename, MSI custom action token write).
