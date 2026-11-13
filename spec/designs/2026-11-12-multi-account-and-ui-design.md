# Multi-account core (M6) and desktop UI `pcb-ui` (M7) — design

- **Date**: 2026-11-12
- **Status**: M6 built 2026-11-12; M7 not planned yet. Where the built M6 differs from §4, the paragraph says so, marked "Built:" with the controller ruling (`.superpowers/sdd/2026-11-12-m6-multi-account/progress.md`, summarised in SPEC §18)
- **Decided by**: author (choices marked "author"); the rest is the agent's call, marked [call]
- **Amends**: decision 0001 (GUIs as clients of the local API), 0013 (the CalDAV username; done, by 0014), SPEC §3 (GUIs not planned), §4 and §13 (one account per daemon), §18 (nothing after M5): M6 part done, see decision 0014

## 1. Intent

The author wants a desktop experience modelled on Proton Mail Bridge: a tray icon, a small window listing Proton accounts, and per account the adapters to switch on and the CalDAV credentials to copy into Thunderbird. Running the UI runs the bridge; quitting the UI stops it. The bridge stays usable headless (`pcb serve`, systemd, container), and both share one codebase.

That needs two things the code does not have:
1. **Several Proton accounts in one process** (M6). Today one process serves exactly one account; `Login` refuses a second (`internal/service/login.go:153`). Author: multi-account goes in the core, so `pcb serve` and the CLI get it too ("we really want only one codebase, and it will be useful anyways even headless").
2. **A desktop binary `pcb-ui`** (M7) that runs the same daemon in-process and draws a tray icon and a window.

Order: M6, then M7. M7 is planned and built only after M6 merges.

## 2. Goals, non-goals, anti-goals

Goals:
- M6: one process (`pcb serve` or `pcb-ui`) serves N Proton accounts through one CalDAV listener and one control socket; every CLI command that concerns one account can name it; existing single-account installs keep working with no action (state migrated on first start, Thunderbird settings unchanged).
- M7: `pcb-ui` on Linux (X11 and Wayland desktops with a StatusNotifierItem tray): tray icon with status, a window with the account list, add-account wizard (password, TOTP, CAPTCHA), per-account CalDAV switch and credentials, desktop notifications when an account needs a human.

Non-goals (not designed here):
- Windows (next, after M7; author), macOS (author: no Mac to test on), auto-update, bug reports, themes.
- Several CalDAV clients per account in the UI (the CLI keeps `pcb client add`); choosing which calendars to sync; editing `config.toml` from the UI.
- Shared calendars between accounts, any merge of accounts.
- Per-account CalDAV listeners or ports.

Anti-goals (failure even if it works):
- An existing single-account install that loses its session, its cache or its CalDAV passwords at the upgrade, or whose Thunderbird stops syncing.
- One account's state, credentials or events reachable through another account's CalDAV password.
- A login storm: adding accounts or a network outage must not multiply Proton logins (the good-citizen rule, SPEC §2).
- `pcb` (the CLI and the container image) gaining cgo or any GUI dependency.
- Copying code, QML or assets from Proton Mail Bridge or any Proton GPL project; using Proton's logo or name as the app's branding. The layout is inspired by Bridge, nothing more (AGENTS.md, decision 0002).

## 3. Constraints

- All repository rules hold (`AGENTS.md`): `--json` envelope, exit codes, tests that bite, no secrets anywhere, live calls only on the test account, `make lint test` as the gate.
- Import rules (`arch_test.go`): `internal/caldav` imports only the standard library, `internal/service` and `internal/exitcode`; `internal/ctl` only stdlib, `service`, `output`, `exitcode`, `buildinfo`; `internal/cli` never `internal/proton/...`, `internal/store/...` or go-proton-api. The new `internal/ui` follows the `cli` rule.
- `pcb` stays `CGO_ENABLED=0` (Makefile, Dockerfile, distroless static image). Only `pcb-ui` uses cgo.
- Platform for M6: as today (Linux supported; macOS and Windows build). M7: Linux only; `pcb-ui` need not build on other systems yet, but the UI logic must not hard-code Linux outside small platform files, so Windows can follow.

## 4. M6 — Multi-account core

### 4.1 Process model [call]

