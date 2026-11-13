# proton-calendar-bridge

Private. The calendar counterpart of Proton Mail Bridge: a local
daemon that logs into Proton, decrypts Proton Calendar (end-to-end encrypted,
undocumented API) and serves it through standard and agent-friendly adapters.

Shape (built through M8, see Status):

- **Core** (Go): accounts and login (as unattended as Proton allows), sync
  engine on Proton's change feed, encrypted local cache, access policy, and an
  iCalendar-native event model (decision 0012; JSCalendar deferred).
- **Adapters**: CalDAV (Thunderbird first; read and write), MCP for AI agents
  (M8, decision 0016: `pcb mcp` over stdio, or `/mcp` with a bearer token; below)
  and a CLI; a REST adapter is out of scope (decision 0007).
- **Runs** headless (systemd / Docker), one process for any number of Proton accounts; on a Linux desktop the same daemon runs inside `pcb-ui`, a tray icon and a window (M7, below).
- **Platforms**: Linux amd64 and arm64 are supported. macOS and Windows only build (`GOOS=darwin` and `GOOS=windows` compile `./cmd/pcb`), are not tested, and have no service management; off Unix there is no file lock and no control socket, so `pcb login`, `pcb sync` and `pcb serve` exit 20 on Windows (`run pcb on Linux or macOS`) and the CLI otherwise runs offline on the files (`spec/SPEC.md` §13, "Platforms").

Reference material, not copied wholesale: `cheeseandcereal/proton-cal` and the
`Barben360/proton-cal` fork (reverse-engineered API docs, login and policy
work, audit findings), plus the community bridges carbonate, cal-gateway and
caloxide.

## Status

Milestone M6 (several Proton accounts in one process) is built on top of M5 (daemon and ops), and the initial-version audit (2026-11-12, `spec/audits/2026-11-12-initial-version.md`) is fixed or parked: `pcb login` (password, TOTP,
CAPTCHA resume, opt-in unattended relogin), `pcb logout`, `pcb status`,
`pcb calendar list`, `pcb sync`, `pcb event list`, `pcb client` and
`pcb serve` (CalDAV for Thunderbird, reads and writes) are built and tested
against a fake Proton only, and M5 adds the daemon around them: a control
socket behind a live `pcb status` and `pcb doctor`, `pcb serve --wait-for-login`,
`pcb install`, `uninstall` and `service` (systemd, user or system), a container image,
and CalDAV passwords you can show again (`pcb client show`). Both smoke tests (the image,
and a Debian container with systemd as PID 1) passed locally, and CI publishes the image from main (below). The live sessions against the test account (M1, M2 for the change feed, step 6
for Thunderbird and the M4 writes, then step 7 for the M5 daemon, service and container) are deferred until the author can solve a
CAPTCHA (`spec/HUMAN-CHECKS.md`, "M1 live session", "M2 live session", steps 6 and 7); until then the cache, sync, write and daemon rules rest on research and tests, not on Proton itself. Do not point pcb at an account you care about before steps 6 and 7 have run. M6 is tested against the fake Proton only, with two or more accounts: there is no second Proton test account, so the live check of the migration, one account and Thunderbird on its per-account URL is `spec/HUMAN-CHECKS.md` step 8, not run yet. **No downgrade after M6**: the first M6 command moves an M5 state into `accounts/<id>/` (session, cache and CalDAV passwords come across); an M5 binary then sees no account, and if it logs in anyway M6 refuses to start (exit 10) and names the files (`account files of an older pcb are beside the per-account layout`, hint `remove them or move the state directory aside: this pcb version cannot be downgraded`). Keep a copy of the state directory before upgrading if you may go back. The design is in
`spec/SPEC.md` and `spec/decisions/0014-several-accounts-per-process.md`.

Milestone M7 adds the Linux desktop app `pcb-ui` (below) and a sealer that keeps the sealing key in the desktop keyring (Secret Service). Both are tested against the fake Proton, a fake Secret Service and Fyne's headless test driver; the GUI itself (tray, notifications, the real keyring) is `spec/HUMAN-CHECKS.md` step 9, in a throwaway VM with a GNOME session and the test account, not run yet. See `spec/decisions/0015-pcb-ui.md`.

Milestone M8 adds an MCP adapter, so AI agents read and edit the calendars they are granted ("Agents (MCP)", below): `pcb mcp` over stdio for an agent on this machine, remote `/mcp` with a bearer token for one elsewhere, grants per agent, and a login the agent starts but you finish (in `pcb-ui` or on a one-time page on 127.0.0.1). It is tested against the fake Proton through the MCP SDK's own client, with a scan for secrets over a whole session, and `make rehearse` drives `pcb mcp` against `pcb-fake`; a real agent client (Claude Code) against the test account is `spec/HUMAN-CHECKS.md` step 10, not run yet. See `spec/decisions/0016-mcp-adapter.md` and `spec/SPEC.md` §12.

## Usage

