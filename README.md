# proton-calendar-bridge

Private, design phase. The calendar counterpart of Proton Mail Bridge: a local
daemon that logs into Proton, decrypts Proton Calendar (end-to-end encrypted,
undocumented API) and serves it through standard and agent-friendly adapters.

Planned shape:

- **Core** (Go): accounts and login (as unattended as Proton allows), sync
  engine on Proton's change feed, encrypted local cache, access policy, and an
  internal event model based on JSCalendar (RFC 8984).
- **Adapters**: CalDAV (Thunderbird first), read-only `.ics` feeds, a REST API
  documented with OpenAPI 3.1 (management + automation), an MCP server for AI
  agents, a CLI on top of the REST API; GUIs later.
- **Runs** headless (systemd / Docker) or on a desktop, same daemon.

Reference material, not copied wholesale: `cheeseandcereal/proton-cal` and the
`Barben360/proton-cal` fork (reverse-engineered API docs, login and policy
work, audit findings), plus the community bridges carbonate, cal-gateway and
caloxide.

## Status

Design phase, milestone M2 (store and sync): `pcb login` (password, TOTP,
CAPTCHA resume, opt-in unattended relogin), `pcb logout`, `pcb status`,
`pcb calendar list`, `pcb sync` and `pcb event list` are built and tested
against a fake Proton; there is no bridge (daemon, CalDAV) yet. The live
sessions against the test account (M1, then M2 for the change feed, `pcb-lab probe feed`) are deferred until the author can solve a
CAPTCHA (`spec/HUMAN-CHECKS.md`, "M1 live session" and "M2 live session"); the cache and sync rules are source-based until then. The design is in
`spec/SPEC.md`.

## Usage

```sh
pcb login                              # prompts for what is missing; or set PCB_USERNAME, PCB_PASSWORD, PCB_TOTP
pcb login --username alice             # the username may be a flag (default: $PCB_USERNAME, else asked)
pcb login --password-fd 3 3<pwfile     # the password never goes in a flag
pcb login --unattended-relogin         # store the credentials so pcb can log in again without you
pcb login resume --captcha-token T     # after solving the CAPTCHA Proton asked for
pcb status                             # the login state, no network call
pcb calendar list [--json]
pcb sync [--full]                      # fill or refresh the local event cache from Proton
pcb sync --watch --interval 30s        # keep it current until Ctrl-C (at least 10s; not with --json)
pcb event list --calendar NAME [--from 2026-11-01] [--to 2026-12-01]   # cached events, no network call
pcb logout
```

Secrets come from a terminal prompt, `--password-fd`, or the environment (`PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP`: a code or an `otpauth://` URI; `scripts/pcb.env.example`), never from a flag. `--unattended-relogin` seals the password (and, with 2FA, `PCB_TOTP` as an `otpauth://` URI: a one-time code is refused) so a session Proton ends is repaired by one automatic login, at most one per 60 s; without it a login stores tokens only. `pcb login resume` takes the same flags. `--non-interactive` never prompts: it exits 10 naming what is missing (or the CAPTCHA URL and the `pcb login resume` command to run).

## Build

```sh
make build        # bin/pcb
make lint test    # gofmt, go vet, golangci-lint, go test -race
bin/pcb version
```

## Credits

Logic reused from another project is audited, rewritten with our own tests and credited here (decision 0002). Nothing below was copied.

- **cheeseandcereal/proton-cal** (Unlicense): the reverse-engineered API and crypto documentation (`docs/api.md`, `docs/crypto.md`, `docs/overview.md`): the key chain, the locked scope and Proton code 9101, member-level calendar metadata, the v2 bootstrap and the default-calendar setting. Used by `internal/proton/auth`, `keys` and `calendar`.
- **Barben360/proton-cal**, our fork: the two-step CAPTCHA login, the env/TOTP login, and the refresh-token race lesson (one refresh at a time across processes, behind `internal/proton/api` and the token store). Sources audited and rewritten: `pkg/auth` (`auth.go`, `unlock.go`, `loginopts.go`) for `internal/lab/session` and `internal/proton/auth` (SRP login, CAPTCHA stop and resume, TOTP, locked-scope unlock with server-proof check, lock) and `internal/proton/keys` (salts with scope elevation, salted key passphrase check, key unlock); `pkg/calendar` (`calendar.go`, `keys.go`) for `internal/proton/calendar` (calendar list, default calendar, bootstrap, member passphrase decryption, calendar key unlock); `pkg/auth/totp.go` for `internal/otp`. The fork's CLI is also run as an external tool by the data-set seeder (`internal/lab/seed`).
- **go-proton-api**, **go-srp**, **gopenpgp** (MIT, used as libraries). go-proton-api's in-memory test server pattern is the model for and the engine behind our shared fake Proton (`internal/proton/protontest`, an httptest front over its server, which the lab's `internal/lab/labtest` builds on).
- **develonrails/carbonate**, **WentTheFox/caloxide**, **ProtonMail/WebClients** (GPL-3.0): read only for the client identity strings tried in experiment E03 (`spec/experiments/E03-client-identity.md`). No code from any of them; WebClients is never copied.
- **Thunderbird's CalDAV provider** (mozilla/releases-comm-central, MPL-2.0): read for experiment E08 (`spec/experiments/E08-thunderbird-caldav.md`), source only, no code copied.
- **develonrails/carbonate** (MIT), **jmdlab/cal-gateway** (MIT) and **WentTheFox/caloxide** (MIT; derives from hydroxide, ferroxide and protoxide, all MIT), for the M2 sync design notes (ideas audited, no code copied; the feed research is in `spec/experiments/raw/m2-feed-research.md`, citations by file and line): carbonate's measured pitfall that a cursor derived at `latest` loses events and its ID-driven re-read that ignores `Action`; jmdlab's order (read the feed head before the full listing) and its rule that a re-read row answering 404 is a delete; caloxide's feed endpoint shapes and its 30 s poll with exponential backoff. Rewritten with our own tests (`internal/syncer`, `internal/store/cache`).
- The other prior art listed in spec §20: design references only; nothing from them is used in this repository so far.
