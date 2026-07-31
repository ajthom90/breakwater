# Windows VM validation runbook

**Purpose:** convert the outstanding "untested on Windows" items into evidence,
and unblock M3 (VSS). Everything here needs a *real Windows install* — a build
runner cannot prove any of it. Work top to bottom; each step says what to record.

**Status of this document:** written 2026-07-31, before the VM exists. Update
statuses in `PROGRESS.md` ("Windows CI vs still-unproven runtime") and
`docs/trust-checklist.md` as items are proven — and only when actually observed.

---

## 0. VM setup (PLAN §Verification lab topology)

Host: TrueNAS SCALE (KVM). Guests, in priority order:

| Guest | Role | Why |
|---|---|---|
| **WS2022** | primary | matches production fleet; SQL Express for lock testing later (M3) |
| WS2019 | version floor | PLAN's stated minimum |
| Win11 | edge | optional; long-path + user-profile quirks |

Set up on WS2022 first — everything below can be done on that one guest.

**Take a VM snapshot named `clean-preinstall` before installing anything.**
Several drills below need a pristine machine, and reverting is far faster than
uninstalling. Take a second snapshot `enrolled` after step 2 succeeds.

Networking: the guest must reach the Breakwater server on **:9443** (agent) and
**:8443** (web/REST). Agents always dial out, so no inbound rules on the guest.

---

## 1. Artifacts

Take the MSI from a green CI run rather than building locally — it is the
artifact users will get.

```powershell
# From the repo host, download the latest CI artifacts (agent exe + MSI + SHA256)
gh run download <run-id> -n <artifact-name>
```

Verify the published SHA256 matches before installing. Record the run ID used.

---

## 2. MSI install with enrollment token  → untested item **#4**, **#5**

On the server, mint a token, then on the guest:

```powershell
# Verbose log is deliberate — we are testing that the token is REDACTED from it.
msiexec /i breakwater-agent.msi /qn BWTOKEN=BW1:<host:port>:<fp>:<secret> /l*v C:\install.log
```

**Record:**
- [ ] Install completes; service `BreakwaterAgent` exists and is `Running` as `LocalSystem`
- [ ] Machine appears in the UI / `GET /api/v1/machines` — **time it** (PLAN's demo says ≤10 s)
- [ ] **`C:\install.log` does NOT contain the token secret** (`Select-String` for the secret; this is the S4-F2 `Property/@Hidden` claim)
- [ ] `C:\ProgramData\Breakwater\pending-enroll.token` is **deleted** after successful enrollment
- [ ] `identity.json`, `cert.pem`, `key.pem` exist

> If the token appears in the log, that is a **finding** — stop and report it
> before continuing; it means the redaction mechanism does not work as believed.

---

## 3. State directory ACLs  → untested item **#2**

```powershell
icacls C:\ProgramData\Breakwater
```

**Record:**
- [ ] Only `SYSTEM` and `Administrators` have access; inheritance disabled
- [ ] As a **standard (non-admin) user**, reading `identity.json` is denied
- [ ] Note the ACL state of the folder *between* MSI `CreateFolder` and the
      agent's first `SecureDir` call (the known window — is it inherited-open?)

---

## 4. Service lifecycle  → untested item **#1**

```powershell
sc.exe stop BreakwaterAgent ; sc.exe start BreakwaterAgent
Restart-Computer      # then confirm the service came back on its own
```

**Record:**
- [ ] Stop is graceful: an in-flight backup job sends a terminal `JobResult`
      (the S3-F10 cancel-confirmation contract) rather than the server waiting
      out `CancelConfirmTimeout`
- [ ] Delayed auto-start brings the service up after reboot without a login
- [ ] Event-log source `BreakwaterAgent` receives entries
- [ ] Server marks the machine offline on stop and online again on start
      (heartbeat re-assert, S4-F8)

---

## 5. Volume inventory  → untested item **#3**

**Record:**
- [ ] Fixed drives appear with sane sizes
- [ ] An **empty CD/DVD drive does not panic** the agent (attach one)
- [ ] Network/mapped drives are excluded
- [ ] Volume IDs are stable across a reboot

---

## 6. Real backup + restore round trip  → untested items **#6**, and Trust Checklist **#2**

Generate the golden dataset **on Windows** (full fixture set — ACLs, ADS, sparse,
junctions, long paths, deny-share-locked), back it up, restore it, compare.

```powershell
go run ./tools/golden generate -root C:\bw-golden          # exact flags per tools/golden
# trigger a file-backup job for C:\bw-golden, then a restore to C:\bw-restored
go run ./tools/golden compare -a C:\bw-golden -b C:\bw-restored
```

**Record:**
- [ ] Comparer reports **equal** with **zero skipped ACL/ADS checks** (on Windows
      these must actually run, not skip — skips here mean the SD/SDDL path is
      not working)
- [ ] Byte, ACL/SD, ADS, and timestamp equality all asserted
- [ ] A **deny-share-locked** file is captured (this is the VSS motivation — on
      the plain-directory path it may legitimately fail; record which)

> This is the first real test of the S4-F7 SDDL comparison path.

---

## 7. Uninstall  → untested item **#4** (second half)

```powershell
msiexec /x breakwater-agent.msi /qn /l*v C:\uninstall.log
```

**Record:**
- [ ] Service stopped and removed
- [ ] Program files removed
- [ ] **Server-side backups untouched** — verify snapshots still list and still
      restore after the agent is gone (this is a core promise; uninstalling an
      agent must never destroy its backups)

---

## 8. Power-loss durability  → untested item **#7**

With the agent enrolled and idle, **hard-kill the VM** (destroy power, not
shutdown) and boot it again.

**Record:**
- [ ] `identity.json` is intact and the agent reconnects without re-enrolling
      (this is the S4-F4 fsync-before-rename claim; if it comes back unenrolled,
      the token is already burned and that is a **finding**)
- [ ] Repeat ~5 times, including a kill *during* a backup

---

## 9. Then: M3 (VSS)

Only after the above is green is M3 worth starting. M3's own gates (PLAN M3):

- Full `C:` from the shadow device
- Exclusion defaults (pagefile, hiberfil, VSS store)
- **Snapshot cleanup guaranteed on every exit path** — `vssadmin list shadows`
  shows zero leftovers after 100 runs (Trust Checklist #11)
- `vsscheck.exe` diagnostics (list writers, create snapshot, read locked file,
  dump writer metadata)
- With SQL Express running: SYSTEM-only ACL'd and locked files all captured
  (Trust Checklist #1)

---

## Reporting back

For each item: **observed behavior**, not "looks right". Paste the command output
into the session or a file. Anything that does not match the expectation above is
a finding — those are the point of this exercise, and finding them here is
exactly where we want to find them.
