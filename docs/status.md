# Status

What is built, how each part was tested, and which checks against a real Proton account have run. Everything described in these docs is built; the sections below say how each part was checked.

**This is still an early version.** On Linux its author uses it with their own Proton account, and every feature works there; the scripted live checks below run on a test account.

## What pcb is made of

- **Core** (Go): accounts and login (as unattended as Proton allows), a sync engine on Proton's change feed, an encrypted local cache, an access policy, and an iCalendar-native event model (JSCalendar is deferred; [why](../spec/decisions/0012-icalendar-native-model.md)).
- **Adapters:** CalDAV (Thunderbird first; read and write), MCP for AI agents (`pcb mcp` over stdio, or `/mcp` with a bearer token; [the decision](../spec/decisions/0016-mcp-adapter.md)) and a CLI. A REST adapter is out of scope ([why](../spec/decisions/0007-scope-caldav-only.md)).
- **Runs** headless (systemd, Docker), one process for any number of Proton accounts. On a Linux or Windows desktop the same daemon runs inside `pcb-ui`, a tray icon and a window.
- **Platforms:** Linux amd64 and arm64 are supported; macOS gets `pcb` only and is untested; Windows 10 and 11 (amd64) are supported, `pcb-ui` and `pcb`: built and tested in CI on Windows and checked live on a Windows 11 desktop from the zip; an installer, also checked live there, ships in each release since v0.2.0 ([install](install.md#macos-and-windows)).

## How things are checked

- **Against a fake Proton:** every feature is built and tested against a fake Proton (`internal/proton/protontest`, and `pcb-fake` for rehearsals).
- **Live:** the checks against the Proton test account are the steps of [HUMAN-CHECKS.md](../spec/HUMAN-CHECKS.md), run on a throwaway VM where noted. Their results are in the write-ups under [spec/experiments/](../spec/experiments/).
- **Audit:** the initial-version audit (2026-11-12, [spec/audits/2026-11-12-initial-version.md](../spec/audits/2026-11-12-initial-version.md)) is fixed or parked.

## Parts, and how each was checked

### Login

| | |
|---|---|
| Built | `pcb login` (password, TOTP, CAPTCHA resume, opt-in unattended relogin), `pcb logout`, `pcb status`, `pcb calendar list` |
| Tested | against the fake Proton |
| Live | the login live session, 2026-11-13, on the build of that day; the fixes it called for are in. Write-up still to come. |

### Sync and the local cache

| | |
|---|---|
| Built | `pcb sync`, `pcb event list`, the encrypted cache, sync on Proton's change feed |
| Tested | against the fake Proton |
| Live | the change-feed live session, 2026-11-13, on the build of that day; fixes in. Write-up still to come. |

### CalDAV, reading

| | |
|---|---|
| Built | `pcb serve` and `pcb client`: CalDAV for Thunderbird, reads |
| Tested | against the fake Proton |
| Live | Thunderbird live session ([HUMAN-CHECKS.md](../spec/HUMAN-CHECKS.md)), 2026-11-13, on the build of that day; fixes in. Write-up still to come. |

### CalDAV, writing

| | |
|---|---|
| Built | writes from Thunderbird: create, edit, move, delete ([Thunderbird](thunderbird.md#writes-from-thunderbird)) |
| Tested | against the fake Proton |
| Live | the same session's write matrix (create, edit, move, delete), 2026-11-13; fixes in. Write-up still to come. |

### The daemon and running it

| | |
|---|---|
| Built | a control socket behind a live `pcb status` and `pcb doctor`; `pcb serve --wait-for-login`; `pcb install`, `uninstall` and `service` (systemd, user or system); a container image; CalDAV passwords you can show again (`pcb client show`) |
| Tested | against the fake Proton; both smoke tests (the image, and a Debian container with systemd as PID 1) passed locally; CI publishes the image from main |
| Live | 2026-11-13: the daemon, the service and the container (a fresh host; the compose file, secret and config mount; a login in the container; Thunderbird not part of it); fixes in. [Write-up](../spec/experiments/M5-live.md) |

### Several accounts in one process

| | |
|---|---|
| Built | any number of Proton accounts in one process, one URL per account; the per-account state layout (`accounts/<id>/`); `--json` schema version 2 |
| Tested | against the fake Proton only, with two or more accounts: there is no second Proton test account |
| Live | 2026-11-14, on the test account: a state from before several accounts upgraded in place with no new login, its CalDAV password and session kept; Thunderbird with calendars on the old URL and on the per-account URL in one profile keeps one saved password per URL and asks for each after a password change. [Write-up](../spec/experiments/M6-live.md) |
| Note | no downgrade after this layout change ([install](install.md#no-downgrade-after-the-multi-account-layout)) |
| Design | [SPEC](../spec/SPEC.md), [the decision](../spec/decisions/0014-several-accounts-per-process.md) |

### The desktop app and the keyring

| | |
|---|---|
| Built | `pcb-ui` ([desktop](desktop.md)), and a sealer that keeps the sealing key in the desktop keyring, Secret Service ([security](security.md#the-desktop-keyring-secret-service)) |
| Tested | against the fake Proton, a fake Secret Service and Fyne's headless test driver |
| Live | 2026-11-13, in a throwaway Debian 13 VM with GNOME 48 and the test account: the keyring sealer, the lock and its recovery without a login, Thunderbird by copy and paste, Quit, the second instance, the conflict dialog and autostart worked. On 2026-11-14, in the same VM against the fake Proton: a notification click with the window hidden (banner and message tray), the window without the tray extension, and `pcb uninstall --delete-data` removing the real keyring item. [Write-up](../spec/experiments/M7-live.md) |
| Fixed | its ten findings: a CAPTCHA'd add deleted by a click on pcb-ui's own notification, no login lines in the log, a silent fallback to the key file, `pcb uninstall --delete-data` unable to clean a desktop-only install, and smaller wording and display issues |
| Design | [the decision](../spec/decisions/0015-pcb-ui.md) |

### Agents (MCP)

| | |
|---|---|
| Built | `pcb mcp` over stdio, remote `/mcp` with a bearer token, grants per agent, and a login the agent starts but you finish (in `pcb-ui` or on a one-time page on 127.0.0.1) ([agents](agents.md)) |
| Tested | against the fake Proton through the MCP SDK's own client, with a scan for secrets over a whole session; `make rehearse` drives `pcb mcp` against `pcb-fake` |
| Live | 2026-11-13 and 14, with a real agent client (Claude Code 2.1.325) against the test account: stdio with a login on the loopback page, a create, update and delete, a session relayed to `pcb-ui`, remote `/mcp` with a token; remote `/mcp` over TLS trusting pcb's certificate through `NODE_EXTRA_CA_CERTS` alone (against the fake Proton). [Write-up](../spec/experiments/M8-live.md) |
| Fixed | its three defects, and a fourth the fixes found |
| Design | [the decision](../spec/decisions/0016-mcp-adapter.md), [the MCP section of the SPEC](../spec/SPEC.md) |

### Windows

| | |
|---|---|
| Built | `pcb-ui.exe` and `pcb.exe` on Windows 10 and 11 (amd64): the tray, the window, "Start at login", the bridge inside `pcb-ui`, the CLI talking to it over a pipe only your Windows account can open, the sealing key protected by Windows (DPAPI), files only your account can open; a `pcb_windows_amd64.zip` in each release ([install](install.md#windows)) |
| Tested | on a Windows runner in CI and on a Windows 11 virtual machine as a standard user, against the fake Proton; a release runs both binaries from the zip before publishing |
| Live | the whole bridge on a Windows 11 virtual machine as a standard user, against the test account, on 2026-11-15: the zip, `pcb-ui` with its tray and notifications, an account added through it, CalDAV, `pcb status` through it, "Start at login" across a real restart, a second Windows user kept out of the files and of the bridge, `pcb uninstall --delete-data`. Thunderbird for Windows itself was not checked (skipped by the author; CalDAV was checked with `curl`); the author confirmed the uninstall ended the Proton session. |
| Design | [the decision](../spec/decisions/0018-windows-port.md) |

### The Windows installer

| | |
|---|---|
| Built | `pcb_windows_amd64.msi`, in each release from v0.2.0: a per-user install with no administrator prompt, `pcb-ui` (required) and `pcb` on your `PATH` (optional), a Start-menu shortcut that names `pcb-ui` to Windows, "Start at login" ticked by default, an uninstall that keeps your accounts ([install](install.md#the-installer)) |
| Tested | `scripts/win/msi-smoke.ps1` on a Windows 11 virtual machine as a standard user: a silent install, the files, the shortcut and its app ID, "Start at login", the `PATH` entry seen by a new process, repairs that leave "Start at login" off, `pcb` removed and added back (also through an upgrade), upgrades, the installer over a zip in its folder, the uninstall keeping the data; CI runs the same script on every pull request, and a release on its own installer before publishing |
| Live | 2026-11-15, on a Windows 11 virtual machine as a standard user, no Proton login: the double-click install and its screens with no administrator prompt, SmartScreen's prompt, the shortcut and a notification showing `pcb-ui`'s name, a repair, "Start at login" off from the command line and on across a restart, upgrades with the screens and silent, the uninstall from Settings and with `msiexec /x`, the installer over a zip in its folder; one fix in (that zip's `pcb-ui.exe` of the same version was kept), and a repair or a silent upgrade leaves `pcb-ui` closed (documented). SmartScreen on a real browser download is for the author to confirm. [Write-up](../spec/experiments/W2-windows-msi-live.md) |
| Design | [the decision](../spec/decisions/0020-windows-msi.md) |

### Two-password accounts

| | |
|---|---|
| Built | Proton's two-password mode: the mailbox password is asked after the 2FA code (`pcb login`, `pcb login resume --mailbox-password`, the `pcb-ui` wizard, an agent's login), kept only in memory, and an unattended relogin reuses the sealed key passphrase ([commands](commands.md), [desktop](desktop.md)) |
| Tested | against the fake Proton, with a two-password account in `make rehearse` |
| Live | not yet: see below |
| Design | [the decision](../spec/decisions/0021-two-password-mode.md) |

### Update check

| | |
|---|---|
| Built | `pcb-ui` and `pcb doctor` tell you once a day that a newer stable release exists; `pcb version --check`, `pcb update-check on\|off\|status`; pcb never downloads or installs anything ([security](security.md#the-update-check)) |
| Tested | against a local stand-in for GitHub's redirect |
| Design | [the decision](../spec/decisions/0019-update-check.md) |

## What changed after the first release, where you would notice

Fixes from a review of the code, merged after v0.3.0:

- A panic in an account's loop parks that account instead of killing the daemon; a command that panics exits 20 with the stack on stderr.
- Proton's `Retry-After` is honoured: a 429 on login or 2FA is remembered, and later logins are refused with the time left (pcb refuses, it does not wait); a 503's `Retry-After` is honoured for reads, up to 60 s; other 429s wait up to 60 s, at most 3 attempts.
- Every MCP tool error has the same shape.
- `pcb doctor` catches a CalDAV port that is already taken and a control-socket path that is too long.
- `config.toml` refuses port 0, and every unknown key is reported, not only the first. An existing `[caldav] listen` with port 0 must be edited to a fixed port, or pcb will not start.
- An unknown subcommand names the next step.
- Without a client that can ask you (form elicitation), `event_delete` and sign-out are refused unless the token (or `pcb mcp --allow`) includes `unconfirmed-delete`. Tokens created before this have to be re-created to get it ([agents](agents.md)).
- A race that could reload an account's keys during logout is fixed.
- Release archives are built reproducibly for a given commit from a pinned Go toolchain; from the next release, the release also attests an SBOM of the Go modules besides the provenance. `install.sh` takes a download base over https (or a local `file://` copy). A weekly vulnerability scan (`govulncheck`) runs in CI ([development](development.md#reproducible-builds-and-what-is-pinned)). It uses the Go toolchain named in `go.mod`, so an advisory against the standard library can fail the scan until that line is bumped.

## Still to check live

- **Windows:** Thunderbird for Windows on the bridge (skipped in the live check; CalDAV itself was checked with `curl`); the installer's SmartScreen prompt on a real browser download ([above](#the-windows-installer)).
- **Session lifetime:** a 14-day probe of one Proton session, running until 2026-11-26 (so far: the access token lasts 24 hours and both tokens change on every refresh).
- **Two-password accounts:** the live check in [HUMAN-CHECKS.md](../spec/HUMAN-CHECKS.md) has not run; its write-up will be added under `spec/experiments/`.
- **Write-ups** of the 2026-11-13 sessions other than the daemon one (login, sync, Thunderbird).