- One process holds every account of a state directory. A new `service.Hub` (Built: the builder's name) owns one `service.Local` per account, plus what is shared: the config, the sealer (key file), the CalDAV failure limiter, the CalDAV listener's TLS directory `<state>/tls`, the control socket `<state>/pcb.sock`.
- `service.Local` stays single-account: it is constructed on the account's own directory, so most of M1 to M5 is reused. Two things it owns today move out and are passed in: the login guard (`login.go:216`, `session.go:229` take it on `Dirs.State`) and the CalDAV failure limiter (`local.go:171`).
- A process lock `<state>/serve.lock` (new) is held by the process that serves (`pcb serve`, `pcb-ui`) for its whole life; a second one exits 20 naming the owner (`pcb serve` or `pcb-ui`, PID). *Built: the file holds the PID as decimal text and a newline (R20); the message names the PID only (`pcb is already serving (pid N)`), the owner's kind is not recorded.* The serving process also holds the root `<state>/sync.lock`, the lock an M5 `pcb serve` or `pcb sync` takes (`serve.go:75`), so an older binary run by mistake refuses instead of writing. The per-account `sync.lock` stays, so offline `pcb sync --account X` still excludes a running daemon for that account.
- Serve runs one `Local.Serve` loop per account in its own goroutine. An account added while serving starts its loop at once; a removed one stops its loop first. One account's failure never stops the others.
- An account loop that ends on an error a login cannot fix (a damaged cache, M5-R10) is **parked**: the account shows `stopped` with the error's message and hint as an error alert `account-stopped`, its CalDAV answers 503, and it restarts when the user acts (`pcb account restart`, the UI's "Restart", a login, or a process restart). No automatic retry loop. *Built: a damaged cache parks with exit code 10, as the single account does today, not 20 (R21); a sync lock held by another process (an offline `pcb sync --account X`) is not a park: the loop retries every 30 s (R23); a Restart that lands between the loop's end and the park record is a no-op and a second Restart works (R25); the Hub runs every loop with wait-for-login and applies the exit rule below itself (R3).*
- Exit rule of `pcb serve`, so the systemd units (`Restart=on-failure`, `RestartPreventExitStatus=10`, `install/units.go:41-43`) and the container keep their meaning: with `--wait-for-login` (what the units and the image run) the process never exits for an account's sake (*Built: it ends only when its context ends, with no account at all too*). Without it, the process refuses to start with no logged-in account (exit 10, as today) and exits with the first stopped account's code once **every** account has stopped; with one account that is exactly today's behaviour.
- `pcb serve` with no account: waits for a login with `--wait-for-login` (as today), exits 10 without it. Accounts that are logged out wait for a login while the others serve. *Built: an account with CalDAV off counts as running for the exit rule.*
- Proton rate limits stay per session (one API client per account, `proton/api/client.go:51`). The login guard (`loginguard`, one attempt per interval) stays at the root state directory, shared by every account [call]: unattended relogins of several accounts after an outage are spaced instead of fired together.

### 4.2 Layout and migration [call]

```
<state>/
  serve.lock  sync.lock  layout.lock  pcb.sock  tls/  logs/  login.lock  last-login  layout.json
  accounts/<local-id>/
    state.json  state.lock  secrets.sealed  secrets.lock  clients.json  clients.lock  pcb.db  sync.lock
```
*Built: the root also holds `sync.lock` (R7) and the new `layout.lock`.*

