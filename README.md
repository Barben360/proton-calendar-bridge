# proton-calendar-bridge

Private, design phase. The calendar counterpart of Proton Mail Bridge: a local
daemon that logs into Proton, decrypts Proton Calendar (end-to-end encrypted,
undocumented API) and serves it through standard and agent-friendly adapters.

Planned shape:

- **Core** (Go): accounts and login (as unattended as Proton allows), sync
  engine on Proton's change feed, encrypted local cache, access policy, and an
  iCalendar-native event model (decision 0012; JSCalendar deferred).
- **Adapters**: CalDAV (Thunderbird first; read and write) and a CLI; REST and
  MCP adapters are out of scope for now (decision 0007).
- **Runs** headless (systemd / Docker) or on a desktop, same daemon.

Reference material, not copied wholesale: `cheeseandcereal/proton-cal` and the
`Barben360/proton-cal` fork (reverse-engineered API docs, login and policy
work, audit findings), plus the community bridges carbonate, cal-gateway and
caloxide.

## Status

Design phase, milestone M4 (CalDAV writes): `pcb login` (password, TOTP,
CAPTCHA resume, opt-in unattended relogin), `pcb logout`, `pcb status`,
`pcb calendar list`, `pcb sync`, `pcb event list`, `pcb client` and
`pcb serve` (CalDAV for Thunderbird, reads and writes) are built and tested
against a fake Proton only; the background daemon comes with M5. The live
sessions against the test account (M1, M2 for the change feed, then step 6 for Thunderbird and the M4 writes) are deferred until the author can solve a
CAPTCHA (`spec/HUMAN-CHECKS.md`, "M1 live session", "M2 live session", step 6); until then the cache, sync and write rules rest on research and tests, not on Proton itself. Do not point pcb at an account you care about before step 6 has run. The design is in
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
pcb client add NAME                    # a CalDAV app password for one client (printed once, never stored); works logged out
pcb client list                        # the clients, never their passwords
pcb client remove NAME|ID              # its password stops working at once, even for a running pcb serve
pcb serve [--listen HOST:PORT]         # sync loop + CalDAV listener until Ctrl-C (see below)
pcb doctor                             # every check (listener, certificate, clients included), with the fix
pcb logout
```

Secrets come from a terminal prompt, `--password-fd`, or the environment (`PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP`: a code or an `otpauth://` URI; `scripts/pcb.env.example`), never from a flag. `--unattended-relogin` seals the password (and, with 2FA, `PCB_TOTP` as an `otpauth://` URI: a one-time code is refused) so a session Proton ends is repaired by one automatic login, at most one per 60 s; without it a login stores tokens only. `pcb login resume` takes the same flags. `--non-interactive` never prompts: it exits 10 naming what is missing (or the CAPTCHA URL and the `pcb login resume` command to run).

CalDAV clients (Thunderbird, a phone) sign in with an app password, not your Proton one: `pcb client add NAME` prints 24 random bytes (base64url) once and stores only a salted SHA-256 in `clients.json` (0600) in the state directory. Use any username, that password, and the URL `pcb serve` prints. Five wrong passwords from one IP in a minute are refused until the minute ends. `pcb logout` keeps the clients; uninstalling removes them.

## Serve to Thunderbird

```sh
pcb client add Thunderbird   # prints the app password once
pcb serve                    # prints the URL to enter, e.g. "Thunderbird URL: http://localhost:1080/"
```

In Thunderbird: New Calendar > On the Network, format CalDAV, the printed URL as Location, any username and the
app password (the live session against the bridge is `spec/HUMAN-CHECKS.md` step 6, not run yet). `pcb serve` keeps the cache in sync (a pass every 30 s) and serves it, with writes, until Ctrl-C
(SIGINT or SIGTERM: requests in flight get 5 s, exit 0); it refuses to start logged out (exit 10, `pcb login`) or
while `pcb sync` runs (exit 20), and stops when Proton needs you again (exit 10). Logs go to stderr, IDs only.
After each sync pass, while an event is served as a placeholder (it could not be decrypted) or marked
`X-PCB-INTEGRITY:unverified` (a signature did not verify), a duplicate
master is left out or a calendar's keys fail, it logs a `render counters` line (counts and calendar IDs), and once
more when they change; `pcb status` does not show these counts, since only the serving process has them.

## Writes from Thunderbird

Creating, editing, moving and deleting events, single occurrences included, go to Proton while `pcb serve` runs: each change is one Proton write call, read back before Thunderbird gets its answer, never retried. Thunderbird shows what Proton stored, which can differ from what it sent (the full table: `spec/mapping.md`):

