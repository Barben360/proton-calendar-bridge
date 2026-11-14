# Windows port — design

- **Date**: 2026-11-14
- **Status**: approved by the author on 2026-11-14; built (decision 0018), live check pending. Where the build differs from this design, the text says so in a "Built:" line.
- **Decided by**: author (choices marked [author]); the rest is the agent's call, marked [call].
- **Amends**: SPEC §3 (Windows leaves "only builds"; "a GUI on Windows" leaves "not planned"), SPEC §13 "Platforms", decision 0015 ("Windows is next"), via a new decision 0018; `docs/install.md`, `docs/desktop.md`, `docs/status.md`, the README.

## 1. Intent

The author wants Windows users to run the bridge, installed later by an MSI where `pcb-ui` is required and `pcb` (the terminal version) optional [author]. The MSI needs binaries that work; today they do not:

- `cmd/pcb-ui/main_other.go` prints "pcb-ui is not supported on this system yet" and exits 2;
- `pcb login`, `pcb sync` and `pcb serve` exit 20 on Windows (no file lock, no control socket), and `internal/paths` fails without `HOME`, which a stock Windows does not set.

This project makes both binaries work on Windows. The MSI, Authenticode signing and the Windows live run are the next steps (§9).

## 2. Goals, non-goals, anti-goals

Goals:
- `pcb-ui.exe` on Windows 10 (1809 or later) and 11, amd64: tray icon, account window, add-account wizard, notifications, "Start at login", settings, the in-process daemon, Thunderbird over CalDAV and agents over MCP exactly as on Linux.
- `pcb.exe`: every command that works on Linux works on Windows, either in-process or through the running `pcb-ui`, except service management (§5.7).
- Secrets at rest encrypted for the Windows user; files and the control channel reachable by that user only.
- CI builds and tests both binaries on Windows; each release ships a `pcb_windows_amd64.zip`, attested like the other files.

Non-goals (not designed here):
- A Windows service, i.e. a bridge running with nobody logged in [author: pcb-ui only, started at login].
- The MSI, Authenticode signing, winget, Chocolatey or Scoop packages (next project).
- Windows on arm64; Windows Server; Windows before 10 1809 [call].
- The container image on Windows (Linux containers already run on Docker Desktop).
- A Windows `install.sh` equivalent (`install.ps1`): the zip and later the MSI cover it [call].

Anti-goals (failure even if it works):
- Another Windows user (or a process of another user) reads the state, the secrets or talks to the daemon.
- Two processes serve or migrate one state at once (the guarantee the Unix locks give today).
- A behaviour change on Linux or macOS, or `pcb` gaining cgo.
- Times written in UTC with no zone where Linux would write a TZID, because the Windows zone had no IANA name.

## 3. Constraints

- `pcb` stays `CGO_ENABLED=0` on every OS (`cmd/pcb/arch_test.go`); only `pcb-ui` uses cgo.
- New code is behind build tags (`windows` / `!windows`) beside the existing `unix` / `!unix` pairs; the Linux and macOS builds compile no Windows code. Built: "Windows code" means code that calls Windows APIs; the logic around it (pipe name, DPAPI error classes, doctor's Windows checks, the Run value's format, the foreign-sealer rule) stays untagged, so Linux tests it too.
- New dependencies are credited in the README's Credits section (CONTRIBUTING): `github.com/Microsoft/go-winio` (MIT) for named pipes; `golang.org/x/sys/windows` (already indirect, BSD) for locks, DPAPI, ACLs, the registry and the time zone.
- Existing state on Linux is untouched; there is no Windows state to migrate.
- The author's Windows 11 VM is available to the agent (SSH, an administrator and a standard user, auto sign-in of the standard user) [author]; CI's Windows runner runs the suites, the VM runs the live checklist (§6).

## 4. Approaches

Chosen by the author on the agent's recommendation [author]:

| Concern | Chosen | Rejected, and why |
|---|---|---|
| Run model | `pcb-ui` only, started at login; `pcb.exe` talks to it | A Windows service: a bridge without a logged-in user, but service registration, a service account, machine-scope secrets and an MSI that installs a service, about twice the work |
| CLI ↔ daemon | Named pipe restricted to the user by its ACL (`go-winio`) | AF_UNIX socket in `%LOCALAPPDATA%` (Go supports it on Windows 10 1803+): reuses more code, but no peer check, stale files and no `chmod` |
| Secrets | DPAPI (`CryptProtectData`, user scope): the key file is encrypted for this Windows user | Credential Manager (`CredWrite`): resembles the Linux keyring, but an extra library or raw calls, size limits and roaming-profile behaviour for no gain |
| `pcb-ui` build | Native on a `windows-latest` runner with mingw-w64 (cgo for Fyne) | Cross-compiling from Linux: no Windows test run, and not Fyne's supported path |

