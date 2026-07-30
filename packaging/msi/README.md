# Breakwater Agent MSI (WiX v5)

## Silent install with enrollment token

```bat
msiexec /i breakwater-agent.msi /qn BWTOKEN=BW1:backup.example.com:9443:<server-fp>:<secret>
```

The service starts as **LocalSystem**, auto-start **delayed**, and enrolls on first start when `BWTOKEN` is supplied (also readable from `HKLM\Software\Breakwater\Agent\PendingEnrollToken`).

## Uninstall

```bat
msiexec /x breakwater-agent.msi /qn
```

Stops the service, removes binaries and agent state under `C:\ProgramData\Breakwater\`. **Server-side backups are never touched.**

## Build

See `build.ps1`. Requires WiX v5 (`wix` CLI) on Windows. CI `windows-latest` job produces the MSI artifact + SHA256 (unsigned for MVP).

## First real Windows run must verify

See PROGRESS.md § stage 4 untested-on-Windows list.
