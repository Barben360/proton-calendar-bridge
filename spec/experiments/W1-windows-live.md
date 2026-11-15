# W1-live — Both binaries on a Windows 11 VM, as a standard user

Status: **HUMAN-CHECKS step 12, run in part on 2026-11-14/15 (UTC); stopped before the login (12.4).** Every line that needs no account ran: 12.1, 12.2, 12.3, 12.7, 12.9, and 12.8, 12.10 and 12.11 without an account. The login could not run: the workstation's password manager CLI (`pass-cli`) had no session, and logging it in needs the author. 12.4, 12.5, 12.6, 12.12 and the account parts of 12.8, 12.10 and 12.11 wait for it (last section). Three findings, all fixed (table at the end); Windows stays "runs, untested" until the rest passes.

## Setup

- The author's Windows 11 VM (`<VM>`, Windows 11 build 26300, en-US, time zone Romance Standard Time, no 3D acceleration). The standard user under test (`<user>`, not elevated: `whoami /groups` shows Medium Mandatory Level, no Administrators) signs in at the console with its password at boot. An administrator account was used only for Mesa, the second user and the reboot. Both reached over SSH by key; `cmd.exe` is the remote shell.
- Contexts: **ssh** (key sign-in of `<user>`), **console** (an interactive scheduled task of `<user>` in its console session: task XML with `InteractiveToken` and no battery conditions, PowerShell run as `conhost.exe --headless powershell.exe …`), **admin** (the administrator over SSH). Screenshots (`Graphics.CopyFromScreen`) were copied to the workstation and stay there.
- Build under test: branch `win-t16`, first `v0.1.0-53-g5cb9a93`, then, after the fixes, `v0.1.0-56-gfa1e8e2`. `pcb.exe` cross-compiled (`GOOS=windows make build`), `pcb-ui.exe` built on the VM (MSYS2 UCRT64, `make ui`), the zip made by `scripts/release/package.sh windows amd64` (zip SHA-256 `304250c0…162023`, then `99589264…071e3a`), copied to `Downloads`, its `Get-FileHash` equal to the workstation's, unzipped with `Expand-Archive` into `%LOCALAPPDATA%\Programs\pcb`.
- Windows Update restarted the VM on its own at 23:35Z and 23:38Z, before the run (a servicing restart, then TrustedInstaller); the first sign-in after it showed the "almost done setting up your PC" screen, dismissed with "Remind me in 3 days".
- **Logins: 0.** The login of 12.4 did not run (see the status line).

## 12.1 The zip: PASS

- 23:45Z, ssh: the zip holds exactly `LICENSE`, `README.md`, `pcb-ui.exe`, `pcb.exe`. `pcb.exe version`: `pcb v0.1.0-53-g5cb9a93 (commit 5cb9a93, …)`, exit 0. `pcb-ui.exe --version` redirected to a file (`Start-Process -RedirectStandardOutput`): the file holds `pcb-ui v0.1.0-53-g5cb9a93 (commit 5cb9a93, built …)`, exit 0.
- 23:45Z, console: `cmd /k` in a visible window (Windows Terminal), `pcb-ui.exe --version` with no redirect: the line printed in that window, the prompt back on a new line (screenshot).

## 12.2 The display: PASS after the VM's workaround (F1, a docs defect)

- 23:47Z, console: `pcb-ui.exe` showed the message box `pcb-ui cannot start` / `pcb-ui needs OpenGL 2.0, which this display does not offer (a VM without 3D acceleration, or a remote desktop session): APIUnavailable: WGL: The driver does not appear to support OpenGL`, the hint naming `docs/troubleshooting.md` and Mesa; OK: exit 10; neither `%APPDATA%\pcb` nor `%LOCALAPPDATA%\pcb` was created.
- The documented workaround, with `mesa-dist-win` 26.2.6 (GitHub release of 2026-11-03): its `release-msvc` archive has **no** `opengl32.dll`; the `release-mingw` archive (SHA-256 `da0f71d9181b672c1918f3c342f11118c687f6059a46b16502845160733cc388`, equal to the digest GitHub shows) has `x64/opengl32.dll` and `x64/libgallium_wgl.dll`, copied beside `pcb-ui.exe`.
- With them, `pcb-ui` passed its OpenGL check, started the bridge (`sealer chosen`, `control socket started`, `listener started` in `pcb-ui.log`), then ended about a second later with exit `0x80070057` (an HRESULT, E_INVALIDARG), no message, no crash report, no `daemon stopped` line, four times. `pcb-ui --background` stayed up until its window was shown (a second `pcb-ui.exe`), then ended the same way. Mesa had picked its Direct3D 12 driver: with `GALLIUM_DRIVER=llvmpipe` (23:52Z) the window opened and worked for the rest of the run. Set for the user with `setx GALLIUM_DRIVER llvmpipe` (inherited by the console tasks and by Start at login). **F1**: `docs/troubleshooting.md` named neither the archive nor the variable.

## 12.3 The first start: PASS (F2)

