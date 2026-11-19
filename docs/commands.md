# Commands

Every `pcb` command, how to pick the account a command acts on, the `--json` output and the exit codes. Scripts and agents can rely on the last two: they never change.

## The commands

### Log in and out

| Command | What it does |
|---|---|
| `pcb login` | Prompts for what is missing; or set `PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP`, `PCB_MAILBOX_PASSWORD`. A two-password account is asked for its mailbox password after the 2FA code (no echo, 3 tries). The username decides: a known address is a relogin, a new one adds an account. |
| `pcb login --username alice` | The username may be a flag (default: `$PCB_USERNAME`, else asked). |
| `pcb login --password-fd 3 3<pwfile` | Reads the password from a file descriptor: the password never goes in a flag. On Windows only `0`, `1` or `2` (the standard handles; another number is refused): `cmd /c "pcb login --password-fd 0 < pwfile"` hands the file's bytes as they are. Not `Get-Content pwfile \| pcb …` in Windows PowerShell 5.1: its pipe re-encodes as ASCII (`$OutputEncoding`) and mangles a non-ASCII password (PowerShell 7 sends UTF-8). |
| `pcb login --mailbox-password-fd 4 4<mbfile` | Reads the mailbox password of a two-password account (the second password, which decrypts your data) from a file descriptor, ahead of the prompt. Wins over `PCB_MAILBOX_PASSWORD`; not the same descriptor as `--password-fd` (exit 2). An account in single-password mode ignores it. On Windows a non-interactive login pipes the login password on descriptor `0` and gives the mailbox password through `PCB_MAILBOX_PASSWORD`. |
| `pcb login --unattended-relogin` | Stores the credentials so pcb can log in again without you. |
| `pcb login --non-interactive` | Never prompts: exits 10 naming what is missing. `--non-interactive` is accepted by every command. |
| `pcb login resume --captcha-token T` | Finishes a login after you solved the CAPTCHA Proton asked for. |
| `PCB_TOTP=<code> pcb login resume --totp` | Finishes a login that waits in the daemon for its 2FA code (from `$PCB_TOTP`, else asked; never an argument). |
| `PCB_MAILBOX_PASSWORD=<password> pcb login resume --mailbox-password` | Finishes a login that waits in the daemon for its mailbox password (from `--mailbox-password-fd`, else `$PCB_MAILBOX_PASSWORD`, else asked; never an argument). A wrong password keeps the login waiting, with no new Proton login; after 3 wrong ones the login is cancelled. |
| `pcb logout [--yes]` | Revokes the session, through the daemon when it runs; `--yes` discards a store this pcb cannot read. |