```sh
pcb login                              # prompts for what is missing; or set PCB_USERNAME, PCB_PASSWORD, PCB_TOTP; the username decides: a known address is a relogin, a new one adds an account
pcb login --username alice             # the username may be a flag (default: $PCB_USERNAME, else asked)
pcb login --password-fd 3 3<pwfile     # the password never goes in a flag
pcb login --unattended-relogin         # store the credentials so pcb can log in again without you
pcb login resume --captcha-token T     # after solving the CAPTCHA Proton asked for
pcb account list                       # every account: local ID, address, phase, CalDAV and MCP on or off, last sync (a pending add, stopped by a CAPTCHA, is listed too)
pcb account caldav off                 # an account's CalDAV switch (on|off): off, its CalDAV clients are refused; its sync pauses only when MCP is off too
pcb account mcp off                    # an account's MCP switch (on|off): off, agents do not see it; its sync pauses only when CalDAV is off too
pcb account restart                    # serve an account again after it stopped on an error (pcb status shows it as account-stopped)
pcb account remove [--yes]             # sign the account out, then delete its directory (its cache and CalDAV passwords too)
pcb status                             # every account's login; with pcb serve running, its live state too (listener, per-calendar sync and write gate, alerts)
pcb status --daemon                    # exit 20 when no daemon answers (the container's health check)
pcb calendar list [--json]
pcb sync [--full]                      # fill or refresh the local event cache from Proton; not needed while pcb serve runs (it syncs by itself: exit 0 and says so)
pcb sync --watch --interval 30s        # keep it current until Ctrl-C (at least 10s; not with --json)
pcb event list --calendar NAME [--from 2026-11-01] [--to 2026-12-01]   # cached events, no network call
pcb client show [NAME] [--reveal]      # URL, username, password and TLS fingerprint of a client; NAME defaults to "default", else the only client (exit 10 when there are several and none is "default"); --reveal puts the password in --json
pcb client add NAME                    # another CalDAV app password, one per device; works logged out
pcb client rotate NAME                 # a new password; the old one stops working at once
pcb client list                        # the clients, never their passwords
pcb client remove NAME|ID              # its password stops working at once, even for a running pcb serve
pcb serve [--listen HOST:PORT] [--wait-for-login] [--log-level LEVEL]   # sync loop + CalDAV listener + control socket until Ctrl-C (see below)
pcb install [--system] [--plan]        # run pcb serve as a systemd service (see "Install as a service")
pcb uninstall [--delete-data] [--plan]
pcb service start|stop|restart|status|logs [-f]
pcb doctor                             # every check (service, daemon, listener, certificate, clients included), with the fix; exit 0 unless a check fails, 10 when you must act, 20 when only the daemon is unreachable
pcb logout [--yes]                     # revokes the session, through the daemon when it runs; --yes discards a store this pcb cannot read
pcb mcp [--profile read-only|full] [--allow CAPS] [--account A]   # MCP for the agent that runs it, on stdin and stdout (see "Agents (MCP)")
pcb mcp token add NAME --allow read    # a remote agent's bearer token, printed once; also list, remove NAME
```