- 23:46Z, ssh, no state: `pcb.exe doctor` exit 0: `! platform: windows/amd64: runs, untested`, `– config dir … does not exist yet`, `– state dir … does not exist yet`, `✓ config file: defaults, no file`, `✓ sealer: not chosen yet: the first login picks it ([secrets] backend = auto)`, `– dpapi key: no key yet: the first login creates it`, `✓ caldav listener: 127.0.0.1:1080, plain HTTP (loopback only)`, `– tls certificate: …`, `! accounts: no account …` with its fix, `✓ elevated: not elevated`, `✓ time zone: Romance Standard Time → Europe/Paris`, `– start at login: Start at login is off`, `– service: no service on Windows: pcb-ui's Start at login runs it`, `– daemon: not running: offline checks only`. Nothing created.
- 23:52Z, console: the window (Accounts empty, `Add an account to start.`, the alert `not logged in to Proton — run pcb login`, `Listening on 127.0.0.1:1080`). The tray icon sits under the arrow of hidden icons, with its attention mark.
- `layout.json` is `{"version":2,"sealer":"dpapi"}` from that first start (`sealer chosen sealer=dpapi backend=auto`); no `secrets.key.dpapi` before a login.
- `icacls`: `%LOCALAPPDATA%\pcb` and `…\logs`: `<user>:(OI)(CI)(F)` and `SYSTEM:(OI)(CI)(F)` only, protected (no inherited entry), owner `<user>`; `layout.json` and `pcb-ui.log` inherit those two only.
- The toast: 23:51Z, a Windows toast titled `pcb`, under the header `io.github.barben360.pcb-ui` (the raw app ID: a zip install registers no display name), body `not logged in to Proton\nrun pcb login` with a **literal** `\n` (**F2**); `notification sent alert=needs-login` in `pcb-ui.log`. After the fix, 00:09Z: body `not logged in to Proton — run pcb login` (screenshot).

## 12.7 Tray and menu: PASS without an account (the account's line waits for 12.4)

- 23:54Z–00:11Z, console: right-click on the icon: `Open pcb`, `Quit` (no account line: none yet). A left click shows the same menu, not the window. The window's X hides it: `pcb.exe status` (ssh) still shows the daemon of kind `ui`. `Open pcb` shows it again.
- 00:08:53Z: `Quit`: `pcb-ui` exited 0 within a second (`listener stopped`, `control socket stopped`, `daemon stopped stop=0s`); `pcb.exe status`: `daemon: not running (offline view)`, exit 0; `pcb.sock` absent.

## 12.8 Start at login across a real end of session: PASS without an account (the account part waits for 12.4)

- 00:11Z, console: Settings, `Start at login` on. ssh: `reg query …\Run /v pcb-ui`: `"<install folder>\pcb-ui.exe" --background`; `pcb.exe doctor`: `✓ start at login: starts <install folder>\pcb-ui.exe`.
- 00:12:06Z, admin: `shutdown /r /t 0`. `pcb-ui.log`: `listener stopped`, `control socket stopped`, `daemon stopped stop=0s` at 00:12:05Z (a clean stop at the end of session; with no account there was nothing to drain). Signed in again at the console at 00:12Z; `pcb-ui` started at 00:12:45Z: its command line `"<install folder>\pcb-ui.exe" --background`, parent `explorer`, session 1, no window (screenshot), the tray icon; `pcb.exe status` (ssh): the daemon of kind `ui`, listener up, the toast sent again (`notification sent alert=needs-login`).

## 12.9 Explorer and config.toml: PASS after F3

- 23:56Z, console: Settings, `Open logs folder`: Explorer on `%LOCALAPPDATA%\pcb\logs` (screenshot).
- 23:56Z: `Open config.toml` with no `config.toml` (the usual case): a dialog `Something went wrong` / `The system cannot find the file specified.` (**F3**). With an empty file made by hand: Windows' `Select an app to open this .toml file` (Notepad, Windows Media Player Legacy, …): no `.toml` association on a fresh Windows; Notepad, Just once, opened it.
- After the fix, 00:10Z, with no `%APPDATA%\pcb`: `Open config.toml` made `%APPDATA%\pcb` (protected: `<user>` and `SYSTEM` only) and an empty `config.toml` (inherited, the same two), then Windows asked for the app as above; Notepad opened it.

## 12.10 A second Windows user is kept out: PASS without an account (to repeat with the secrets, after 12.4)

