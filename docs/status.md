# Status

What is built, how each part was tested, and which checks against a real Proton account have run. pcb is built through milestone M8.

**This is still an early version: do not point pcb at a Proton account you care about yet.** Use a test account.

## What pcb is made of

- **Core** (Go): accounts and login (as unattended as Proton allows), a sync engine on Proton's change feed, an encrypted local cache, an access policy, and an iCalendar-native event model ([decision 0012](../spec/decisions/0012-icalendar-native-model.md); JSCalendar deferred).
- **Adapters:** CalDAV (Thunderbird first; read and write), MCP for AI agents (`pcb mcp` over stdio, or `/mcp` with a bearer token; [decision 0016](../spec/decisions/0016-mcp-adapter.md)) and a CLI. A REST adapter is out of scope ([decision 0007](../spec/decisions/0007-scope-caldav-only.md)).
- **Runs** headless (systemd, Docker), one process for any number of Proton accounts. On a Linux or Windows desktop the same daemon runs inside `pcb-ui`, a tray icon and a window.
- **Platforms:** Linux amd64 and arm64 are supported; macOS gets `pcb` only and is untested; Windows 10 and 11 (amd64) are supported, `pcb-ui` and `pcb`: built and tested in CI on Windows and checked live on a Windows 11 desktop ([install](install.md#macos-and-windows)).

## How things are checked

- **Against a fake Proton:** every milestone is built and tested against a fake Proton (`internal/proton/protontest`, and `pcb-fake` for rehearsals).
- **Live:** the checks against the Proton test account are the steps of [HUMAN-CHECKS.md](../spec/HUMAN-CHECKS.md), run on a throwaway VM where noted. Their results are in the write-ups under [spec/experiments/](../spec/experiments/).
- **Audit:** the initial-version audit (2026-11-12, [spec/audits/2026-11-12-initial-version.md](../spec/audits/2026-11-12-initial-version.md)) is fixed or parked.

## Milestones

### M1: Proton core

| | |
|---|---|
| Built | `pcb login` (password, TOTP, CAPTCHA resume, opt-in unattended relogin), `pcb logout`, `pcb status`, `pcb calendar list` |
| Tested | against the fake Proton |
| Live | the M1 live session, 2026-11-13, on the M5 build; the fixes it called for are in. Write-up still to come. |

### M2: store and sync

| | |
|---|---|
| Built | `pcb sync`, `pcb event list`, the encrypted cache, sync on Proton's change feed |
| Tested | against the fake Proton |
| Live | the M2 live session (the change feed), 2026-11-13, on the M5 build; fixes in. Write-up still to come. |

### M3: CalDAV read

| | |
|---|---|
| Built | `pcb serve` and `pcb client`: CalDAV for Thunderbird, reads |
| Tested | against the fake Proton |
| Live | `spec/HUMAN-CHECKS.md` step 6 (Thunderbird), 2026-11-13, on the M5 build; fixes in. Write-up still to come. |

### M4: CalDAV write

| | |
|---|---|
| Built | writes from Thunderbird: create, edit, move, delete ([Thunderbird](thunderbird.md#writes-from-thunderbird)) |
| Tested | against the fake Proton |
| Live | step 6 with its 6.7 write matrix, 2026-11-13, on the M5 build; fixes in. Write-up still to come. |

### M5: daemon and operations

| | |
|---|---|
| Built | a control socket behind a live `pcb status` and `pcb doctor`; `pcb serve --wait-for-login`; `pcb install`, `uninstall` and `service` (systemd, user or system); a container image; CalDAV passwords you can show again (`pcb client show`) |
| Tested | against the fake Proton; both smoke tests (the image, and a Debian container with systemd as PID 1) passed locally; CI publishes the image from main |
| Live | step 7 (the daemon, the service and the container; 7.1 a fresh host, 7.2 the compose file, secret, config mount and a login in the container, Thunderbird not part of it), 2026-11-13; fixes in. [M5-live.md](../spec/experiments/M5-live.md) |

### M6: several accounts in one process

| | |
|---|---|
| Built | any number of Proton accounts in one process, one URL per account; the per-account state layout (`accounts/<id>/`); `--json` schema version 2 |
| Tested | against the fake Proton only, with two or more accounts: there is no second Proton test account |
| Live | 2026-11-14, on the test account: a real M5 state upgraded in place with no new login, its CalDAV password and session kept; Thunderbird with calendars on the old URL and on the per-account URL in one profile keeps one saved password per URL and asks for each after a password change. [M6-live.md](../spec/experiments/M6-live.md) |
| Note | no downgrade after M6 ([install](install.md#no-downgrade-after-the-multi-account-layout)) |
| Design | [SPEC](../spec/SPEC.md), [decision 0014](../spec/decisions/0014-several-accounts-per-process.md) |

### M7: the desktop app and the keyring sealer

| | |
|---|---|
| Built | `pcb-ui` ([desktop](desktop.md)), and a sealer that keeps the sealing key in the desktop keyring, Secret Service ([security](security.md#the-desktop-keyring-secret-service)) |
| Tested | against the fake Proton, a fake Secret Service and Fyne's headless test driver |
| Live | step 9, 2026-11-13, in a throwaway Debian 13 VM with GNOME 48 and the test account: the keyring sealer, the lock and its recovery without a login, Thunderbird by copy and paste, Quit, the second instance, the conflict dialog and autostart worked. On 2026-11-14, in the same VM against the fake Proton: a notification click with the window hidden (banner and message tray), the window without the tray extension, and `pcb uninstall --delete-data` removing the real keyring item. [M7-live.md](../spec/experiments/M7-live.md) |
| Fixed | its ten findings: a CAPTCHA'd add deleted by a click on pcb-ui's own notification, no login lines in the log, a silent fallback to the key file, `pcb uninstall --delete-data` unable to clean a desktop-only install, and smaller wording and display issues |
| Design | [decision 0015](../spec/decisions/0015-pcb-ui.md) |

### M8: MCP adapter for agents

| | |
|---|---|
| Built | `pcb mcp` over stdio, remote `/mcp` with a bearer token, grants per agent, and a login the agent starts but you finish (in `pcb-ui` or on a one-time page on 127.0.0.1) ([agents](agents.md)) |
| Tested | against the fake Proton through the MCP SDK's own client, with a scan for secrets over a whole session; `make rehearse` drives `pcb mcp` against `pcb-fake` |
| Live | 2026-11-14, with a real agent client (Claude Code 2.1.325) against the test account: stdio with a login on the loopback page, a create, update and delete, a session relayed to `pcb-ui`, remote `/mcp` with a token; remote `/mcp` over TLS trusting pcb's certificate through `NODE_EXTRA_CA_CERTS` alone (against the fake Proton). [M8-live.md](../spec/experiments/M8-live.md) |
| Fixed | its three defects, and a fourth the fixes found |
| Design | [decision 0016](../spec/decisions/0016-mcp-adapter.md), [SPEC §12](../spec/SPEC.md) |

### The Windows port

| | |
|---|---|
| Built | `pcb-ui.exe` and `pcb.exe` on Windows 10 and 11 (amd64): the tray, the window, "Start at login", the bridge inside `pcb-ui`, the CLI talking to it over a pipe only your Windows account can open, the sealing key protected by Windows (DPAPI), files only your account can open; a `pcb_windows_amd64.zip` in each release ([install](install.md#windows)) |
| Tested | on a Windows runner in CI and on a Windows 11 virtual machine as a standard user, against the fake Proton; a release runs both binaries from the zip before publishing |
| Live | the whole bridge on a Windows 11 virtual machine as a standard user, against the test account, on 2026-11-15: the zip, `pcb-ui` with its tray and notifications, an account added through it, CalDAV, `pcb status` through it, "Start at login" across a real restart, a second Windows user kept out of the files and of the bridge, `pcb uninstall --delete-data`. Thunderbird for Windows itself was not checked (skipped by the author; CalDAV was checked with `curl`); the author confirmed the uninstall ended the Proton session. |
| Design | [the Windows port](../spec/decisions/0018-windows-port.md) |

## Still to check live

- **Windows:** Thunderbird for Windows on the bridge (skipped in the live check; CalDAV itself was checked with `curl`).
- **Session lifetime:** a 14-day probe of one Proton session, running until 2026-11-26 (so far: the access token lasts 24 hours and both tokens change on every refresh).
- **Write-ups** of the 2026-11-13 sessions other than step 7 (M1, M2, step 6).