With several accounts, a command about one account names it: `--account ADDRESS|LOCAL-ID` or `PCB_ACCOUNT` (`logout`, `login resume`, `status`, `calendar list`, `event list`, `sync`, `client ...`, `account caldav|mcp|restart|remove`); with one account (a pending add does not count) it is optional, and with several and none named the command exits 2, says `several accounts: name one with --account` and lists them (`--json`: `data.accounts`). With no account yet (or only a pending add) it exits 10, `no account`, hint `add one with pcb login`; a selector that names no account exits 2. `pcb login` takes no `--account`. `--json` is schema version 2 since M6: `pcb status` data is `{daemon, listener, accounts[], alerts}` (an account's login fields moved under `accounts[].login`), and `pcb doctor` names each per-account result `<address>: <check>` (the local ID when the account has no address).

Secrets come from a terminal prompt, `--password-fd`, or the environment (`PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP`: a code or an `otpauth://` URI; `scripts/pcb.env.example`), never from a flag. When `pcb serve` runs, `pcb login` goes through it, over a control socket only its own user can open; a login that waits for its 2FA code stays in the daemon for 5 minutes, and `PCB_TOTP=<code> pcb login resume --totp` finishes it (the code is never an argument). With no daemon, `pcb login` says so and logs in on its own. `--unattended-relogin` seals the password (and, with 2FA, `PCB_TOTP` as an `otpauth://` URI: a one-time code is refused) so a session Proton ends is repaired by one automatic login, at most one per 60 s; without it a login stores tokens only. `pcb login resume` takes the same flags. `--non-interactive` never prompts: it exits 10 naming what is missing (or the CAPTCHA URL and the `pcb login resume` command to run). A login that hits a CAPTCHA while the stored session is still working leaves that session as it was. Each Proton call of a login has a 30 s deadline (exit 20, run the command again), and a Proton 429 names the wait (`Retry-After`).

Exit codes (`--json` has the same ones): `0` done, `2` bad usage, `10` you must act (a login, a lost or unreadable key file, a `secrets.sealed` or `state.json` written by a newer pcb: upgrade pcb), `20` transient or no daemon (retry; systemd does not restart on 10). The same failure has the same code in every command. A `--json` run prints one envelope on stdout; it can carry `data` and `error` together (a CAPTCHA puts `captcha_token` and `captcha_url` in `data`; a partial sync its counts). Warnings from the code that runs a command (a clock set back, a client that could not be created) go to stderr as `Warning: …`, also with `--json`.

`pcb logout` revokes the session. When the stored secrets were written by a newer pcb or another sealer, pcb cannot revoke it: it says so, asks you to end the session in the Proton web app (Settings, Security, Sessions) and then to type `discard` (or run `pcb logout --yes`) to remove the local files. If another process is syncing, the logout finishes and tells you to run it again once that process stops.

CalDAV clients (Thunderbird, a phone) sign in with an app password, not your Proton one. A `pcb login` that completes while no client exists creates one named `default` (also after you removed them all: a new password); `pcb client show` prints it again whenever you need it, as Proton Mail Bridge shows its bridge password (decision 0013). `pcb client add NAME` makes another for one more device, each separately revocable. Each password is 24 random bytes (base64url) kept in `clients.json` (0600) in the state directory twice: as a salted SHA-256 for the check, and sealed with the key that seals your Proton session, so whoever can read both the state directory and that key can read these passwords (the same access already exposes the Proton session). `show` prints `URL:` (the account's own, `.../dav/<local-id>/`), `Username:` (the account's Proton address; any username is accepted), `Password:` and, over TLS, the certificate's SHA-256 fingerprint; `--json` leaves the password out unless you add `--reveal`. Clients created before M5 keep only their hash: `show` says so and names `pcb client rotate NAME`. Five wrong passwords from one IP in a minute are refused until the minute ends. `pcb logout` keeps the clients and the key; `pcb uninstall --delete-data` removes them.

Notes:
- A pcb from before M5 refuses the version 2 `clients.json` M5 writes, and an M5 pcb cannot use the per-account layout M6 moves the state to: do not downgrade (above).
- An M5 account that was signed out when M6 moved it has no address: the first `pcb login` of an address pcb does not know takes it over, with its CalDAV clients. If that login is a different Proton account, the old Thunderbird password would read the new account's calendars: run `pcb client rotate NAME` for each client (`pcb client list`).
- Without a running daemon, `pcb client show` builds the URL from `config.toml`, so it ignores a `pcb serve --listen` override; with one running it prints the daemon's own listener.
- With no key file but with `secrets.sealed` present, `pcb client add` and `pcb client rotate` refuse (exit 10): minting a new key would make the stored session unreadable. Restore the key file, or run `pcb logout` then `pcb login`.

## Serve to Thunderbird

```sh
pcb login                    # a login with no client yet also creates the CalDAV client "default"
pcb serve                    # prints the URL to enter per account, e.g. "Thunderbird URL (alice@proton.me): http://localhost:1080/dav/1a2b3c4d/"
pcb client show              # in another terminal: URL, username, password and, over TLS, the fingerprint
```

In Thunderbird: New Calendar > On the Network, format CalDAV, the URL of `pcb client show` as Location (`/dav/<local-id>/`: one per account), its username (the account's address; any username is accepted) and its
password (the live session against the bridge is `spec/HUMAN-CHECKS.md` step 6, not run yet). `pcb serve` keeps the cache in sync (a pass every 30 s, every 5 min once nobody has used the bridge for 35 minutes, and at once when a client asks again) and serves it, with writes, until Ctrl-C
(SIGINT or SIGTERM: requests in flight get 5 s, exit 0; a second signal exits 20). Plain `pcb serve` refuses to start logged out (exit 10, `pcb login`),
while `pcb sync` runs or while another `pcb serve` runs (exit 20; the second says `pcb is already serving (pid N)`), and stops when Proton needs you again (exit 10). With `--wait-for-login` (what the service and the image run) it keeps
listening instead: with no account it waits for the first login (no CalDAV client exists before one, so CalDAV answers 401), and an account waiting for a login, from any process, has its CalDAV answer 503 with `Retry-After: 30` until it completes (again whenever Proton ends the session), with no restart.
Logs go to stderr, IDs only; `--log-level debug` (or `[log] level`) adds what a write normalised.
After each sync pass, while an event is served as a placeholder (it could not be decrypted) or marked
`X-PCB-INTEGRITY:unverified` (a signature did not verify), a duplicate
master is left out or a calendar's keys fail, it logs a `render counters` line (counts and calendar IDs), and once
more when they change, and `pcb status` shows them from the running daemon (`dropped_alarms` counts a malformed reminder left out of an event that is still served; a deleted event stops counting at once), with a calendar's write gate and its reason and the alerts
(`account-stopped`, `account-unreadable`, `needs-login`, `needs-captcha`, `human-verification`, `sync-stalled`, `calendar-keys-failed`, `write-gate-undecided`,
`clients-file-unreadable`, `tls-certificate-expiring`, `tls-certificate-expired`), each with the fix; `pcb status` exits 0 whatever the alerts.

Several accounts: `pcb login` with another username adds an account to the same process (one CalDAV listener, one control socket, one URL per account). Each account has its own CalDAV passwords, its own cache and its own sync loop, so one account's CAPTCHA, damaged cache or expired session never stops another's (a stopped account shows an `account-stopped` alert, answers 503 and starts again with `pcb account restart` or a login). Use the per-account URL in Thunderbird; the M5 URL (`/`, `/dav/calendars/me/...`) still works, the password alone choosing the account, so an upgraded install keeps syncing with no change. Two accounts in one Thunderbird profile have distinct realms (`pcb <local-id>`) and paths so its HTTP auth cache should keep their passwords apart: that is checked live in `spec/HUMAN-CHECKS.md` step 8, not yet. Wrong passwords count against one limiter shared by all accounts. `pcb serve` without `--wait-for-login` stops once every account has stopped, with the first one's exit code; with it (the service and the image) it never stops for an account.

## Install as a service

From a clone, on a host with systemd (252 or later: Debian 12 or 13):

```sh
make install                 # bin/pcb -> ~/.local/bin/pcb (PREFIX=/usr/local to change it)
pcb install --plan           # what it would do; changes nothing
pcb install                  # user unit (no root), directories 0700, a default config.toml; enables and starts it
loginctl enable-linger "$USER"   # so it runs without a login session; pcb install prints this line unless already on
pcb login                    # goes through the running service; creates the CalDAV client "default"
pcb client show              # the URL, username, password and fingerprint for Thunderbird
pcb service status           # also: start, stop, restart, logs -f
pcb doctor                   # service, daemon, listener, certificate, clients, with the fix for each failure; exit 0 while the service waits for your login (needs-login shows as "!")
```

The unit runs `pcb serve --wait-for-login`: it waits for your first login, and answers 503 while an account needs a login again; after a reboot or a restart it
serves again by itself. `pcb install` is idempotent (a second run keeps every file, and never replaces an edited `config.toml`);
edit `[caldav]` (`listen`, `hostnames`) in `~/.config/pcb/config.toml`, then `pcb service restart`.

For a server: `sudo pcb install --system` creates the `pcb` user, uses `/etc/pcb` and `/var/lib/pcb`, copies the binary to
`/usr/local/bin/pcb` and installs the fully hardened system unit. The control socket answers only the daemon's own user, so run the
CLI as that user, as `pcb install` prints it: `sudo -u pcb env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb /usr/local/bin/pcb login`
(and `... client show`). `pcb service start|stop|restart` needs root with `--system`; `status` and `logs` do not. A command that opens the state, run as root against a state directory owned by `pcb`, refuses (exit 10, `sudo -u pcb pcb ...`) rather than leave root-owned files the service cannot read. `pcb install` refuses a binary or home path with a space, `%`, `$` or a non-ASCII letter (exit 10, also under `--plan`): systemd would split or expand it.

`pcb uninstall` stops the service and removes the unit, keeping your data. `pcb uninstall --delete-data` (it asks, or `--yes`) first
logs out, which revokes the Proton session, then deletes the config and state directories (and with `--system` the `pcb` user); it
removes only a directory named `pcb`, and keeps and names a key file kept elsewhere. A state sealed with the desktop keyring also loses its `pcb sealing key` item ("The desktop keyring" below). It leaves `/usr/local/bin/pcb` and prints the `rm` line.
`--plan` shows the steps without running them. The live run on a fresh host is `spec/HUMAN-CHECKS.md` step 7.1, not run yet.

## Desktop app (pcb-ui, Linux)

`pcb-ui` is the desktop face of the same bridge, in the spirit of Proton Mail Bridge: a tray icon (ok, syncing, needs attention), a window with your Proton accounts, an add-account wizard (password, 2FA code, CAPTCHA), per account the CalDAV switch and the connection to copy into Thunderbird (URL, username, a password hidden behind Show and Copy), and desktop notifications when an account needs you. It is a separate binary that runs the daemon itself: no `pcb serve` is needed, and `pcb status`, `pcb client show` and Thunderbird work the same while it runs. Closing the window hides it; Quit in the tray menu stops the bridge. Without a tray, closing the window quits (the same drain as Quit).

```sh
make ui                      # bin/pcb-ui (cgo: needs the packages below)
make install-ui              # $(PREFIX)/bin/pcb-ui, share/applications/pcb-ui.desktop (Exec= the absolute path), share/icons/hicolor/scalable/apps/pcb-ui.svg; PREFIX ?= ~/.local
pcb-ui [--background]        # --background: tray only (the window opens anyway when no tray answers)
```

- **Build dependencies** (Fyne draws with OpenGL on X11 and Wayland, so `make ui` needs the C headers; `make build` and the container image do not, but `make lint test` does, since `go vet`, golangci-lint and `go test ./...` compile `cmd/pcb-ui` with cgo): Debian and Ubuntu, `sudo apt-get install libgl1-mesa-dev libwayland-dev libxkbcommon-dev xorg-dev` (what CI installs); Arch, `sudo pacman -S libglvnd libxcursor libxrandr libxinerama libxi wayland libxkbcommon` (from Fyne's documented list, not run here), plus a C compiler on both.
- **GNOME shows no tray icon** unless the AppIndicator extension (AppIndicator and KStatusNotifierItem Support) is installed and enabled. Without a tray, `pcb-ui` opens its window and says so once, and closing that window quits `pcb-ui`, since nothing could bring it back.
- **One server per state directory.** `pcb-ui` and `pcb serve` (the systemd service, the container) cannot both serve one state. A second `pcb-ui` just shows the first one's window; `pcb-ui` facing `pcb serve` says so in a dialog and exits 20 (`pcb service stop` first when it is installed as a service). `pcb doctor` warns when the desktop entry and the service are both installed.
- **Start at login:** Settings, "Start at login" writes `~/.config/autostart/pcb-ui.desktop` (`$XDG_CONFIG_HOME/autostart`) with the absolute path and `--background`; switching it off deletes that file.
- **A failed restart shows.** After you edit `config.toml` (Settings, "Open config.toml") and press "Restart bridge", an error in the file stops the bridge: the window says `bridge stopped: <error>`, the tray shows attention, and the account actions refuse until a Restart succeeds.
- **Logs:** `<state>/logs/pcb-ui.log` (10 MiB, then `pcb-ui.log.1`, one rotation). The log level in Settings lasts until the bridge restarts; set `[log] level` in `config.toml` to keep it.
- **Linux only** for now (X11 and Wayland with a StatusNotifierItem tray). `pcb` itself stays one static binary with no GUI dependency.

### The desktop keyring (Secret Service)

A state created on a desktop session whose keyring (GNOME Keyring, KWallet) is unlocked keeps its sealing key there, as one item named `pcb sealing key`, instead of a key file beside the data. `[secrets] backend` in `config.toml` chooses, **once, when the state is created**: `auto` (default: the keyring when an unlocked one answers within 2 s, else the key file), `keyfile`, or `secret-service` (refuses to create the state, exit 10, when the keyring cannot be used). The choice is recorded in `layout.json` and never probed again: an existing state, an M5 or M6 one included, keeps its key file. The first command that opens a new state decides, so run it where you want it (a `pcb status` over SSH, without a session bus, fixes the key file); `pcb doctor` shows `sealer:` and says `not chosen yet` before that.

`pcb` and `pcb-ui` both open what the other sealed. A locked keyring (a service started before you logged in) is not an error to fix: the account waits, shows the alert `keyring-locked`, makes no Proton login and retries every 30 s until you unlock it. pcb never prompts for the keyring. Any process of your login session can read the item, so against a same-user process it is no stronger than a key file; it keeps the key off the disk beside the data. `pcb uninstall --delete-data` removes the item (a `remove keyring` step after the state directory); when the keyring is locked or not available it deletes the data anyway and its output says how to delete the item by hand in Seahorse. `pcb account remove` keeps it, since the key belongs to the state `[verified: TestUninstallDeleteDataRemovesTheKeyringItem, TestUninstallDeleteDataWithoutTheKeyringSaysHowToRemoveTheItem, TestAccountRemoveKeepsTheKeyringItem]`.

When no keyring answers at all (`pcb` over SSH, or a service started outside the desktop session), commands on a keyring state exit 20 with `the desktop keyring is not available` and the hint to run pcb inside the desktop session; `pcb status` shows `secrets: keyring-unavailable` and the alert `keyring-unavailable`, never `needs-login`: the stored session is intact, so do not log out. A login checks the keyring first and, when it is locked or not available, refuses before contacting Proton.

## Agents (MCP)

`pcb` is an MCP server, so an AI agent (Claude Code, or any client that speaks MCP and can send a header) reads your calendars and, when you allow it, edits them. It has eleven tools: `status` (with the current time: agents have no reliable clock), `calendars_list`, `free_busy`, `events_list` (recurring events expanded), `event_get`, `events_search`, `event_create`, `event_update`, `event_delete`, `login` and `logout`. Text that comes from calendars (titles, descriptions, locations, attendee names) reaches the agent marked `untrusted`, with a note that it is data written by others, never instructions, and invitations from someone else are flagged; that helps, but your controls are the grants below, the ETags (an agent edits only what it just read, and only the fields it names: alarms, exceptions and anything else stay) and the delete confirmation `[verified: TestUntrustedFieldsAreGrouped, TestOccurrencesFlagExternalOrganizer, TestUpdateStaleETagIsConflict, TestApplyEditKeepsUntouchedLines]`. Built and tested against the fake Proton only; with a real client and Proton it is `spec/HUMAN-CHECKS.md` step 10, not run yet `[assumed → step 10]`.

**On this machine (stdio).** The agent starts `pcb mcp`, which speaks MCP on stdin and stdout. When pcb already runs (`pcb serve`, the service, `pcb-ui`), it relays the session to it over the control socket; otherwise it serves your accounts itself for as long as the agent's session lasts, and meanwhile `pcb serve` and `pcb-ui` refuse to start, saying an agent session holds the state (end it first) `[verified: TestMCPStdioRelaysToARunningDaemon, TestMCPStdioBecomesTheDaemon, TestServeLockNamesTheKind, TestBridgeNamesAnAgentSession]`. With the pcb user service installed and running or restarting, `pcb mcp` never takes its place: it waits up to 30 s for the service and relays to it, else exits 20 asking for `pcb service start`; a service you stopped changes nothing `[verified: TestMCPStdioWaitsForARestartingUserUnit, TestMCPStdioServesNextToAStoppedUserUnit]`. For Claude Code `[assumed → step 10]`:

```sh
claude mcp add pcb -- pcb mcp                    # read-only (the default): read, plus starting a login
claude mcp add pcb -- pcb mcp --profile full     # also create, edit, delete, log out
```

or in a project's `.mcp.json`: `{"mcpServers": {"pcb": {"command": "pcb", "args": ["mcp", "--profile", "full"]}}}`. Give the absolute path of `pcb` when the agent's `PATH` lacks it.

What an agent may do:

| Profile or `--allow` capability | Tools |
|---|---|
| `freebusy` | `status`, `calendars_list`, `free_busy` (busy times only) |
| `read` (includes `freebusy`) | + `events_list`, `event_get`, `events_search` |
| `write` (includes `read`) | + `event_create`, `event_update` |
| `delete` (needs `write`) | + `event_delete` |
| `login` (stdio only) | `login` |
| `manage` (stdio only) | `logout` |
| `--profile read-only` (default) | `read` + `login` |
| `--profile full` | all six |

`--allow write,delete` replaces the profile; `--account ADDRESS` (repeatable) limits the session to those accounts. Over stdio a profile is a guard rail, not a lock: any program running as you can run `pcb` itself. `pcb account mcp off` hides an account from every agent at once, whatever their grants (its sync keeps running while CalDAV is on) `[verified: TestToolsListFollowsTheGrant, TestProfiles, TestParseCapsAppliesImplications, TestScopedGrantCannotSeeOtherAccounts, TestMCPOffAccountIsInvisible, TestCalDAVOffMCPOnKeepsSyncing]`.

**From another machine (remote, a token).** Off by default `[verified: TestRemoteMCPOffByDefault, TestMCPConfigStrict]`. Turn on `/mcp` on the CalDAV listener, then give each agent its own token:

```toml
# config.toml
[mcp]
remote = true            # serve /mcp on the [caldav] listener (same port and TLS rule)
# timezone = "Europe/Paris"   # for times given without a zone; empty: this machine's zone
```

```sh
pcb mcp token add laptop-agent --allow read [--account ADDRESS] [--calendar ID] [--expires 30d]   # prints the token once: pcbm_...
pcb mcp token list                     # names, grants, last use, expiry; never the tokens
pcb mcp token remove laptop-agent      # it stops working at once
```

A token is stored as a salted hash only: lost, remove it and add another. It can never be granted `login` or `manage`, and an account or calendar outside its grant does not exist for it `[verified: TestTokenAddStoresOnlyAHash, TestMCPTokenRefusesLoginCap, TestScopedGrantCannotSeeOtherAccounts]`. The agent sends `Authorization: Bearer <token>`; keep the token out of command lines, for example in Claude Code's `.mcp.json` from an environment variable `[assumed → step 10]`:

```json
{"mcpServers": {"pcb": {"type": "http", "url": "http://127.0.0.1:1080/mcp",
  "headers": {"Authorization": "Bearer ${PCB_MCP_TOKEN}"}}}}
```

The URL is your listener's plus `/mcp`. On a non-loopback bind pcb serves TLS, with its own certificate unless you set yours (`<state>/tls/cert.pem`, the fingerprint in `pcb status`): the agent's runtime must trust it (for Node-based clients, `NODE_EXTRA_CA_CERTS` pointing at that file) `[assumed → step 10]`, or keep the listener on loopback and reach it through `ssh -L 1080:127.0.0.1:1080 <server>`. After five bad tokens in a minute from one address, further requests from it get 503 with a `Retry-After` (IPv6 counted per /64); a request whose `Host` is not a loopback name, one of `[caldav] hostnames` or the address it came to, or whose `Origin` differs from `Host`, gets 403. With a system service installed (`pcb install --system`), `pcb mcp` does not serve on its own (it would be a second server of the same Proton account): use the remote `/mcp` with a loopback listen and a token instead `[verified: TestRemoteMCPLimiter503AfterFiveFailures, TestLimiterKeysIPv6ByPrefix64, TestRemoteMCPRefusesForeignHost, TestRemoteMCPRefusesForeignOrigin, TestMCPStdioRefusesNextToASystemUnit]`.

**Logging in from an agent.** `login` takes a username and nothing else: you type the password, 2FA code or CAPTCHA, never the agent. In `pcb-ui` it opens the add-account wizard with the address filled in. Otherwise pcb opens a one-time page on `http://127.0.0.1:<port>/login/...` for that username (5 minutes, the first browser to use it keeps it), and the agent shows you the link (or your client opens it, when it supports that). Every answer also names `pcb login --username ADDRESS`, for a `pcb mcp` in a container whose loopback your browser cannot reach. An agent may start one login a minute per session. An account that is already logged in is refused (nothing opens), and a login you abandon at the 2FA step never signs out an account that was working before it `[verified: TestLoginToolRefusesAReadyAccount, TestFlowCancelKeepsAnAccountThatWasReady]`. Over remote there is no login: an account that needs one shows `needs-login` and the step (`pcb login` on the server, or `pcb-ui`) `[verified: TestLoginToolRaisesTheWizardInPcbUI, TestLoginPageSingleUse, TestLoginPageBindsLoopbackWithANonce, TestLoginPageExpires, TestLoginToolResultHasNoSecret, TestLoginToolCooldown, TestLoginAndLogoutNeverRemote, TestStatusGivesNow]`.

**Deletes and sign-outs ask you** when your client supports MCP form elicitation: `Delete the event on <date>: «title»?` (what goes comes first, and the title, which a third party may write, cannot rewrite the question `[verified: TestDeletePromptResistsSpoofing]`); declining deletes nothing. A client without it deletes on the `delete` grant alone, so grant `delete` only to an agent you would let delete unasked `[verified: TestDeleteAsksWhenElicitationDeclared, TestDeleteWithoutElicitationNeedsDeleteCap, TestLogoutConfirms]`. Which clients ask is `[assumed → step 10]`.

**Audit.** Every tool call is one `mcp call` line in the daemon's log (`pcb serve`'s stderr or the journal, `pcb-ui.log`, `pcb mcp`'s stderr when it serves the session itself): the tool, how the agent came (`remote`, `relay`, `stdio`), the token name or profile, the accounts, calendars and event UID, the outcome and the duration; never a title, a description, a link or a secret. `pcb status` shows `mcp: remote on|off; live sessions: ...` while a daemon runs, and `pcb doctor` lists expired tokens and warns when remote is on with a non-loopback bind and no usable token. `[verified: TestAuditLineHasNoContent, TestNoSecretInAnyMCPOutput, TestStatusShowsRemoteMCP, TestDoctorWarnsRemoteWithoutToken]`

## Run in a container

The image (distroless, no shell, UID 65532, one volume `/data`) runs `pcb serve --wait-for-login --listen 0.0.0.0:1080`: it serves
**self-signed TLS** on 1080, comes up healthy with no login and waits for one (CalDAV answers 401 until a login creates the first client). Its health check is
`pcb status --json --daemon`. The image is published at `ghcr.io/barben360/proton-calendar-bridge` (tags `:main` and `:sha-<12 hex>`; CI pushes it only from main) as a **private** package:
pulling it needs `docker login ghcr.io` with a token that can read packages. Everyone else builds it with `make image` (tag `pcb:local`).

`/data` must be writable by UID 65532, and its `config` and `state` directories 0700 (else `pcb serve` exits 20). A named volume
gets that from the image. A bind mount does not: `mkdir -p data/config data/state && sudo chown -R 65532:65532 data && chmod 700 data/config data/state`.

```yaml
# compose.yaml, next to config.toml and pcb_key
services:
  pcb:
    image: pcb:local                  # make image
    container_name: pcb
    restart: unless-stopped
    ports:
      - "1080:1080"                   # TLS; publish the same port: the printed URL carries the container's 1080
    volumes:
      - pcb-data:/data                # one volume: config, state, certificate
    configs:
      - source: pcb_config
        target: /data/config/config.toml
    secrets:
      - source: pcb_key
        target: pcb_key               # /run/secrets/pcb_key, with the host file's owner and mode (no uid/mode outside swarm)
volumes:
  pcb-data:
configs:
  pcb_config:
    file: ./config.toml
secrets:
  pcb_key:
    file: ./pcb_key                   # head -c 32 /dev/urandom > pcb_key  (exactly 32 bytes)
```

```toml
# config.toml
[caldav]
hostnames = ["pcb.lan"]               # names clients use: in the certificate, the first one in the printed URL
[secrets]
key_file = "/run/secrets/pcb_key"     # the key on another mount than the sealed data
```

Without `[secrets] key_file` the key lives in `/data/config`, beside the data it protects. Outside swarm, compose bind-mounts a file
secret with the host file's owner and mode and ignores `uid`, `gid` and `mode`, so give the key to UID 65532 on the host before the
first start, always: pcb runs as 65532 and refuses a key file others can read (mode 0444 included). A key it cannot read stops
`pcb login` and `pcb client add` with "permission denied", and `pcb doctor` says so on its `key file` line (exit 10; so does a key of the wrong size, now with its size).

```sh
head -c 32 /dev/urandom > pcb_key
sudo chown 65532:65532 pcb_key && sudo chmod 600 pcb_key   # unconditional: compose keeps the host owner and mode
docker compose up -d
docker exec -it pcb pcb login     # asks for the username, password and 2FA code; creates the client "default"
docker exec pcb pcb client show   # URL, username, password and the certificate fingerprint for Thunderbird
docker exec pcb pcb status        # live state; docker exec pcb pcb doctor for the checks
```

`pcb client show` prints the URL with the first of `hostnames` and the fingerprint to compare in Thunderbird's certificate exception
dialog (it shows it once and you confirm only an equal one). The image is smoke-tested (`make container-smoke`: healthy with no account, 401 over TLS with or without
credentials, UID 65532, healthy after `docker kill`); the compose file above, the compose secret and `config` mount, and a login in the
container are `spec/HUMAN-CHECKS.md` step 7.2, not run yet. Five wrong passwords from one IP in a minute lock that IP for the minute:
Docker's port publishing keeps the client's source IP, but with the userland proxy (rootless Docker, some setups) every client shares
one IP, so one device's typos lock them all for that minute (`spec/OPEN.md`).

**When the container stops or restarts in a loop.** `docker logs pcb` ends with the error and its hint. Exit 10 is a fault you
must repair (a lost or unreadable key, a damaged cache, an unreadable `state.json` or `layout.json`, an older pcb's files beside the per-account layout, a bad `config.toml`), exit 20 one that may pass
(the port in use, `/data` not writable by 65532). `restart: unless-stopped` restarts after either, as Docker has no equivalent of the
units' `RestartPreventExitStatus=10`, and `docker exec` cannot reach a stopped container. So stop it, run the CLI in a one-off
container on the same volume, secret and config, then start it again:

```sh
docker compose stop pcb
docker compose run --rm --no-deps --entrypoint pcb pcb doctor   # every check, with the fix; also: status, logout
docker compose up -d
```

- `the sealing key file /run/secrets/pcb_key is missing`, or `the stored secrets cannot be opened with the key file ...` (a
  different key): restore `pcb_key` from a backup, or
  discard the stored session with `docker compose run --rm --no-deps --entrypoint pcb pcb logout`, start it, then
  `docker exec -it pcb pcb login`. Clients sealed with the lost key cannot be shown again: `pcb client show` names
  `pcb client rotate NAME` for them, and `pcb doctor` warns once per such client (`caldav client NAME: password cannot be shown`, fix `pcb client rotate NAME`).
- `permission denied` or `mode 0444` on the key file: the host file's owner or mode; `sudo chown 65532:65532 pcb_key && sudo chmod 600 pcb_key`.
- Anything else: the hint in the log, run through `docker compose run` as above.

## Writes from Thunderbird

Creating, editing, moving and deleting events, single occurrences included, go to Proton while `pcb serve` runs: each change is one Proton write call, read back before Thunderbird gets its answer, and never retried once it may have reached Proton (the one exception: it is resent once when Proton answers 401 for an expired token before processing it, which refreshes the session, M4-R23). Thunderbird shows what Proton stored, which can differ from what it sent (the full table: `spec/mapping.md`):

- alarms become Proton notifications (email or device, before or at the start, at most 10); others are dropped, and alarm-level properties such as Thunderbird's per-alarm acknowledgement are lost;
- `STATUS:TENTATIVE` is dropped (Proton refuses it), your colour choice is ignored (Proton's colour stays), `DURATION` becomes an end time, a "monthly on the 2nd Tuesday" or "every weekday" rule is written in Proton's equivalent form;
- unknown properties (Thunderbird's `X-MOZ-*` and others) are kept, encrypted;
- changes to an existing attendee's `ROLE`, `CN`, `PARTSTAT` or `RSVP` are ignored and still answered as saved (204): Proton keeps the attendee's stored line, so a participation status you change in Thunderbird shows its old value again at the next read; change it in the Proton web app.

Writes are refused (HTTP 403) when pcb cannot store them faithfully or safely. For a calendar without offline support, Thunderbird shows a generic error and then **disables the calendar** until you enable it again (E08 §4.2); with offline support it keeps the change locally and retries it silently. Subscribe to pcb calendars with "Offline support" checked (the default in the New Calendar dialog; Calendar properties otherwise). The refusals:

- calendars you may not write: holidays and subscribed calendars, calendars where your member lacks write permission, calendars whose key signature does not check out (`pcb calendar list` shows them `read_only`);
- adding or removing attendees, or an attendee on a new event: invitations are not supported, and **pcb never notifies attendees** in v1 (moving an occurrence of a meeting keeps its attendees but sends them nothing);
- recurrence rules Proton cannot hold: hourly or finer, a count above 49, an end after 2037-12-31, `RDATE`, `EXRULE`, "this and all future occurrences" sent as one change, and the other forms in `spec/mapping.md` §4;
- floating times (no time zone) and time zones Go's database does not know;
- events pcb cannot rewrite safely: one it cannot decrypt or verify, one written by another person, someone else's invitation, one whose key material changed since pcb first saw it;
- edits of an event that holds per-user data pcb cannot rewrite yet (`pcb:personal-part`: typically alarms stored by older Proton clients; edit it in the Proton web app); deleting it works.

`pcb serve` logs each refused write at warning level (`refused a CalDAV write`, with its code, calendar, href and reason, never the event's content), so a change Thunderbird keeps retrying silently shows up there. The same code and href is logged at most once per 10 minutes; the next line after that carries `repeated=N`, the refusals left out.

When Proton is unavailable, rate limiting or asking for a human verification (`pcb serve` logs it: open the Proton web app from the same network and complete it), or a change cannot be confirmed by reading it back, the answer is 503 and Thunderbird tries again later; pcb's cache then holds whatever Proton holds, which Thunderbird sees at its next sync. When someone changed the same event elsewhere in the meantime, the answer is 412 and Thunderbird offers its conflict dialog.

The listener comes from `[caldav]` in `config.toml` (or `--listen`):

```toml
[caldav]
listen = "0.0.0.0:8443"        # default 127.0.0.1:1080
hostnames = ["pcb.lan"]        # names clients use; the first one is in the printed URL
# tls_cert = "/etc/pcb/cert.pem" # your own certificate: both keys or neither
# tls_key = "/etc/pcb/key.pem"
```

A loopback address serves plain HTTP; any other one serves TLS 1.2+ only (with `tls_cert`/`tls_key`, every
address does). Without `tls_cert`/`tls_key`, pcb
generates a self-signed certificate (ECDSA P-256, 825 days, for the hostnames, `localhost` and the bind address
or, on `0.0.0.0`/`[::]`, every interface address) in `tls/` of the state directory (directory 0700, key 0600,
narrowed again at each start; files of another user are refused), reuses it, and replaces it when a hostname or
a specific bind address is no longer covered, under 30 days remain, or the stored pair is unreadable or
mismatched (with a warning). A user certificate expired or within 30 days of expiry gets a warning at start. `pcb serve` and `pcb doctor` print its
SHA-256 fingerprint: Thunderbird shows a certificate exception dialog on first use; confirm the exception only
when its SHA-256 fingerprint is the printed one (again after a replacement; `pcb serve` logs a warning naming the old and the new fingerprint when it replaces one).
`0.0.0.0` listens on IPv4 only (`[::]` is dual stack). Each CalDAV request has limits: bodies of at most 8 MiB for a PUT and 1 MiB for PROPFIND and REPORT (413 above), a requested property name or namespace of at most 256 bytes (400). If `clients.json` cannot be read, CalDAV answers 503 with `Retry-After: 30` (logged once) instead of refusing every password; repair or restore it. A failed TLS handshake (a port scan) is logged at debug level only.

## Build

```sh
make build        # bin/pcb
make lint test    # gofmt, go vet, golangci-lint, go test -race, on JOBS CPUs (4 at most by default); compiles cmd/pcb-ui, so it needs a C compiler and the GL/X headers of "Desktop app"
make install      # bin/pcb -> $(PREFIX)/bin/pcb, PREFIX ?= ~/.local
make ui           # bin/pcb-ui, the desktop app (cgo; Debian: libgl1-mesa-dev libwayland-dev libxkbcommon-dev xorg-dev, Arch: libglvnd libxcursor libxrandr libxinerama libxi wayland libxkbcommon; see "Desktop app")
make install-ui   # pcb-ui, its desktop entry and its icon under $(PREFIX)
make image        # the container image, pcb:local
make container-smoke systemd-smoke   # throwaway containers; each prints PASS last
make rehearse     # developer only: the real pcb against pcb-fake, a loopback fake Proton (never installed)
make live-preflight-test   # developer only: the live-session guard's own test; live-preflight checks a run root
make shellcheck   # developer only: the live-session scripts, with the pinned shellcheck image CI uses (needs docker)
bin/pcb version
```

## Credits

Logic reused from another project is audited, rewritten with our own tests and credited here (decision 0002). Nothing below was copied.

- **cheeseandcereal/proton-cal** (Unlicense): the reverse-engineered API and crypto documentation (`docs/api.md`, `docs/crypto.md`, `docs/overview.md`): the key chain, the locked scope and Proton code 9101, member-level calendar metadata, the v2 bootstrap and the default-calendar setting. Used by `internal/proton/auth`, `keys` and `calendar`.
- **Barben360/proton-cal**, our fork: the two-step CAPTCHA login, the env/TOTP login, and the refresh-token race lesson (one refresh at a time across processes, behind `internal/proton/api` and the token store). Sources audited and rewritten: `pkg/auth` (`auth.go`, `unlock.go`, `loginopts.go`) for `internal/lab/session` and `internal/proton/auth` (SRP login, CAPTCHA stop and resume, TOTP, locked-scope unlock with server-proof check, lock) and `internal/proton/keys` (salts with scope elevation, salted key passphrase check, key unlock); `pkg/calendar` (`calendar.go`, `keys.go`) for `internal/proton/calendar` (calendar list, default calendar, bootstrap, member passphrase decryption, calendar key unlock); `pkg/auth/totp.go` for `internal/otp`. The fork's CLI is also run as an external tool by the data-set seeder (`internal/lab/seed`).
- **go-proton-api**, **go-srp**, **gopenpgp** (MIT, used as libraries). go-proton-api's in-memory test server pattern is the model for and the engine behind our shared fake Proton (`internal/proton/protontest`, an httptest front over its server, which the lab's `internal/lab/labtest` builds on). The developer-only `pcb-lab probe sessions` takes the sessions endpoints and their auth from go-proton-api's `AuthSessions` and `AuthRevoke` (`GET /auth/v4/sessions`, `DELETE /auth/v4/sessions/{UID}`, the session's own token, no extra scope), on our own code and transport (`internal/lab/probe/sessions.go`).
- **develonrails/carbonate**, **WentTheFox/caloxide**, **ProtonMail/WebClients** (GPL-3.0): read only for the client identity strings tried in experiment E03 (`spec/experiments/E03-client-identity.md`). No code from any of them; WebClients is never copied.
- **emersion/go-webdav** (MIT): audited in experiment E17 (`spec/experiments/E17-go-webdav-audit.md`) and **not** used as the server; its CalDAV client, with `go-ical` (MIT), is a test-only dependency that checks our server from the outside and never reaches the binary. It moved `go-vcard` to a newer revision through module version selection (only README and an example differ).
- **modelcontextprotocol/go-sdk** v1.8.0 (the official MCP Go SDK; Apache-2.0, with the contributions not yet relicensed under MIT): the MCP server, transports and test client of the MCP adapter, used as a dependency, nothing copied; audited in experiment E19 (`spec/experiments/E19-mcp-go-sdk.md`). It brings `google/jsonschema-go` (MIT), `segmentio/encoding` and `segmentio/asm` (MIT), `yosida95/uritemplate` (BSD-3-Clause) and `golang.org/x/oauth2` (BSD-3-Clause).
- **Fyne** v2.8.2 (fyne.io/fyne/v2, BSD-3-Clause): the toolkit of `pcb-ui` (window, tray, notifications, headless test driver), used as a dependency, nothing copied (`internal/ui/desktop`, `cmd/pcb-ui`).
- **godbus/dbus** v5.2.4 (BSD-2-Clause): the D-Bus client of the Secret Service sealer and of the desktop notifications, used as a dependency, nothing copied. The Secret Service client is written from the freedesktop.org specification, with no other implementation read (`internal/store/seal`).
- **Thunderbird's CalDAV provider** (mozilla/releases-comm-central, MPL-2.0): read for experiment E08 (`spec/experiments/E08-thunderbird-caldav.md`), source only, no code copied.
- **develonrails/carbonate** (MIT), **jmdlab/cal-gateway** (MIT) and **WentTheFox/caloxide** (MIT; derives from hydroxide, ferroxide and protoxide, all MIT), for the M2 sync design notes (ideas audited, no code copied; the feed research is in `spec/experiments/raw/m2-feed-research.md`, citations by file and line): carbonate's measured pitfall that a cursor derived at `latest` loses events and its ID-driven re-read that ignores `Action`; jmdlab's order (read the feed head before the full listing) and its rule that a re-read row answering 404 is a delete; caloxide's feed endpoint shapes and its 30 s poll with exponential backoff. Rewritten with our own tests (`internal/syncer`, `internal/store/cache`).
- **WentTheFox/caloxide**, **develonrails/carbonate**, **jmdlab/cal-gateway** (all MIT) and **cheeseandcereal/proton-cal** (Unlicense), for the M3 CalDAV design notes (research, no code copied): what each had to patch around go-webdav (getctag and `supported-report-set`, privileges, sync-collection, body limits), the Proton card-to-iCalendar merge and the one-object-per-UID rule, tolerance of undecryptable events. Rewritten with our own tests (`internal/caldav`, `internal/ical`, `internal/proton/cards`).
- **jmdlab/cal-gateway** (MIT), **cheeseandcereal/proton-cal** (Unlicense), **develonrails/carbonate** (MIT) and **WentTheFox/caloxide** (MIT), for the M4 write design (research `spec/experiments/raw/m4-research.md`, citations by file and line; no code copied, every rule rewritten with our own tests):
  - cal-gateway's alarm rules (`internal/proton/notifications.go`, which it says it ported from the web client): EMAIL to an email notification, DISPLAY and AUDIO to a device one, relative-to-start triggers only, nothing after the start except on all-day events, Proton's per-unit bounds, at most 10, and the tri-state of `Notifications` on update (`internal/ical/alarms_write.go`, `internal/service/writeplan.go`); also its refusals of floating times and `RANGE=THISANDFUTURE`, its RRULE INTERVAL bounds, its mirroring of a full PUT onto master and exception rows, and its rule that a refusal must be an honest 403, never a 2xx no-op;
  - proton-cal's RRULE limits (`docs/api.md`: FREQ, COUNT ≤ 49, UNTIL ≤ 2037-12-31, COUNT and UNTIL exclusive, MONTHLY and YEARLY forms; `internal/ical/validate.go`), its documentation of the sync endpoint, the property routing table, the fragment form, the sealing of each part, session-key reuse on update and full-replace semantics (`docs/api.md`, `docs/crypto.md`; `internal/proton/cards`, `internal/proton/events`, `internal/ical/write.go`), and its patch-in-place update;
  - carbonate's and caloxide's live notes on Proton's refusals (signing with the member's address key, bare `SEQUENCE`, RECURRENCE-ID with RRULE), and caloxide's member permission bits (`Write = 8`, `internal/proton/calendar`).
- **WentTheFox/caloxide** (MIT), for the M5 idle cadence: its standby idea, slowing the poll when nobody uses the bridge (idea audited from the M2 research, no code copied). Rewritten with our own definition of idle (no authenticated request for 35 minutes, longer than Thunderbird's 30-minute refresh), a wake on the first request, and our own tests (`internal/service`, `internal/syncer`).
- **Proton Mail Bridge** (and the author's headless Bridge container), for the M5 container and credential model: one `/data` volume, login with `docker exec -it`, state kept in the volume, and a bridge password the user can look up again (decision 0013, `pcb client show`). The usage model only: ideas, no code read or copied; our own image, units and tests (`Dockerfile`, `internal/install`, `internal/ctl`).
- The other prior art listed in spec §20: design references only; nothing from them is used in this repository so far.
