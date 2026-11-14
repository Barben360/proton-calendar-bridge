# Commands

Every `pcb` command, how to pick the account a command acts on, the `--json` output and the exit codes. Scripts and agents can rely on the last two: they never change.

## The commands

### Log in and out

| Command | What it does |
|---|---|
| `pcb login` | Prompts for what is missing; or set `PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP`. The username decides: a known address is a relogin, a new one adds an account. |
| `pcb login --username alice` | The username may be a flag (default: `$PCB_USERNAME`, else asked). |
| `pcb login --password-fd 3 3<pwfile` | Reads the password from a file descriptor: the password never goes in a flag. |
| `pcb login --unattended-relogin` | Stores the credentials so pcb can log in again without you. |
| `pcb login --non-interactive` | Never prompts: exits 10 naming what is missing. `--non-interactive` is accepted by every command. |
| `pcb login resume --captcha-token T` | Finishes a login after you solved the CAPTCHA Proton asked for. |
| `PCB_TOTP=<code> pcb login resume --totp` | Finishes a login that waits in the daemon for its 2FA code (from `$PCB_TOTP`, else asked; never an argument). |
| `pcb logout [--yes]` | Revokes the session, through the daemon when it runs; `--yes` discards a store this pcb cannot read. |

Details: [security](security.md#log-in).

### Accounts

| Command | What it does |
|---|---|
| `pcb account list` | Every account: local ID, address, phase, CalDAV and MCP on or off, last sync. A pending add, stopped by a CAPTCHA, is listed too. |
| `pcb account caldav on\|off` | The account's CalDAV switch. Off, its CalDAV clients are refused; its sync pauses only when MCP is off too. |
| `pcb account mcp on\|off` | The account's MCP switch. Off, agents do not see it; its sync pauses only when CalDAV is off too. |
| `pcb account restart` | Serves an account again after it stopped on an error (`pcb status` shows it as `account-stopped`). |
| `pcb account remove [--yes]` | Signs the account out, then deletes its directory (its cache and CalDAV passwords too). |

### Status and checks

| Command | What it does |
|---|---|
| `pcb status` | Every account's login; with pcb serving, its live state too (listener, per-calendar sync and write gate, alerts). |
| `pcb status --daemon` | Exits 20 when no daemon answers (the container's health check). |
| `pcb doctor` | Every check (service, daemon, listener, certificate, clients included), with the fix. Exits 0 unless a check fails, 10 when you must act, 20 when only the daemon is unreachable. |

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
| `pcb client add NAME` | Another CalDAV app password, one per device; works logged out. |
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

### Agents

| Command | What it does |
|---|---|
| `pcb mcp [--profile read-only\|full] [--allow CAPS] [--account A]` | MCP for the agent that runs it, on stdin and stdout ([agents](agents.md)). |
| `pcb mcp token add NAME --allow read` | A remote agent's bearer token, printed once. |
| `pcb mcp token list`, `pcb mcp token remove NAME` | List the tokens (never their values), remove one. |

### Other

| Command | What it does |
|---|---|
| `pcb version` | The build's version. |

## Choose the account

With several accounts, a command about one account names it: `--account ADDRESS|LOCAL-ID`, or the environment variable `PCB_ACCOUNT`.

This applies to `logout`, `login resume`, `status`, `calendar list`, `event list`, `sync`, `client ...` and `account caldav|mcp|restart|remove`. `pcb login` takes no `--account`: the username chooses.

| Accounts | No account named | A selector that names no account |
|---|---|---|
| none yet (or only a pending add) | exit 10, `no account`, hint `add one with pcb login` | exit 2 |
| one (a pending add does not count) | that account | exit 2 |
| several | exit 2: `several accounts: name one with --account`, and the list (`--json`: `data.accounts`) | exit 2 |

## JSON output

Every command has `--json`. It prints one envelope on stdout and nothing else:

```json
{"schema_version": 2, "command": "...", "ok": true, "data": {}}
```

- On failure `ok` is false and `error` is set. An envelope can carry `data` and `error` together: a CAPTCHA puts `captcha_token` and `captcha_url` in `data`; a partial sync its counts.
- `--json` exit codes are the same as without it.

### Schema version 2

The schema is version 2 since the multi-account layout:

- `pcb status` data is `{daemon, listener, accounts[], alerts}`; an account's login fields moved under `accounts[].login`;
- `pcb doctor` names each per-account result `<address>: <check>` (the local ID when the account has no address).

## Exit codes

The same failure has the same code in every command, with `--json` or without.

| Code | Means | Examples |
|---|---|---|
| `0` | done | |
| `2` | bad invocation: unknown command, flag or argument | several accounts and none named; a selector that names no account |
| `10` | you must act: answer, type a secret or run something | a login; a lost or unreadable key file; a `secrets.sealed` or `state.json` written by a newer pcb (upgrade pcb) |
| `20` | blocked: the machine or the environment prevents the operation; often transient (retry) | no daemon; the port in use; another `pcb serve` running; a Proton call past its 30 s deadline |
| `30` | failed and rolled back | reserved: no `pcb` command returns it yet |
| `40` | partly done and resumable | a `pcb install` or `pcb uninstall` step failed (finished steps are kept: run it again); `pcb sync` where some calendars failed (`N of M calendars failed to sync`) |

systemd does not restart the service on 10: it is a fault you must repair. Docker does ([container](container.md#when-the-container-stops-or-restarts-in-a-loop)).

An unexpected error with no code of its own exits 20.

## Warnings

Warnings from the code that runs a command (a clock set back, a client that could not be created) go to stderr as `Warning: …`, also with `--json`.

## Notes

- **Do not downgrade.** A pcb from before CalDAV passwords could be shown again refuses the version 2 `clients.json` later versions write, and a single-account pcb cannot use the per-account layout a multi-account version moves the state to ([install](install.md#no-downgrade-after-the-multi-account-layout)).
- **A signed-out account from before the upgrade.** An account that was signed out when the upgrade moved it to the per-account layout has no address: the first `pcb login` of an address pcb does not know takes it over, with its CalDAV clients. If that login is a different Proton account, the old Thunderbird password would read the new account's calendars: run `pcb client rotate NAME` for each client (`pcb client list`).
- **`pcb client show` without a daemon** builds the URL from `config.toml`, so it ignores a `pcb serve --listen` override; with one running it prints the daemon's own listener.
- **A missing key file.** With no key file but with `secrets.sealed` present, `pcb client add` and `pcb client rotate` refuse (exit 10): minting a new key would make the stored session unreadable. Restore the key file, or run `pcb logout` then `pcb login`.
