# proton-calendar-bridge

Private. The calendar counterpart of Proton Mail Bridge: a local
daemon that logs into Proton, decrypts Proton Calendar (end-to-end encrypted,
undocumented API) and serves it through standard and agent-friendly adapters.

Shape (built through M5, see Status):

- **Core** (Go): accounts and login (as unattended as Proton allows), sync
  engine on Proton's change feed, encrypted local cache, access policy, and an
  iCalendar-native event model (decision 0012; JSCalendar deferred).
- **Adapters**: CalDAV (Thunderbird first; read and write) and a CLI; REST and
  MCP adapters are out of scope for now (decision 0007).
- **Runs** headless (systemd / Docker) or on a desktop, same daemon.
- **Platforms**: Linux amd64 and arm64 are supported. macOS and Windows only build (`GOOS=darwin` and `GOOS=windows` compile `./cmd/pcb`), are not tested, and have no service management; off Unix there is no file lock and no control socket, so `pcb login`, `pcb sync` and `pcb serve` exit 20 on Windows (`run pcb on Linux or macOS`) and the CLI otherwise runs offline on the files (`spec/SPEC.md` §13, "Platforms").

Reference material, not copied wholesale: `cheeseandcereal/proton-cal` and the
`Barben360/proton-cal` fork (reverse-engineered API docs, login and policy
work, audit findings), plus the community bridges carbonate, cal-gateway and
caloxide.

## Status

Milestone M5 (daemon and ops) is built, and the initial-version audit (2026-11-12, `spec/audits/2026-11-12-initial-version.md`) is fixed or parked: `pcb login` (password, TOTP,
CAPTCHA resume, opt-in unattended relogin), `pcb logout`, `pcb status`,
`pcb calendar list`, `pcb sync`, `pcb event list`, `pcb client` and
`pcb serve` (CalDAV for Thunderbird, reads and writes) are built and tested
against a fake Proton only, and M5 adds the daemon around them: a control
socket behind a live `pcb status` and `pcb doctor`, `pcb serve --wait-for-login`,
`pcb install`, `uninstall` and `service` (systemd, user or system), a container image,
and CalDAV passwords you can show again (`pcb client show`). Both smoke tests (the image,
and a Debian container with systemd as PID 1) passed locally, and CI publishes the image from main (below). The live sessions against the test account (M1, M2 for the change feed, step 6
for Thunderbird and the M4 writes, then step 7 for the M5 daemon, service and container) are deferred until the author can solve a
CAPTCHA (`spec/HUMAN-CHECKS.md`, "M1 live session", "M2 live session", steps 6 and 7); until then the cache, sync, write and daemon rules rest on research and tests, not on Proton itself. Do not point pcb at an account you care about before steps 6 and 7 have run. The design is in
`spec/SPEC.md`.

## Usage

```sh
pcb login                              # prompts for what is missing; or set PCB_USERNAME, PCB_PASSWORD, PCB_TOTP
pcb login --username alice             # the username may be a flag (default: $PCB_USERNAME, else asked)
pcb login --password-fd 3 3<pwfile     # the password never goes in a flag
pcb login --unattended-relogin         # store the credentials so pcb can log in again without you
pcb login resume --captcha-token T     # after solving the CAPTCHA Proton asked for
pcb status                             # the login; with pcb serve running, its live state too (listener, per-calendar sync and write gate, alerts)
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
```

Secrets come from a terminal prompt, `--password-fd`, or the environment (`PCB_USERNAME`, `PCB_PASSWORD`, `PCB_TOTP`: a code or an `otpauth://` URI; `scripts/pcb.env.example`), never from a flag. When `pcb serve` runs, `pcb login` goes through it, over a control socket only its own user can open; a login that waits for its 2FA code stays in the daemon for 5 minutes, and `PCB_TOTP=<code> pcb login resume --totp` finishes it (the code is never an argument). With no daemon, `pcb login` says so and logs in on its own. `--unattended-relogin` seals the password (and, with 2FA, `PCB_TOTP` as an `otpauth://` URI: a one-time code is refused) so a session Proton ends is repaired by one automatic login, at most one per 60 s; without it a login stores tokens only. `pcb login resume` takes the same flags. `--non-interactive` never prompts: it exits 10 naming what is missing (or the CAPTCHA URL and the `pcb login resume` command to run). A login that hits a CAPTCHA while the stored session is still working leaves that session as it was. Each Proton call of a login has a 30 s deadline (exit 20, run the command again), and a Proton 429 names the wait (`Retry-After`).