- alarms become Proton notifications (email or device, before or at the start, at most 10); others are dropped, and alarm-level properties such as Thunderbird's per-alarm acknowledgement are lost;
- `STATUS:TENTATIVE` is dropped (Proton refuses it), your colour choice is ignored (Proton's colour stays), `DURATION` becomes an end time, a "monthly on the 2nd Tuesday" or "every weekday" rule is written in Proton's equivalent form;
- unknown properties (Thunderbird's `X-MOZ-*` and others) are kept, encrypted.

Writes are refused (HTTP 403; Thunderbird shows a generic error, or, for a calendar with offline support, keeps the change locally and retries it silently) when pcb cannot store them faithfully or safely:

- calendars you may not write: holidays and subscribed calendars, calendars where your member lacks write permission, calendars whose key signature does not check out (`pcb calendar list` shows them `read_only`);
- adding or removing attendees, or an attendee on a new event: invitations are not supported, and **pcb never notifies attendees** in v1 (moving an occurrence of a meeting keeps its attendees but sends them nothing);
- recurrence rules Proton cannot hold: hourly or finer, a count above 49, an end after 2037-12-31, `RDATE`, `EXRULE`, "this and all future occurrences" sent as one change, and the other forms in `spec/mapping.md` §4;
- floating times (no time zone) and time zones Go's database does not know;
- events pcb cannot rewrite safely: one it cannot decrypt or verify, one written by another person, someone else's invitation, one whose key material changed since pcb first saw it.

When Proton is unavailable or rate limiting, or a change cannot be confirmed by reading it back, the answer is 503 and Thunderbird tries again later; pcb's cache then holds whatever Proton holds, which Thunderbird sees at its next sync. When someone changed the same event elsewhere in the meantime, the answer is 412 and Thunderbird offers its conflict dialog.

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
when its SHA-256 fingerprint is the printed one (again after a replacement).

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
- **emersion/go-webdav** (MIT): audited in experiment E17 (`spec/experiments/E17-go-webdav-audit.md`) and **not** used as the server; its CalDAV client, with `go-ical` (MIT), is a test-only dependency that checks our server from the outside and never reaches the binary. It moved `go-vcard` to a newer revision through module version selection (only README and an example differ).
- **Thunderbird's CalDAV provider** (mozilla/releases-comm-central, MPL-2.0): read for experiment E08 (`spec/experiments/E08-thunderbird-caldav.md`), source only, no code copied.
- **develonrails/carbonate** (MIT), **jmdlab/cal-gateway** (MIT) and **WentTheFox/caloxide** (MIT; derives from hydroxide, ferroxide and protoxide, all MIT), for the M2 sync design notes (ideas audited, no code copied; the feed research is in `spec/experiments/raw/m2-feed-research.md`, citations by file and line): carbonate's measured pitfall that a cursor derived at `latest` loses events and its ID-driven re-read that ignores `Action`; jmdlab's order (read the feed head before the full listing) and its rule that a re-read row answering 404 is a delete; caloxide's feed endpoint shapes and its 30 s poll with exponential backoff. Rewritten with our own tests (`internal/syncer`, `internal/store/cache`).
- **WentTheFox/caloxide**, **develonrails/carbonate**, **jmdlab/cal-gateway** (all MIT) and **cheeseandcereal/proton-cal** (Unlicense), for the M3 CalDAV design notes (research, no code copied): what each had to patch around go-webdav (getctag and `supported-report-set`, privileges, sync-collection, body limits), the Proton card-to-iCalendar merge and the one-object-per-UID rule, tolerance of undecryptable events. Rewritten with our own tests (`internal/caldav`, `internal/ical`, `internal/proton/cards`).
- **jmdlab/cal-gateway** (MIT), **cheeseandcereal/proton-cal** (Unlicense), **develonrails/carbonate** (MIT) and **WentTheFox/caloxide** (MIT), for the M4 write design (research `spec/experiments/raw/m4-research.md`, citations by file and line; no code copied, every rule rewritten with our own tests):
  - cal-gateway's alarm rules (`internal/proton/notifications.go`, which it says it ported from the web client): EMAIL to an email notification, DISPLAY and AUDIO to a device one, relative-to-start triggers only, nothing after the start except on all-day events, Proton's per-unit bounds, at most 10, and the tri-state of `Notifications` on update (`internal/ical/alarms_write.go`, `internal/service/writeplan.go`); also its refusals of floating times and `RANGE=THISANDFUTURE`, its RRULE INTERVAL bounds, its mirroring of a full PUT onto master and exception rows, and its rule that a refusal must be an honest 403, never a 2xx no-op;
  - proton-cal's RRULE limits (`docs/api.md`: FREQ, COUNT ≤ 49, UNTIL ≤ 2037-12-31, COUNT and UNTIL exclusive, MONTHLY and YEARLY forms; `internal/ical/validate.go`), its documentation of the sync endpoint, the property routing table, the fragment form, the sealing of each part, session-key reuse on update and full-replace semantics (`docs/api.md`, `docs/crypto.md`; `internal/proton/cards`, `internal/proton/events`, `internal/ical/write.go`), and its patch-in-place update;
  - carbonate's and caloxide's live notes on Proton's refusals (signing with the member's address key, bare `SEQUENCE`, RECURRENCE-ID with RRULE), and caloxide's member permission bits (`Write = 8`, `internal/proton/calendar`).
- The other prior art listed in spec §20: design references only; nothing from them is used in this repository so far.