- 00:03Z: a temporary local user, not in Administrators (checked from inside the check: `admin=False`). Two ways to run as it failed for reasons of Windows, not pcb: a scheduled task never started (standard users have no right to log on as a batch job: `SeBatchLogonRight` is Administrators, Backup Operators and Performance Log Users only), and `Start-Process -Credential` from the administrator's SSH session ended `0xC0000142` (no access to that session's window station). It ran from `<user>`'s console session as `runas` does (`Start-Process -Credential`), the password handed over in a file only `<user>` could read, deleted by the task as it read it.
- As that user, with `pcb-ui` running and `PCB_STATE_DIR` set to `<user>`'s `%LOCALAPPDATA%\pcb`: `pcb.exe status` exit 20, `cannot reach the pcb daemon on …\pcb.sock: open <state>: Access is denied.`, hint `check that you run pcb as the user that runs pcb serve`; `pcb.exe doctor` exit 10 (`state dir: … Access is denied`, `accounts: open …layout.json: Access is denied`, `daemon: … Access is denied`). Listing the state directory and `logs`, reading `layout.json` and `pcb-ui.log`: `Access is denied`. Opening the pipe `\\.\pipe\pcb-<hash>` (the name from `pcb-ui.log`): `Access to the path is denied`.
- Observation: for the files that did not exist yet (`state.json`, `secrets.sealed`, `secrets.key.dpapi`) that user got "Could not find file", so it can tell whether a name exists in the directory (Windows lets everyone traverse a directory to a named child), not read it.
- Then the user, its profile and the folder were deleted (checked: none left).

## 12.11 `pcb doctor`: PASS without an account (the `dpapi key` line waits for 12.4)

- 00:10Z, with `pcb-ui` running, console and ssh give the same lines, exit 0: `! platform: windows/amd64: runs, untested`, `✓ config dir … private`, `✓ state dir … private`, `✓ config file`, `✓ sealer: dpapi`, `– dpapi key: no key yet: the first login creates it`, `✓ caldav listener: 127.0.0.1:1080, plain HTTP (the running daemon; URL http://localhost:1080)`, `– tls certificate`, `! accounts: no account …`, `✓ elevated: not elevated`, `✓ time zone: Romance Standard Time → Europe/Paris`, `– start at login: Start at login is off` (`✓ … starts <install folder>\pcb-ui.exe` once on), `– service: no service on Windows: pcb-ui's Start at login runs it`, `✓ daemon: running … kind ui`, `! alert needs-login …`. Beyond `platform`, the warnings are the missing account's.

## Not run yet (they need the login)

| Line | What remains |
|---|---|
| 12.4 | the one login, through the running `pcb-ui`, from the workstation's password manager over SSH's stdin; then `secrets.key.dpapi`, `layout.json`, the login lines of `pcb-ui.log`, no address in it |
| 12.5 | `pcb status` (ssh and console) with the account, `pcb client show` |
| 12.6 | Thunderbird for Windows (or `curl.exe` PROPFIND/REPORT and Thunderbird left to the author) |
| 12.7 | the account's line in the tray menu |
| 12.8 | a second reboot with the account: `last_login` unchanged, the drain with an account to stop |
| 12.10 | the same check with `secrets.sealed`, `secrets.key.dpapi` and `state.json` present |
| 12.11 | `pcb doctor` with the account (the `dpapi key` line ok; over ssh, what a key sign-in reports for it) |
| 12.12 | the refusal while `pcb-ui` serves, `uninstall --delete-data --plan` and `--yes`, the web app's session list (author) |

## Findings and their fixes

| # | Finding | Fix | Commit (subject) |
|---|---|---|---|
| F1 | The OpenGL workaround in `docs/troubleshooting.md` did not work as written: the `msvc` archive of mesa-dist-win 26.2.6 has no `opengl32.dll`, and with the `mingw` archive's DLLs Mesa chose its Direct3D 12 driver and `pcb-ui` ended silently (exit `0x80070057`) when its window showed. A defect of the VM's setup and of the docs, not of pcb | the page names the `mingw` archive's two DLLs and `setx GALLIUM_DRIVER llvmpipe`, and what happens without it | "Say which Mesa files pcb-ui needs, and to make Mesa use llvmpipe" |
| F2 | A toast showed `\n` literally: Fyne writes the title and body, quoted by Go's `%q`, into a PowerShell double-quoted string, where Go's escapes are text, and where a `"` (or `“ ” „`), a `$` or a backtick would end or expand the string | `Notify` passes both through `toastText`: a line break becomes ` — `, quotes and backticks `'`, `$` and `\` their fullwidth forms, anything `%q` escapes a space (`TestToastText`, `TestToastTextIsInertInFynesTemplate`) | "Keep line breaks and quotes from breaking a Windows toast" |
| F3 | `Open config.toml` with no `config.toml` (the usual case) failed: `The system cannot find the file specified.` | it makes an empty `config.toml` in a private config directory first, and leaves an existing one alone (`TestSettingsOpenConfigMakesAMissingFile`, `TestSettingsOpenConfigFailsWithoutOpening`); `docs/desktop.md` says so, and that Windows asks for an app for `.toml` | "Make config.toml before Open config.toml opens it" |
| — | HUMAN-CHECKS step 12's method (battery conditions of the tasks, the headless console, Mesa's variable, the second user's sign-in, the tray's left click) | corrected | "Correct HUMAN-CHECKS step 12 after its first run" |

Observations, not defects: the toast's header is the raw app ID `io.github.barben360.pcb-ui` (no Start-menu shortcut gives it a name; docs/desktop.md already calls zip toasts best effort); the OpenGL message box's hint names `docs/troubleshooting.md`, which the zip does not hold (only `README.md`); a second user can tell whether a name exists in the state directory (12.10); the second user's `pcb status` hint names `pcb serve` where the server is `pcb-ui`.