- `<local-id>`: 8 random lowercase hex characters, assigned when the account is added, never reused or renamed. Proton's user ID stays inside (`state.json`, cache `meta`) and is checked as today.
- `layout.json` `{ "version": 2 }` marks the new layout. Its absence while `<state>/state.json` exists means the M5 layout: the first command that opens the state (every command but `pcb doctor`, which reads both layouts and never writes) moves the account files into `accounts/<new-id>/`, then writes `layout.json` [call: one rule, no read-only special case to get wrong]. *Built: only the locks it needs are taken, and only while it migrates (R2); with layout v2 `OpenHub` takes neither, so CLI commands work while a daemon serves. Legacy detection counts data files only, never locks (R6).* The move is crash-safe: files go into `accounts/<id>.migrating/`, the directory is renamed, then `layout.json` is written; a start that finds `.migrating` resumes it.
- Migration takes the root `sync.lock` (and `layout.lock`, new, not `serve.lock`); if an M5 `pcb serve` (or `pcb sync`) holds the root `sync.lock`, the migration refuses, exit 20, "stop the running pcb first". *Built: the root `sync.lock` is not moved with the account files (R7: it is v2's root lock); the other lock files move, and a stale root `state.lock`, `secrets.lock` or `clients.lock` an old binary recreates later is left alone, harmless.*
- Downgrade: an M5 binary cannot be changed and would see no root `state.json` (logged out). If it logs in, it writes root `state.json` and `secrets.sealed` beside `layout.json`. M6 detects root account files next to `layout.json` and refuses, exit 20 (*Built: exit 10 since R36, a person must act, so a unit with `RestartPreventExitStatus=10` stops instead of looping; also exit 10 for a `layout.json` newer than M6's, `upgrade pcb`, R9, and for a damaged or unreadable one; another pcb holding `layout.lock` or the root `sync.lock` stays exit 20*), naming the files and the fix (remove them, or move the state aside; hint `this pcb version cannot be downgraded`) [call: never guess which data is current]. The README says "no downgrade after M6", as for `clients.json` v2 in M5. The serving M6 process holds the root `sync.lock`, so an older binary cannot write while it runs.
- `state.json` gains the account's Proton address (`address`; *Built: the key is `address`, not `username`, R8*), kept after a sign-out so a signed-out account still shows its name. The address is not a secret. *Built: an M5 account has none, so `OpenHub` backfills it from the sealed account's username, and a migrated signed-out account without one (its `state.json` readable, R37) is adopted by the first login that matches no account (R16); that login inherits its `clients.json`, so `pcb client rotate` its clients when the new login is a different account (SPEC §4.1).*
- The sealer is shared: every account's `secrets.sealed` and `clients.json` are sealed with the one key (`[secrets] key_file`). `secrets.sealed` keeps its v2 format and its per-file generation mirror in the account's own `state.json`.

### 4.3 Adding, signing out, removing [call]

- `pcb login` asks for (or takes) the Proton username first. If an account with that address exists (pending or not), the login is for that account (a relogin); otherwise it adds an account: a new `<local-id>` directory is created, the login runs in a `Local` on it, and on success the account joins the running set.
- A failed add (wrong password, Proton refusal, TOTP code not given within the 5 min TTL) removes the new directory at once.
- An add stopped by a CAPTCHA keeps its directory as a **pending** account: phase `needs-captcha` as today, its address in `state.json`, no Proton user ID yet. It is listed by `account list`, `status` and the UI (marked pending), is selected by `login resume --account` (or without a selector when it is the only pending login), and does not count as an account for the "exactly one account" selector rule of the other commands. It ends when the login completes, when `pcb account remove` removes it, or at the first Hub open more than 24 h after it was created [call: the solved token's lifetime is unknown, E02]. *Built: an abandoned add (no Proton user, never logged in, no data, no CAPTCHA, no held 2FA login) is removed lazily and by `OpenHub` when older than the 2FA TTL plus a minute (R11, R13).*
- A login whose Proton user ID turns out to be an account already present (another address of the same user) is refused, exit 10, naming the existing account, and the new directory is removed. *Built: the session the login opened is revoked first (R12); a 2FA code goes through `Hub.SubmitTOTP`, which applies the same rules (R10).*
- One login in flight per process, as today (`ctl` `oneLogin`).
- **Sign out** = today's `pcb logout`, for one account: revokes the session, wipes `secrets.sealed` and `pcb.db`, resets the login in `state.json`, keeps the directory, the address and `clients.json` (CalDAV passwords stay valid and viewable, as M5 decided). The account stays listed as `logged-out`.
- **Remove** (new, `pcb account remove`): sign out, then delete the account directory. Confirmation as `pcb logout --yes` does; `--non-interactive` without `--yes` exits 10.
- The first successful login of an account creates its `default` CalDAV client, as decision 0013 does today.

### 4.4 CalDAV routing [call]

- One listener for every account (`[caldav] listen`, default `127.0.0.1:1080`).
- **Per-account URLs.** Each account is served under `/dav/<local-id>/` (principal `/dav/<local-id>/principals/me/`, home `/dav/<local-id>/calendars/me/`), and every 401 there carries the realm `pcb <local-id>`. Reason: one Thunderbird profile holding two accounts on the same host would otherwise see one origin, one realm and the same paths for both, and Thunderbird's HTTP auth cache could send account A's password on account B's URLs. Distinct realms and path prefixes keep the two identities apart [assumed → a Thunderbird check in the M6 live session; the design does not depend on the outcome]. On those URLs the password must belong to that account's clients, else 401. *Built: an unknown but well-shaped local ID is also 401 with realm `pcb <id>` and a limiter charge, so IDs cannot be probed; a first segment that is not an ID is a compatibility path (R27). `Router.Route` returns the matched local ID too (R26).*
- **Compatibility URLs.** `/`, `/.well-known/caldav` and the M5 paths `/dav/principals/me/`, `/dav/calendars/me/...` stay, realm `pcb`: the password alone selects the account (each account's client store verifies it; salted SHA-256 over 192-bit random passwords, `access/clients.go:68`: a few hashes per request, no ambiguity). So a migrated install's Thunderbird keeps working with no change. `/` answers `current-user-principal` with the per-account principal, so a new Thunderbird setup ends on per-account URLs.
- The URL printed by `pcb serve`, `pcb client show` and the UI is the per-account one, `<scheme>://<host>:<port>/dav/<local-id>/`. The username shown is the account's Proton address (decision 0013 amended: was the client's name); any username still works.
- The failure limiter (per source IP, `access/limiter.go`) moves to the process: one limiter for all accounts, checked once per request before any verification.
- An account whose CalDAV adapter is off (§4.5) answers 401 on its URLs, and its clients are skipped on the compatibility URLs. *Built: its clients are still verified, with no touch and no activity: a match is 401 with no limiter charge (the account's own Thunderbird shares 127.0.0.1 with every other and would lock them all out), a miss is charged (R28).*
- An account whose `clients.json` cannot be loaded: 503 on its own URLs (as today, no limiter charge). On the compatibility URLs, the other accounts are tried; if none matches, 503 while any account's store is broken (the password may be that account's), else 401. *Built: that 503 is not charged to the limiter either (R29); the adapter's log carries `account=<local-id>` (R30).*
- A parked or logged-out account: 503 with `Retry-After` after authentication, as today for a daemon waiting for a login.

### 4.5 Adapters switch [call]

- Per account, `state.json` gains `adapters: { "caldav": true }` (default on, also for migrated accounts). *Built: one boolean key `caldav`; absent (an M5 `state.json`) is on. The switch is refused while a login of that account waits for its 2FA code.*
- Off: CalDAV refuses that account's clients (401) and its sync loop pauses, since nothing consumes it. On: the loop resumes with a pass at once. The switch is the adapter list the UI shows; CalDAV is the only adapter in v1.
- CLI: `pcb account caldav on|off [--account A]`.

### 4.6 CLI [call]

- Account selector: `--account A`, or `PCB_ACCOUNT`, where `A` is the Proton address (case-insensitive) or the local ID. Optional when exactly one account exists; with several and none given, the command exits 2 and lists the accounts (`--json`: `data.accounts`). *Built: "exactly one" counts accounts that are not pending (a lone pending add is found by `login resume` and `logout`, R34); `SelectAccount` is shared by `service.Hub` and the CLI over a daemon's list; a command that does not take the selector refuses `--account` (exit 2); a selector that names no account is exit 2, but none given with no account to choose (none, or only pending adds) is exit 10, a person must add one (R38).*
- Takes the selector: `logout`, `login resume`, `calendar list`, `event list`, `sync`, `client add|list|show|rotate|remove`, `account caldav`, `account restart`, `account remove`.
- `pcb login`: no selector, the username decides (§4.3).
- New `pcb account list`: local ID, address, phase, CalDAV on/off, last sync.
- `pcb status`: every account, each with its login state, sync, alerts; daemon, listener and process-wide alerts once. `--account` narrows it to one.
- `pcb doctor`: process-wide checks once, account checks per account, each result naming its account.
- `pcb uninstall --delete-data`: signs out every account before deleting.
- `--json` data shapes of `status`, `doctor`, `client show` change: `output.SchemaVersion` goes from 1 to 2 [call]. *Built: `pcb status` data is `{daemon, listener, accounts[], alerts}`, each account its identity fields flat plus `login`, `health`, `url`, `alerts`; `pcb doctor` names each result `<address>: <check>` and has an `accounts` layout result.* The project is private and pre-release; no shim for v1 readers.

### 4.7 Control socket [call]

- The CTL API moves to `/v2/` (breaking: account-aware): `GET /v2/accounts`, `GET /v2/status` (all accounts), and the per-account routes under `/v2/accounts/{local-id}/...` (`status`, `login/totp`, `logout`, `caldav`, `restart`, `remove`: removing an account the daemon serves goes through it), plus `POST /v2/login` (the username decides). `Daemon` gains `kind: "serve" | "ui"`. *Built: no `/v2/accounts/{id}/status` route (`GET /v2/status` lists every account, and one that cannot be read keeps its entry with an `account-unreadable` alert); the error envelope of a several-accounts refusal carries `data.accounts` (R31); an unclean path is a 404 envelope (R32); an account-changing call goes through the Hub's own method (R14), and `SignOut` fires the change callback (R17).*
- Still no CTL endpoint for showing or rotating CalDAV passwords (M5-R13): the CLI does those offline, the UI in-process.

### 4.8 Daemon wiring extracted [call]

- What `internal/cli/serve.go` wires today (config, TLS, CalDAV listener, CTL server, signals, drain) moves into a new package `internal/daemon` with one entry point used by both `pcb serve` and `pcb-ui`. *Built: `pcb serve` opens the Hub itself and runs `daemon.Run` with kind `serve` (R33); `daemon.Hub` is the interface `daemon`, `ctl` and `cli` share (R5); `service.AccountPath` is the URL helper (R4).* `internal/daemon` exposes the running accounts (the caller's Hub), the listener info (URL, TLS fingerprint, certificate expiry) and a stop that drains as today (5 s). Signal handling stays in the callers.
- `internal/daemon` follows the `cli` import rule.

### 4.9 Tests (M6)

- The fake Proton already supports several users (`protontest.AddUser`, per-user calendars). Contract tests run two accounts side by side: each Thunderbird-like client sees only its account's calendars; a write through account A never reaches account B; signing out A leaves B serving; one account's CAPTCHA does not stall the other.
- Migration: an M5 fixture state directory (made by M5 code in the test, not a hand-written blob) migrates, keeps its session, cache and CalDAV passwords, and a crash at each step of the move resumes correctly.
- The fake's failure injection (`SetCaptcha`, `FailNext`, `StallNext`, `fake.go:218-255`) and its 2FA options apply to the whole fake today; M6 makes them per user first, so "one account's CAPTCHA does not stall the other" can be tested. `AddUser` already gives each user a second address (for the "same user, other address" refusal).
- Routing: on per-account URLs a password of account B is refused on account A's paths; on the compatibility URLs each password reaches its own account.
- Live: one test account only (AGENTS.md). The live run checks the migration, one account, and in Thunderbird the per-account URL; multi-account itself is proven on the fake only (author, §8).

## 5. M7 — `pcb-ui` on Linux

### 5.1 Binary and process [author + call]

- Separate binary `pcb-ui` (author), `cmd/pcb-ui/main.go`, a few lines calling `internal/ui`. Built with cgo; `pcb` unchanged.
- It runs `internal/daemon` in-process (author: no subprocess, no CLI call): the same CalDAV listener and control socket as `pcb serve`, so `pcb status`, `pcb client show` and Thunderbird work the same whichever runs. The UI reads and acts through `service.Accounts` in-process.
- Quit (tray menu) drains and stops everything. Closing the window only hides it.
- Single instance: a second `pcb-ui` that finds `serve.lock` held asks the owner over CTL; if the owner is a `pcb-ui`, it asks it to show its window (`POST /v2/ui/show`, served only by a UI owner) and exits 0; if the owner is `pcb serve`, it shows a dialog naming the PID and how to stop it (`pcb service stop` when installed as a service, otherwise "stop the `pcb serve` process") and exits 20.
- Logs: no terminal, so `<state>/logs/pcb-ui.log`, size-capped with one rotated file [call: 10 MiB × 2]. Same content and levels as `pcb serve`.

### 5.2 Toolkit [call]

- **Fyne v2** (Go only, tray built in through StatusNotifierItem over D-Bus, notifications, runs on Windows later; cgo for OpenGL). Wails was considered: closer to Bridge's look (HTML/CSS), but brings a JavaScript toolchain. The author may overrule.
- New dependencies are audited and credited in `README.md` (AGENTS.md).
- GNOME without the AppIndicator extension shows no tray icon: `pcb-ui` then opens its window at start and says, once, that the tray needs the extension [call].

### 5.3 Screens

Tray icon, three states: ok, syncing, needs attention (any account with an error-severity alert). Menu:
- one line per account: address and short state ("synced 2 min ago", "needs login"); clicking it opens the window on that account;
- "Open pcb", "Quit".

Window (single, about 800 × 550 [call]):
- **Left column**: the accounts, each with a state dot, and "+ Add account".
- **Account pane**:
  - header: address, state, last sync; buttons "Sign out" and "Remove" (with a confirmation naming what is deleted); a signed-out account shows "Sign in" instead of "Sign out".
  - **Adapters**: a CalDAV switch (§4.5).
  - **CalDAV connection**: URL, username (the address), password masked with "Show" and "Copy", TLS certificate fingerprint when TLS is on, "New password" (rotates the `default` client after a confirmation; the old one stops at once). This is `pcb client show` and `rotate` for the `default` client.
  - **Calendars**: name, read-only badge (subscribed, holidays, or write gate closed), last sync or error.
  - **Alerts** of that account, with their fix text (the `ctl` alert rules, reused).
- **Add-account wizard**: address + password, "Repair the session without me" (unattended relogin, off by default, with the one-line warning `doctor` gives) → TOTP code if asked → CAPTCHA if asked ("Open verification" launches `https://verify.proton.me/...` in the browser, then "I solved it, continue") → done: the CalDAV connection block for the new account, ready to copy. Errors as the CLI words them (`exitcode.Error` message and hint).
- **Settings** (gear): start at login (writes or deletes `~/.config/autostart/pcb-ui.desktop`), log level, open the logs folder, open `config.toml` in the default editor, "Restart bridge" (to apply a hand-edited config), the `pcb doctor` report. Listen address and TLS are shown read-only.

### 5.4 Secrets on the desktop [call; details settled when M7 is planned]

- The Secret Service sealer (SPEC §7.3, planned, not built) is built in M7 in `internal/store`, pure Go over D-Bus (a vetted library, audited), so both `pcb` and `pcb-ui` can open what the other sealed (the sealer name is in the AAD of `secrets.sealed` and `clients.json`).
- The backend is chosen once, when the state is created, and recorded in `layout.json` (`sealer`); it is never probed again. A fresh desktop state picks Secret Service when an unlocked keyring answers, else the key file. An existing state keeps its key file.
- A locked keyring (a user service started before the desktop login): accounts wait with an alert `keyring-locked` and retry every 30 s; nothing is re-sealed with another backend.
- The UI never holds a secret longer than a call: the password field is cleared once `Login` returns; a shown CalDAV password is hidden again when the account pane changes.

### 5.5 Notifications

A desktop notification when an account gains an error-severity alert (`needs-login`, `needs-captcha`, `human-verification`, `sync-stalled`), once per alert occurrence, naming the account and the fix. Clicking it opens the window on that account.

### 5.6 Install

`make ui` builds `pcb-ui`; `make install-ui` puts it in `~/.local/bin`, with `pcb-ui.desktop` in `~/.local/share/applications` and an icon of our own (not Proton's) in `~/.local/share/icons`. A user who also installed the systemd service gets the §5.1 conflict dialog; `pcb doctor` warns when both are installed.

### 5.7 Tests (M7)

- The UI logic is a view model (account list, states, what the tray shows, wizard steps, when to notify) in a package without Fyne, tested like the rest against the fake Proton through `service.Accounts`.
- The Fyne layer is thin and tested with Fyne's headless test driver: each screen renders the model and each button calls the expected action.
- CI: a job with the cgo build dependencies builds `pcb-ui` and runs its tests; `make lint test` covers it on the workstation once the dependencies are installed (README lists them).
- No live check needs a display in CI; the author runs one manual session on the desktop (`spec/HUMAN-CHECKS.md`, new step).

## 6. Left to the builder

- Type and package names, except `internal/daemon`, `internal/ui`, `cmd/pcb-ui`.
- How the migration lock is taken for offline commands, within §4.2's rules.
- Exact CTL route shapes and wire types within §4.7.
- Fyne widget choice, icons, spacing, the exact wording of states.
- How the per-account CalDAV routing is wired between `internal/caldav` and `service.Accounts`, within the import rules.

## 7. Documents to update

- New decisions: 0014 (several accounts per process, per-account CalDAV URLs, compatibility URLs routed by password; written in M6), 0015 (`pcb-ui`: separate cgo binary, in-process daemon; amends 0001).
- 0013: the username shown is the account's address.
- SPEC §3, §4, §7.1, §7.3, §9, §13, §14, §15, §18; `README.md`; `spec/HUMAN-CHECKS.md` (M6 migration step, M7 desktop step); `spec/OPEN.md`.

## 8. Settled by the author

- No second Proton test account (2026-11-12): multi-account is proven against the fake Proton only (two or more users); live checks use the one test account (`spec/HUMAN-CHECKS.md` step 8).