## 5. Design

### 5.1 Paths (`internal/paths`)

On Windows [call]:

| | Default | Override (unchanged) |
|---|---|---|
| Config | `%APPDATA%\pcb` (roams with the profile: `config.toml`) | `PCB_CONFIG_DIR` |
| State | `%LOCALAPPDATA%\pcb` (does not roam: sessions, cache, DPAPI key, logs, TLS) | `PCB_STATE_DIR` |

Resolved with `os.UserConfigDir` / `os.UserCacheDir`'s Windows sources (`APPDATA`, `LOCALAPPDATA`); neither set: the existing "set the overrides" error. `XDG_*` and `HOME` are not consulted on Windows. The DPAPI-protected key lives in the non-roaming state directory, with the secrets it protects: it never travels to another machine, so pcb never depends on how a roaming profile carries DPAPI master keys [call].

### 5.2 Access control (files and directories)

- Every directory pcb creates under the config and state roots gets a **protected DACL**: full control for the current user's SID and for `SYSTEM`, nothing else, no inheritance from the parent, and its **owner set explicitly to the user's SID** (an elevated process would otherwise make `BUILTIN\Administrators` the owner) [call, review]. Files created inside inherit it.
- Where Unix code checks `perm & 0077` or the owner UID (`store`, `tlsutil`, `doctor`, `seal/keyfile.go`), the Windows variant checks the DACL instead: an allow ACE for any SID other than the user, `SYSTEM` or `Administrators` is refused with the same error kinds and exit codes (the Windows message names the ACL and the fix, `pcb doctor`) [call].
- `os.Chmod` calls stay (harmless on Windows); the `runtime.GOOS == "windows"` skips in `keyfile.go`, `selfsigned.go`, `doctor/caldav.go`, `doctor/cache.go` are replaced by the DACL checks.
- Owner checks (`DirOwner`, `fileOwner`, `CheckStateOwner`; built: directories only, a file's owner is not checked, only its access list) accept the process token's user SID, and `BUILTIN\Administrators` only when the token is an elevated member of it (files an elevated run made before the owner was set explicitly, and CI's elevated runner); anything else is refused as today. Elevated runs are not refused: `pcb doctor` warns that pcb runs elevated [call, review].

### 5.3 Locks (`internal/filelock`)

`filelock_windows.go`: `TryLock` opens the file with `FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE` and takes `LockFileEx(LOCKFILE_EXCLUSIVE_LOCK|LOCKFILE_FAIL_IMMEDIATELY)` on **one byte at offset `1<<62`**, far past any content: Windows byte-range locks are mandatory, and `lockServe` writes, reads and truncates `serve.lock` through other handles, so the locked byte must never overlap the content [call, review]. Closing the handle releases it, as `flock` does on Unix. Same `TryLock`/`Lock` API, so every caller (serve, sync, layout, login, state, secrets, sealer, clients, tokens) gets cross-process exclusion; `ProbeServe` reads the content while it is locked. `ErrUnsupported` remains for other systems.

### 5.4 Control channel (`internal/ctl`)

- `listen_windows.go` / `dial_windows.go`: a named pipe `\\.\pipe\pcb-<hash>` where `<hash>` is the first 16 hex of SHA-256 of the **canonical** state directory (the final path from `GetFinalPathNameByHandle`, so case, 8.3 short names and junctions give one pipe; lower-cased), so two states (overrides) get two pipes and the name leaks no path [call, review].
- Security descriptor (SDDL): owner and full access for the current user's SID only, `D:P(A;;GA;;;<user SID>)`; remote clients refused (`PIPE_REJECT_REMOTE_CLIENTS`, go-winio's default).
- Peer check: the server reads the client's SID (`GetNamedPipeClientProcessId` → process token) and refuses another SID, as `SO_PEERCRED` does on Linux; the client checks the server process's SID the same way before sending anything [call]. Built: the client checks the pipe's **owner SID** (`GetSecurityInfo`), not the server process's: a squatter could serve from a handle whose creating process had exited, and the reused PID could be one of the user's; no user can make another user's SID a pipe's owner without SeRestorePrivilege.
- A pipe name already served by another user's process (squatting): the client's server-SID check refuses it, exit 10, as a foreign-owned socket does today. Built: exit 20 (Blocked, as every control-channel refusal), `the control pipe <name> is owned by another user (<SID>): pcb refuses to talk to it`; `Listen` on a name already served fails with exit 20 too.
- Everything above `Listen`/`Dial` (the HTTP protocol, `/v2/...`, single-instance `ui.Probe`, `pcb login` going through `pcb-ui`) is unchanged.

### 5.5 Secrets (`internal/store/seal`)

- A new sealer kind **`dpapi`**, recorded in `layout.json` (`"sealer": "dpapi"`) like `keyfile` and `secret-service` [call]. Its key: 32 random bytes, stored at `<state>\secrets.key.dpapi` as the output of `CryptProtectData` (user scope, `CRYPTPROTECT_UI_FORBIDDEN`, an entropy string naming pcb), written with the existing atomic-file code.
- On Windows `[secrets] backend = auto` chooses `dpapi`; `keyfile` stays available (a plain key file, DACL-protected) for those who want a portable key; `secret-service` is a configuration fault on Windows, exit 10 like every config fault (SPEC §7.3, ruling R2) [call, review].
- An unknown sealer on another OS (a `dpapi` state copied to Linux) is `ErrUnknownSealer`, exit 10, hint "this state was created on Windows" [call]. Built: `the state in <root> was created on <OS> (sealer <s>): this pcb cannot open it here`, hint `use it with pcb on <OS>, or move <root> aside and run pcb login to start a new state`; a `secret-service` state on Windows gets the same, naming Linux.
- Built (not in this design): an unprotect failure that is not an integrity failure (a logon without the password, such as SSH with a key; another user's key; a profile without its master key) is exit 20, `secrets: key-unavailable` with `secrets_cause` and the alert `key-unavailable`, phase kept, never a logout hint; integrity failures, a machine-scope blob and a wrong size are corrupt. A protect failure at the first login is exit 10.
- `uninstall --delete-data` deletes the DPAPI key with the state.

### 5.6 Time zone

- `time/tzdata` is embedded in both binaries on Windows (build-tagged import), so `time.LoadLocation` works without Go installed — today `[mcp] timezone` and the config's zone fail to load on a stock Windows [call].
- The process's zone: `GetDynamicTimeZoneInformation` gives the Windows key name (e.g. `W. Europe Standard Time`), mapped to its IANA name through CLDR's `windowsZones.xml` territory `001` entries, embedded as a generated Go table (`go generate`, the CLDR version recorded, credited) [call]. A key with no mapping keeps today's behaviour (offset label, UTC instants), and `pcb doctor` warns naming the key.
- This replaces "A zone with no name found (Windows, where Go reads no `TZ` …)" in SPEC §12 for Windows.

### 5.7 Commands and the CLI on Windows

| Command | On Windows |
|---|---|
| `login`, `sync`, `status`, `client …`, `account …`, `mcp`, `doctor`, `logout`, `version` | as on Linux (in-process, or through `pcb-ui` when it runs) |
| `serve` | works in a console (foreground, Ctrl-C stops it); refused while `pcb-ui` serves the state, as today |
| `install`, `service …` | exit 20, `no service on Windows`, hint `use pcb-ui; "Start at login" runs it at each login` [call] |
| `uninstall [--delete-data]` | stops nothing (refuses while `pcb-ui` or `serve` runs, exit 20, as today), removes the autostart entry, and with `--delete-data` the config and state directories (DPAPI key included). While it holds `serve.lock` (`HoldServe`) Windows cannot remove that file nor its directory: it deletes everything else first, then closes the lock and removes `serve.lock` and the directory; a server started in that instant finds no layout and stops, as on Linux after a deletion [call, review] |

- Signals: in a console, Ctrl-C (`os.Interrupt`) and the console close, logoff and shutdown events (which Go's `os/signal` delivers as `syscall.SIGTERM`) drain like SIGTERM on Linux; the existing `signal.Notify(SIGINT, SIGTERM)` calls already cover them [call]. `pcb-ui` has no console: at logoff or shutdown it must drain too, through the window session-end message (`WM_QUERYENDSESSION`/`WM_ENDSESSION`) reaching Fyne's lifecycle or a small hidden message window of its own; the builder picks the mechanism that works, and the live checklist (§6) proves it with a real logoff.
- Hints and `Fix` lines quote for PowerShell on Windows (`'…'` with `''` doubling), POSIX on Unix: `internal/shellquote` gets a per-OS `Quote` with its own safe-character set on Windows (`@ { } , ; ( ) $ \` ` and the like are special in PowerShell and always quoted) [call, review].
- `pcb doctor` on Windows: `platform` reports `windows/amd64: runs, untested` (warn) until the live check of §6 passes, then `ok` in the same project [call, review]; the DACL and owner checks of §5.2; an "elevated" warning; no systemd, desktop-entry or Secret Service checks (built: the `service` line says `pcb-ui`'s Start at login runs pcb there; also a `dpapi key` check, and a `time zone` check that warns on an unmapped key and is OK when `[mcp] timezone` is set); when the autostart value exists, a check that it names an existing `pcb-ui.exe`, warning when that is not the one beside `pcb.exe` (no new control-channel field) [call, review].

### 5.8 The desktop app (`cmd/pcb-ui`, `internal/ui/desktop`)

- `cmd/pcb-ui/main_windows.go` (build tag `windows && cgo`), sharing the Linux startup order; the `DISPLAY`/`WAYLAND_DISPLAY` check is dropped but the `glfw.Init`/`Terminate` probe is kept, so a machine without OpenGL 2.0 (a VM without 3D acceleration, some RDP sessions: Windows' own OpenGL is 1.1) gets exit 10 and a message naming OpenGL 2.0 and the workaround in `docs/troubleshooting.md` (Mesa's software `opengl32.dll` beside `pcb-ui.exe`, not shipped) [call, review]. `main_other.go` becomes `!linux && !windows || !cgo`.
- Built with `-ldflags -H=windowsgui` (no console window) and an embedded icon and version resource (`.syso` generated at build time from the existing SVG, via a pinned tool) [call]. `--version`, `--help` and usage errors attach to the parent console (`AttachConsole(ATTACH_PARENT_PROCESS)`) so they print when run from a terminal [call, review].
- `internal/ui/desktop/platform_windows.go`:
  - Tray: Fyne's system tray (always present on Windows); `TrayHost` true.
  - Notifications: `fyne.SendNotification` (Windows toast); a click is not reported by Fyne, so the notification text says where to look (as on non-Linux today). Toasts from a zip install may not show on every Windows build without a Start-menu shortcut carrying the app ID; they are best effort until the MSI creates that shortcut, the tray icon's attention state carries the same news, and the live check records what is seen [call, review].
  - Autostart: `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, value `pcb-ui`, data `"<absolute exe>" --background`; switching it off deletes the value; the MSI later sets the same value so both agree [call].
  - Open config / logs: `fyne.App.OpenURL(file://…)` (ShellExecute), already used. Built: `ShellExecute` with the plain path; a `file://` URL percent-encodes a non-ASCII profile name, which Windows decoded wrongly (`Zoë` became `ZoÃ«`).
- Single instance: unchanged (`ui.Probe` over the pipe).

### 5.9 Build, CI and release

- `Makefile`: `make build` and `make ui` honour `GOOS=windows` and append `.exe`; no new Windows-only make targets beyond what CI needs [call].
- CI (`ci.yml`): a `windows` job on `windows-latest` (pinned actions, `persist-credentials: false`), mingw-w64 from a pinned `msys2/setup-msys2` (or the runner's preinstalled one, builder's choice, pinned either way): `go vet ./...`, `go test -race` of the packages touched here (filelock, ctl, paths, store, seal, tlsutil, doctor, shellquote, ui, cmd/pcb, cmd/pcb-ui), `make build ui`, then `pcb.exe version --json`, `pcb.exe doctor --json` and `pcb-ui.exe --version`. The `GOOS=windows go vet` step in `check` stays. Built: two jobs, `windows` (vet, `make build ui`, the three commands) and `windows-test` (`go test -race`, sharded `service`, `cli` and the rest), with `msys2/setup-msys2` pinned by SHA (UCRT64).
- Release (`release.yml`): `pcb` for windows/amd64 cross-compiled with the others; `pcb-ui` for windows/amd64 built on a `windows-latest` job; a Windows smoke job runs both from the archive; `scripts/release/package.sh` accepts `windows` and writes `pcb_windows_amd64.zip` (`pcb.exe`, `pcb-ui.exe`, `LICENSE`, `README.md`), byte-reproducible like the tarballs (built: `scripts/release/mkzip.py`, identical for the same zlib build); `checksums.txt` and the attestations cover it; the asset count checks grow accordingly. `install.sh` refuses Windows with a pointer to the zip [call].

### 5.10 Files replaced while another process reads them

Windows refuses to rename over a file another process holds open unless that process opened it with `FILE_SHARE_DELETE`, which Go's `os.Open` does not ask for. A `pcb status` reading `state.json` while `pcb-ui` writes it would make the write fail. So, on Windows [call, review]:
- every read of a file that `atomicfile` replaces goes through one helper that opens with `FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE` (build-tagged; `os.Open` elsewhere);
- `atomicfile`'s rename retries on a sharing violation or access denied for at most 2 s (a reader outside pcb, such as an antivirus scan, can still hold the file), then fails as today;
- the layout migration's directory renames get the same bounded retry.

Built: `os.Rename` (`MoveFileEx`) refuses to replace a file another handle holds open even with `FILE_SHARE_DELETE`, so a file is replaced by a POSIX rename: `SetFileInformationByHandle(FileRenameInfoEx)` with `FILE_RENAME_FLAG_REPLACE_IF_EXISTS|FILE_RENAME_FLAG_POSIX_SEMANTICS`, the source opened with `FILE_FLAG_OPEN_REPARSE_POINT`. The target name is NUL-terminated: Windows ignores `FileNameLength` and reads up to the first NUL (the cause of the first build's flaky renames). A directory at the target is refused at once with `EEXIST`, as Go's Unix `rename` does, instead of an access denied retried for 2 s; directories themselves still go through `os.Rename`.

### 5.11 Logoff and shutdown

Windows ends a console process about 5 s after the close, logoff or shutdown event, and a console process that loads `user32.dll` gets no logoff or shutdown event at all; `pcb-ui` gets the window session-end message. The daemon's drain (5 s today) is cut to 2 s on a session end, and what is lost by a cut drain is only what a crash would lose: files are replaced atomically and the cache is SQLite in WAL mode, so the next start recovers [call, review]. A test kills a serving process mid-sync on the Windows runner and checks the next start is clean.

## 6. Testing (tests that bite)

- Unit tests on the Windows runner for each Windows file: two processes contending a lock (a helper process), the pipe refusing another SID (a test that impersonates is not available; instead: the server refuses a client whose SID differs, with the SID lookup injected) and accepting the same user, the DACL created and the check refusing an added `Everyone` ACE, the DPAPI round trip and a tampered blob refused, the CLDR mapping for a fixed list of keys, the Run-key write/delete, the PowerShell quoting table.
- The existing suites (`internal/service`, `internal/cli`, `internal/ui`) run on Windows in CI; tests that are Unix-specific by nature stay tagged; any test skipped on Windows says why.
- Linux and macOS: the existing suites unchanged, proving no behaviour change.
- A live checklist, `spec/HUMAN-CHECKS.md` step 12 (built: step 11 is the first release's), run by the agent on the author's Windows 11 VM as its standard user (test account only; elevation hides the permission problems a real user meets) [author: the agent may use the VM and install anything on it]: install from the zip, add the test account in `pcb-ui`, Thunderbird on Windows, `pcb status` through the pipe, tray, notification, "Start at login" across a real logoff/logon, a second Windows user cannot read the state nor reach the pipe, `uninstall --delete-data`.

## 7. Documentation

`docs/install.md` (Windows: the zip, where files go, SmartScreen's "unknown publisher" until signing), `docs/desktop.md` (Windows tray, autostart, no notification click), `docs/status.md`, `docs/troubleshooting.md` (DACL and DPAPI errors), README; SPEC §3, §12 (time zone), §13 "Platforms"; decision 0018; decision 0015's "Windows is next" marked done; OPEN.md.

## 8. Rollout

Merged to main behind CI; the next release carries `pcb_windows_amd64.zip`. Windows is labelled "untested" in the docs and in `pcb doctor` until the live checklist (§6) passes on the VM, then "supported", within this project [call].

## 9. Next (separate projects)

1. The MSI (WiX): `pcb-ui` required, `pcb` optional (adds `pcb.exe` to the user's `PATH`), the Run-key autostart, per-user install (no admin rights) [author for the components; per-user is the agent's proposal, to settle then].
2. Authenticode signing: SignPath Foundation (free for open source, approval needed) or Azure Trusted Signing; a self-signed certificate is not an option.
3. Windows on arm64.

## 10. Left to the builder

- File layout of the Windows variants and how much of `listen_unix.go` / `listen_windows.go` is shared.
- How the SID of a named-pipe client is obtained (go-winio exposes the handle; `GetNamedPipeClientProcessId` + `OpenProcessToken`, or `ImpersonateNamedPipeClient`).
- The mingw source on the Windows runner and the tool for the `.syso` resource, pinned.
- Exact Windows wording of messages, within the existing error kinds and exit codes.