Exit codes (`--json` has the same ones): `0` done, `2` bad usage, `10` you must act (a login, a lost or unreadable key file, a `secrets.sealed` or `state.json` written by a newer pcb: upgrade pcb), `20` transient or no daemon (retry; systemd does not restart on 10). The same failure has the same code in every command. A `--json` run prints one envelope on stdout; it can carry `data` and `error` together (a CAPTCHA puts `captcha_token` and `captcha_url` in `data`; a partial sync its counts). Warnings from the code that runs a command (a clock set back, a client that could not be created) go to stderr as `Warning: …`, also with `--json`.

`pcb logout` revokes the session. When the stored secrets were written by a newer pcb or another sealer, pcb cannot revoke it: it says so, asks you to end the session in the Proton web app (Settings, Security, Sessions) and then to type `discard` (or run `pcb logout --yes`) to remove the local files. If another process is syncing, the logout finishes and tells you to run it again once that process stops.

CalDAV clients (Thunderbird, a phone) sign in with an app password, not your Proton one. A `pcb login` that completes while no client exists creates one named `default` (also after you removed them all: a new password); `pcb client show` prints it again whenever you need it, as Proton Mail Bridge shows its bridge password (decision 0013). `pcb client add NAME` makes another for one more device, each separately revocable. Each password is 24 random bytes (base64url) kept in `clients.json` (0600) in the state directory twice: as a salted SHA-256 for the check, and sealed with the key that seals your Proton session, so whoever can read both the state directory and that key can read these passwords (the same access already exposes the Proton session). `show` prints `URL:`, `Username:` (the client's name; any username is accepted), `Password:` and, over TLS, the certificate's SHA-256 fingerprint; `--json` leaves the password out unless you add `--reveal`. Clients created before M5 keep only their hash: `show` says so and names `pcb client rotate NAME`. Five wrong passwords from one IP in a minute are refused until the minute ends. `pcb logout` keeps the clients and the key; `pcb uninstall --delete-data` removes them.

Notes:
- A pcb from before M5 refuses the version 2 `clients.json` M5 writes: do not downgrade.
- Without a running daemon, `pcb client show` builds the URL from `config.toml`, so it ignores a `pcb serve --listen` override; with one running it prints the daemon's own listener.
- With no key file but with `secrets.sealed` present, `pcb client add` and `pcb client rotate` refuse (exit 10): minting a new key would make the stored session unreadable. Restore the key file, or run `pcb logout` then `pcb login`.

## Serve to Thunderbird

```sh
pcb login                    # a login with no client yet also creates the CalDAV client "default"
pcb serve                    # prints the URL to enter, e.g. "Thunderbird URL: http://localhost:1080/"
pcb client show              # in another terminal: URL, username, password and, over TLS, the fingerprint
```

In Thunderbird: New Calendar > On the Network, format CalDAV, the URL of `pcb client show` as Location, its username (`default`) and its
password (the live session against the bridge is `spec/HUMAN-CHECKS.md` step 6, not run yet). `pcb serve` keeps the cache in sync (a pass every 30 s, every 5 min once nobody has used the bridge for 35 minutes, and at once when a client asks again) and serves it, with writes, until Ctrl-C
(SIGINT or SIGTERM: requests in flight get 5 s, exit 0; a second signal exits 20). Plain `pcb serve` refuses to start logged out (exit 10, `pcb login`),
while `pcb sync` runs or while another `pcb serve` runs (exit 20; the second says `pcb serve is already running`), and stops when Proton needs you again (exit 10). With `--wait-for-login` (what the service and the image run) it keeps
listening instead: CalDAV answers 503 with `Retry-After: 30` until a login completes, from any process, and again whenever Proton ends the session, with no restart.
Logs go to stderr, IDs only; `--log-level debug` (or `[log] level`) adds what a write normalised.
After each sync pass, while an event is served as a placeholder (it could not be decrypted) or marked
`X-PCB-INTEGRITY:unverified` (a signature did not verify), a duplicate
master is left out or a calendar's keys fail, it logs a `render counters` line (counts and calendar IDs), and once
more when they change, and `pcb status` shows them from the running daemon (`dropped_alarms` counts a malformed reminder left out of an event that is still served; a deleted event stops counting at once), with a calendar's write gate and its reason and the alerts
(`needs-login`, `needs-captcha`, `human-verification`, `sync-stalled`, `calendar-keys-failed`, `write-gate-undecided`,
`clients-file-unreadable`, `tls-certificate-expiring`, `tls-certificate-expired`), each with the fix; `pcb status` exits 0 whatever the alerts.

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

The unit runs `pcb serve --wait-for-login`: until you log in, CalDAV answers 503; after a reboot or a restart it
serves again by itself. `pcb install` is idempotent (a second run keeps every file, and never replaces an edited `config.toml`);
edit `[caldav]` (`listen`, `hostnames`) in `~/.config/pcb/config.toml`, then `pcb service restart`.

For a server: `sudo pcb install --system` creates the `pcb` user, uses `/etc/pcb` and `/var/lib/pcb`, copies the binary to
`/usr/local/bin/pcb` and installs the fully hardened system unit. The control socket answers only the daemon's own user, so run the
CLI as that user, as `pcb install` prints it: `sudo -u pcb env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb /usr/local/bin/pcb login`
(and `... client show`). `pcb service start|stop|restart` needs root with `--system`; `status` and `logs` do not. A command that opens the state, run as root against a state directory owned by `pcb`, refuses (exit 10, `sudo -u pcb pcb ...`) rather than leave root-owned files the service cannot read. `pcb install` refuses a binary or home path with a space, `%`, `$` or a non-ASCII letter (exit 10, also under `--plan`): systemd would split or expand it.

`pcb uninstall` stops the service and removes the unit, keeping your data. `pcb uninstall --delete-data` (it asks, or `--yes`) first
logs out, which revokes the Proton session, then deletes the config and state directories (and with `--system` the `pcb` user); it
removes only a directory named `pcb`, and keeps and names a key file kept elsewhere. It leaves `/usr/local/bin/pcb` and prints the `rm` line.
`--plan` shows the steps without running them. The live run on a fresh host is `spec/HUMAN-CHECKS.md` step 7.1, not run yet.

## Run in a container

The image (distroless, no shell, UID 65532, one volume `/data`) runs `pcb serve --wait-for-login --listen 0.0.0.0:1080`: it serves
**self-signed TLS** on 1080, comes up healthy with no login, and CalDAV answers 503 until you log in. Its health check is
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
dialog (it shows it once and you confirm only an equal one). The image is smoke-tested (`make container-smoke`: healthy with no login, 401 and 503
over TLS, UID 65532, healthy after `docker kill`); the compose file above, the compose secret and `config` mount, and a login in the
container are `spec/HUMAN-CHECKS.md` step 7.2, not run yet. Five wrong passwords from one IP in a minute lock that IP for the minute:
Docker's port publishing keeps the client's source IP, but with the userland proxy (rootless Docker, some setups) every client shares
one IP, so one device's typos lock them all for that minute (`spec/OPEN.md`).

**When the container stops or restarts in a loop.** `docker logs pcb` ends with the error and its hint. Exit 10 is a fault you
must repair (a lost or unreadable key, a damaged cache, an unreadable `state.json`, a bad `config.toml`), exit 20 one that may pass
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
make lint test    # gofmt, go vet, golangci-lint, go test -race, on JOBS=4 CPUs
make install      # bin/pcb -> $(PREFIX)/bin/pcb, PREFIX ?= ~/.local
make image        # the container image, pcb:local
make container-smoke systemd-smoke   # throwaway containers; each prints PASS last
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
- **WentTheFox/caloxide** (MIT), for the M5 idle cadence: its standby idea, slowing the poll when nobody uses the bridge (idea audited from the M2 research, no code copied). Rewritten with our own definition of idle (no authenticated request for 35 minutes, longer than Thunderbird's 30-minute refresh), a wake on the first request, and our own tests (`internal/service`, `internal/syncer`).
- **Proton Mail Bridge** (and the author's headless Bridge container), for the M5 container and credential model: one `/data` volume, login with `docker exec -it`, state kept in the volume, and a bridge password the user can look up again (decision 0013, `pcb client show`). The usage model only: ideas, no code read or copied; our own image, units and tests (`Dockerfile`, `internal/install`, `internal/ctl`).
- The other prior art listed in spec §20: design references only; nothing from them is used in this repository so far.