Details: [security](security.md#log-in).

### Accounts

| Command | What it does |
|---|---|
| `pcb account list` | Every account: local ID, address, phase, CalDAV on or off, MCP off, read-only or read-write, last sync. A pending add, stopped by a CAPTCHA, is listed too. |
| `pcb account caldav on\|off` | The account's CalDAV switch. Off, its CalDAV clients are refused; its sync pauses only when MCP is off too. |
| `pcb account mcp off\|read-only\|read-write` | What agents may do with the account, whatever their profile or token. Off, they do not see it, and its sync pauses only when CalDAV is off too. Read-only, they read it and every change is refused. Read-write, they do what their profile or token allows (the default; `pcb mcp`'s default profile allows everything). `on` means read-write. |
| `pcb account restart` | Serves an account again after it stopped on an error (`pcb status` shows it as `account-stopped`). |
| `pcb account remove [--yes]` | Signs the account out, then deletes its directory (its cache and CalDAV passwords too). |

### Status and checks

| Command | What it does |
|---|---|
| `pcb status` | Every account's login; with pcb serving, its live state too (listener, per-calendar sync and write gate, alerts). |
| `pcb status --daemon` | Exits 20 when no daemon answers (the container's health check). |
| `pcb doctor` | Every check (service, daemon, listener, certificate, clients included), with the fix. With no daemon running, the `caldav listener` line tries to bind the port and fails (exit 20) when another program holds it; `control socket path` fails when the state directory makes the socket path too long (107 bytes on Linux; set `PCB_STATE_DIR`). On Windows also the access lists, `elevated`, `dpapi key`, `time zone` and `start at login`; its `service` line says `no service on Windows: pcb-ui's Start at login runs it`. Exits 0 unless a check fails, 10 when you must act, 20 when only the daemon is unreachable. Its last line, `update`, warns when a newer stable release exists (the fix: its page and how to update), and is skipped when switched off, on a dev build or when github.com does not answer: it never changes the exit code. |

### Calendars and events

| Command | What it does |
|---|---|
| `pcb calendar list [--json]` | The calendars; read-only ones show `read_only`. |
| `pcb sync [--full]` | Fills or refreshes the local event cache from Proton. Not needed while `pcb serve` runs: it syncs by itself, so `pcb sync` exits 0 and says so. |
| `pcb sync --watch --interval 30s` | Keeps the cache current until Ctrl-C (at least 10s; not with `--json`). |
| `pcb event list --calendar NAME [--from 2026-11-01] [--to 2026-12-01]` | Cached events, no network call. |

### CalDAV clients

| Command | What it does |
|---|---|
| `pcb client show [NAME] [--reveal]` | URL, username, password and TLS fingerprint of a client. `NAME` defaults to `default`, else the only client (exit 10 when there are several and none is `default`); `--reveal` puts the password in `--json`. |
| `pcb client add NAME` | Another CalDAV app password, one per device. Needs an account (exit 10 `no account` before the first `pcb login`), not a signed-in one: it works while the account is signed out. |
| `pcb client rotate NAME` | A new password; the old one stops working at once. |
| `pcb client list` | The clients, never their passwords. |
| `pcb client remove NAME\|ID` | Its password stops working at once, even for a running `pcb serve`. |

Details: [Thunderbird](thunderbird.md#caldav-app-passwords).

### Serve and run as a service

| Command | What it does |
|---|---|
| `pcb serve [--listen HOST:PORT] [--wait-for-login] [--log-level LEVEL]` | Sync loop, CalDAV listener and control socket until Ctrl-C ([Thunderbird](thunderbird.md#what-pcb-serve-does)). |
| `pcb install [--system] [--plan]` | Runs `pcb serve` as a systemd service ([service](service.md)). |
| `pcb uninstall [--delete-data] [--plan]` | Removes the service, and with `--delete-data` the data ([service](service.md#uninstall)). |
| `pcb service start\|stop\|restart\|status\|logs [-f]` | Manages the installed service. |

On Windows there is no service: `pcb install` and every `pcb service` command exit 20 with `no service on Windows`, hint `use pcb-ui; "Start at login" runs it at each login`. `pcb serve` runs in a terminal (Ctrl-C stops it). `pcb uninstall` stops nothing and refuses (exit 20) while `pcb-ui`, `pcb serve` or an agent session (`pcb mcp`) serves the state; it turns `pcb-ui`'s "Start at login" off (deletes its Run value) when that value starts the `pcb-ui.exe` beside this `pcb.exe` on the default directories, and keeps it otherwise, saying why. With `--delete-data` it also logs out and deletes the config and state directories, the sealing key included ([install](install.md#remove-it-from-windows)).

### Agents

| Command | What it does |
|---|---|
| `pcb mcp [--profile full\|read-only] [--allow CAPS] [--account A]` | MCP for the agent that runs it, on stdin and stdout; `full`, the default, allows everything, `read-only` reading and starting a login ([agents](agents.md)). |
| `pcb mcp token add NAME --allow read` | A remote agent's bearer token, printed once. `--allow` takes `freebusy`, `read`, `write`, `delete`, `unconfirmed-delete` (deletes without asking you, for a client that cannot ask). |
| `pcb mcp token list`, `pcb mcp token remove NAME` | List the tokens (never their values), remove one. |

### Other

| Command | What it does |
|---|---|
| `pcb version` | The build's version. No network call. |
| `pcb version --check` | Also asks github.com whether a newer stable release exists: `update available: vX.Y.Z <release page>` with how to update this install (the `install.sh` line on Linux and macOS, pulling the image in the container), or `up to date`, `check skipped: dev build`, `check failed: …`. Exits 0 in every case; with `--json`, `data.update` holds `latest`, `url`, `available`, `hint`, `error`. |
| `pcb update-check on\|off\|status` | The daily update check of `pcb doctor` and `pcb-ui` (kept in `update.json` in the state directory). `PCB_UPDATE_CHECK=off` turns it off for one process. ([security](security.md#the-update-check)) |
| `pcb crash-reports status\|show\|send\|discard\|always\|ask\|never` | The crash reports pcb kept. `status` (which `always`, `ask`, `never` and `discard` also end with) gives the mode, the `PCB_CRASH_REPORTS` override, how many wait and `build_has_address`; `show` prints them as they would be sent (with `--json`, the events themselves); `send` sends them now whatever the mode (exit 40 when some are left, for example past the daily limit or while the server asks pcb to wait); `discard` deletes them; `always`, `ask` and `never` set the mode (`never` also deletes those waiting). A build without a crash report address (built from source) keeps none; `send` with reports waiting exits 20 saying so, and with none says `no crash report waiting` (exit 0). ([security](security.md#crash-reports)) |

## Choose the account

With several accounts, a command about one account names it: `--account ADDRESS|LOCAL-ID`, or the environment variable `PCB_ACCOUNT`.

This applies to `logout`, `login resume`, `status`, `calendar list`, `event list`, `sync`, `client ...` and `account caldav|mcp|restart|remove`. `pcb login` takes no `--account`: the username chooses.

| Accounts | No account named | A selector that names no account |
|---|---|---|
| none yet (or only a pending add) | exit 10, `no account`, hint `add one with pcb login` | exit 2 |
| one (a pending add does not count) | that account | exit 2 |
| several | exit 2: `several accounts: name one with --account`, and the list (`--json`: `data.accounts`) | exit 2 |

## JSON output

Every command has `--json`. It prints one envelope on stdout and nothing else, with one exception: `--help` and `pcb help` always print the usage text (exit 0), because an envelope around help text adds nothing for a script or an agent.

```json
{"schema_version": 2, "command": "...", "ok": true, "data": {}}
```

- On failure `ok` is false and `error` is set. An envelope can carry `data` and `error` together: a CAPTCHA puts `captcha_token`, `captcha_url` and `captcha_retry_at` (when the retry with that token may reach Proton) in `data`; a partial sync its counts.
- `--json` exit codes are the same as without it.

### Schema version 2

The schema is version 2 since the multi-account layout:

- `pcb status` data is `{daemon, accounts[], alerts}`, plus `listener` (and `mcp`) while a daemon answers: without one there is no listener to report, so the key is absent; an account's login fields moved under `accounts[].login`;
- `pcb doctor` names each per-account result `<address>: <check>` (the local ID when the account has no address).

## Exit codes

The same failure has the same code in every command, with `--json` or without.

| Code | Means | Examples |
|---|---|---|
| `0` | done | |
| `2` | bad invocation: unknown command (the hint names the group's help and its commands), flag or argument | several accounts and none named; a selector that names no account |
| `10` | you must act: answer, type a secret or run something | a login; a lost or unreadable key file; a `secrets.sealed` or `state.json` written by a newer pcb (upgrade pcb) |
| `20` | blocked: the machine or the environment prevents the operation; often transient (retry) | no daemon; the port in use; another `pcb serve` running; a Proton call past its 30 s deadline |
| `30` | failed and rolled back | reserved: no `pcb` command returns it yet |
| `40` | partly done and resumable | a `pcb install` or `pcb uninstall` step failed (finished steps are kept: run it again); `pcb sync` where some calendars failed (`N of M calendars failed to sync`) |

systemd does not restart the service on 10: it is a fault you must repair. Docker does ([container](container.md#when-the-container-stops-or-restarts-in-a-loop)).

An unexpected error with no code of its own exits 20. So does a bug that makes a command panic: with `--json` the one envelope says `internal error`, and the stack goes to stderr, never to stdout. In `pcb serve` and `pcb-ui`, a bug in one account's sync loop stops that account (`account-stopped`, exit code 20, `internal error`) while the others keep serving.

## Warnings

Warnings from the code that runs a command (a clock set back, a client that could not be created) go to stderr as `Warning: …`, also with `--json`.

## Notes

- **Do not downgrade.** A pcb from before CalDAV passwords could be shown again refuses the version 2 `clients.json` later versions write, and a single-account pcb cannot use the per-account layout a multi-account version moves the state to ([install](install.md#no-downgrade-after-the-multi-account-layout)).
- **A signed-out account from before the upgrade.** An account that was signed out when the upgrade moved it to the per-account layout has no address: the first `pcb login` of an address pcb does not know takes it over, with its CalDAV clients. If that login is a different Proton account, the old Thunderbird password would read the new account's calendars: run `pcb client rotate NAME` for each client (`pcb client list`).
- **`pcb client show` without a daemon** builds the URL from `config.toml`, so it ignores a `pcb serve --listen` override; with one running it prints the daemon's own listener.
- **A missing key file.** With no key file but with `secrets.sealed` present, `pcb client add` and `pcb client rotate` refuse (exit 10): minting a new key would make the stored session unreadable. Restore the key file, or run `pcb logout` then `pcb login`.
