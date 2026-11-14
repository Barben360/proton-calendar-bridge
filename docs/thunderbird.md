# Thunderbird

How to serve your Proton calendars to Thunderbird (or another CalDAV client, such as a phone): connecting, CalDAV app passwords, several accounts, what happens to your edits, and the listener's settings.

## Serve and connect

```sh
pcb login                    # a login with no client yet also creates the CalDAV client "default"
pcb serve                    # prints the URL to enter per account, e.g. "Thunderbird URL (alice@proton.me): http://localhost:1080/dav/1a2b3c4d/"
pcb client show              # in another terminal: URL, username, password and, over TLS, the fingerprint
```

With the [service](service.md), the [container](container.md) or the [desktop app](desktop.md), pcb already serves: skip `pcb serve`.

### Connect Thunderbird

In Thunderbird: New Calendar, On the Network, then:

| Field | Value |
|---|---|
| Format | CalDAV |
| Location | the URL of `pcb client show`: `/dav/<local-id>/`, one per account |
| Username | the one `pcb client show` prints: the account's address (any username is accepted) |
| Password | the one `pcb client show` prints |
| Offline support | checked (the default in the New Calendar dialog). See [why](#when-thunderbird-disables-a-calendar). |

Over TLS, Thunderbird shows a certificate exception dialog on first use: confirm it only when its SHA-256 fingerprint is the one pcb printed ([the certificate](#tls-and-the-certificate)).

How it was tested: Thunderbird was run live against the bridge on 2026-11-13 ([status](status.md)).

## CalDAV app passwords

CalDAV clients sign in with an app password, not your Proton one.

```sh
pcb client show [NAME] [--reveal]   # URL, username, password and TLS fingerprint of a client
pcb client add NAME                 # another app password, one per device; works logged out
pcb client rotate NAME              # a new password; the old one stops working at once
pcb client list                     # the clients, never their passwords
pcb client remove NAME|ID           # its password stops working at once, even for a running pcb serve
```

- A `pcb login` that completes while no client exists creates one named `default` (also after you removed them all: a new password).
- `pcb client show` prints it again whenever you need it, as Proton Mail Bridge shows its bridge password. `NAME` defaults to `default`, else the only client; with several clients and none named `default` it exits 10.
- `show` prints `URL:` (the account's own, `.../dav/<local-id>/`), `Username:` (the account's Proton address; any username is accepted), `Password:` and, over TLS, the certificate's SHA-256 fingerprint.
- `--json` leaves the password out unless you add `--reveal`.
- Each device can have its own password (`pcb client add`), each separately revocable.
- Clients created by a version from before passwords could be shown again keep only their hash: `show` says so and names `pcb client rotate NAME`.
- Without a running daemon, `pcb client show` builds the URL from `config.toml`, so it ignores a `pcb serve --listen` override; with one running it prints the daemon's own listener.
- `pcb logout` keeps the clients and the key; `pcb uninstall --delete-data` removes them.
- Five wrong passwords from one IP in a minute are refused until the minute ends.

How the passwords are stored: [security](security.md#caldav-passwords). Why they can be shown again: [viewable CalDAV credentials](../spec/decisions/0013-viewable-caldav-credentials.md).

## What `pcb serve` does

```sh
pcb serve [--listen HOST:PORT] [--wait-for-login] [--log-level LEVEL]
```

It runs the sync loop, the CalDAV listener and the control socket until Ctrl-C, and serves the cache, with writes.

**Sync cadence.** A pass every 30 s; every 5 min once nobody has used the bridge for 35 minutes; and at once when a client asks again.

**Stopping.** SIGINT or SIGTERM: requests in flight get 5 s, then exit 0. A second signal exits 20.

**When it refuses to start or stops.**

| Situation | Plain `pcb serve` |
|---|---|
| logged out | refuses to start, exit 10 (`pcb login`) |
| `pcb sync` runs | refuses, exit 20 |
| another `pcb serve` runs | refuses, exit 20: `pcb is already serving (pid N, kind K)`, K being `serve`, `ui` or `mcp` |
| Proton needs you again | stops, exit 10 |
| every account has stopped | stops, with the first one's exit code |

### Keep serving while logged out: `--wait-for-login`

With `--wait-for-login` (what the service and the image run), `pcb serve` keeps listening instead:

- with no account, it waits for the first login. No CalDAV client exists before one, so CalDAV answers 401;
- an account waiting for a login, from any process, has its CalDAV answer 503 with `Retry-After: 30` until the login completes (again whenever Proton ends the session), with no restart;
- it never stops for an account.

### Logs

Logs go to stderr, IDs only. `--log-level debug` (or `[log] level` in `config.toml`) adds what a write normalised.

## Several accounts

`pcb login` with another username adds an account to the same process: one CalDAV listener, one control socket, one URL per account.

- Each account has its own CalDAV passwords, its own cache and its own sync loop, so one account's CAPTCHA, damaged cache or expired session never stops another's.
- A stopped account shows an `account-stopped` alert, answers 503, and starts again with `pcb account restart` or a login.
- Use the per-account URL in Thunderbird. The older URL (`/`, `/dav/calendars/me/...`) still works, the password alone choosing the account, so an upgraded install keeps syncing with no change.
- Two accounts in one Thunderbird profile have distinct realms (`pcb <local-id>`) and paths, so its HTTP auth cache should keep their passwords apart. That is not checked live yet ([status](status.md#still-to-check-live)).
- Wrong passwords count against one limiter shared by all accounts.

Per-account switches: `pcb account caldav off` refuses an account's CalDAV clients ([commands](commands.md)).

## Status, counters and alerts

```sh
pcb status
```

With `pcb serve` running, `pcb status` shows its live state: the listener, each calendar's sync and write gate (with its reason), the render counters and the alerts. It exits 0 whatever the alerts.

**Render counters.** After each sync pass, while any of these holds, `pcb serve` logs a `render counters` line (counts and calendar IDs), and once more when they change:

- an event is served as a placeholder (it could not be decrypted);
- an event is marked `X-PCB-INTEGRITY:unverified` (a signature did not verify);
- a duplicate master is left out;
- a calendar's keys fail.

`dropped_alarms` counts a malformed reminder left out of an event that is still served. A deleted event stops counting at once.

**Alerts**, each shown with its fix:

`account-stopped`, `account-unreadable`, `needs-login`, `needs-captcha`, `human-verification`, `sync-stalled`, `calendar-keys-failed`, `write-gate-undecided`, `clients-file-unreadable`, `tls-certificate-expiring`, `tls-certificate-expired`.

The keyring alerts `keyring-locked` and `keyring-unavailable` are in [security](security.md#the-desktop-keyring-secret-service).

## Writes from Thunderbird

Creating, editing, moving and deleting events, single occurrences included, go to Proton while pcb serves:

- each change is one Proton write call, read back before Thunderbird gets its answer;
- it is never retried once it may have reached Proton. The one exception: it is resent once when Proton answers 401 for an expired token before processing it, which refreshes the session.

### What Proton changes

Thunderbird shows what Proton stored, which can differ from what it sent. The full table is in the [property mapping](../spec/mapping.md).

| You set in Thunderbird | What Proton keeps |
|---|---|
| alarms | Proton device notifications (before or at the start, at most 10); others are dropped, and alarm-level properties such as Thunderbird's per-alarm acknowledgement are lost |
| email notifications | they stay Proton-side: Proton sends them, pcb does not show them to the client, and your edits keep them (set them in the Proton web app) |
| `STATUS:TENTATIVE` | dropped (Proton refuses it) |
| a colour | ignored (Proton's colour stays) |
| `DURATION` | becomes an end time |
| "monthly on the 2nd Tuesday", "every weekday" | written in Proton's equivalent form |
| unknown properties (Thunderbird's `X-MOZ-*` and others) | kept, encrypted |
| changes to an existing attendee's `ROLE`, `CN`, `PARTSTAT` or `RSVP` | ignored, and still answered as saved (204): Proton keeps the attendee's stored line, so a participation status you change in Thunderbird shows its old value again at the next read. Change it in the Proton web app. |

### Writes pcb refuses

Writes are refused (HTTP 403) when pcb cannot store them faithfully or safely:

- **Calendars you may not write:** holidays and subscribed calendars, calendars where your member lacks write permission, calendars whose key signature does not check out. `pcb calendar list` shows them `read_only`.
- **Attendees:** adding or removing attendees, or an attendee on a new event. Invitations are not supported, and **pcb never notifies attendees** in v1: moving an occurrence of a meeting keeps its attendees but sends them nothing.
- **Recurrence rules Proton cannot hold:** hourly or finer, a count above 49, an end after 2037-12-31, `RDATE`, `EXRULE`, "this and all future occurrences" sent as one change, and the other forms in the recurrence section of the [property mapping](../spec/mapping.md).
- **A series whose start is not one of its own occurrences** (Proton would drop that start, Thunderbird show it): refused when a write creates one or changes the start or rule of an existing one; other edits pass. Put the start on one of the rule's days in that same edit, or delete the series and create it again.
- **Times:** floating times (no time zone), and time zones Go's database does not know.
- **Events pcb cannot rewrite safely:** one it cannot decrypt or verify, one written by another person, someone else's invitation, one whose key material changed since pcb first saw it.
- **Per-user data pcb cannot rewrite yet** (`pcb:personal-part`, typically alarms stored by older Proton clients): edits are refused, edit it in the Proton web app; deleting it works.

`pcb serve` logs each refused write at warning level: `refused a CalDAV write`, with its code, calendar, href and reason, never the event's content. So a change Thunderbird keeps retrying silently shows up there. The same code and href is logged at most once per 10 minutes; the next line after that carries `repeated=N`, the refusals left out.

### When Thunderbird disables a calendar

For a calendar without offline support, Thunderbird shows a generic error after a refused write and then **disables the calendar** until you enable it again. With offline support it keeps the change locally and retries it silently.

Subscribe to pcb calendars with "Offline support" checked: the default in the New Calendar dialog, otherwise in the calendar's properties. To recover a disabled calendar, enable it again in its properties, and look for the `refused a CalDAV write` line to see why.

### 503 and 412 answers

- **503, Thunderbird tries again later:** Proton is unavailable, rate limiting, or asking for a human verification (`pcb serve` logs it: open the Proton web app from the same network and complete it), or a change cannot be confirmed by reading it back. pcb's cache then holds whatever Proton holds, which Thunderbird sees at its next sync.
- **412, Thunderbird offers its conflict dialog:** someone changed the same event elsewhere in the meantime.

## The CalDAV listener

The listener comes from `[caldav]` in `config.toml`, or from `pcb serve --listen`:

```toml
[caldav]
listen = "0.0.0.0:8443"        # default 127.0.0.1:1080
hostnames = ["pcb.lan"]        # names clients use; the first one is in the printed URL
# tls_cert = "/etc/pcb/cert.pem" # your own certificate: both keys or neither
# tls_key = "/etc/pcb/key.pem"
```

- A loopback address serves plain HTTP; any other one serves TLS 1.2+ only. With `tls_cert` and `tls_key`, every address serves TLS.
- `0.0.0.0` listens on IPv4 only; `[::]` is dual stack.

After editing, restart pcb (`pcb service restart` for the service, "Restart bridge" in `pcb-ui`).

### TLS and the certificate

Without `tls_cert`/`tls_key`, pcb generates a self-signed certificate and reuses it:

- ECDSA P-256, valid 825 days;
- for the hostnames, `localhost` and the bind address or, on `0.0.0.0`/`[::]`, every interface address;
- in `tls/` of the state directory (directory 0700, key 0600, narrowed again at each start; files of another user are refused).

It is replaced, with a warning, when a hostname or a specific bind address is no longer covered, under 30 days remain, or the stored pair is unreadable or mismatched. A certificate of your own that has expired or is within 30 days of expiry gets a warning at start.

`pcb serve` and `pcb doctor` print its SHA-256 fingerprint. Thunderbird shows a certificate exception dialog on first use: confirm the exception only when its SHA-256 fingerprint is the printed one. Do so again after a replacement: `pcb serve` logs a warning naming the old and the new fingerprint when it replaces one.

A failed TLS handshake (a port scan) is logged at debug level only.

### Request limits

| Limit | Answer above it |
|---|---|
| a PUT body of at most 8 MiB | 413 |
| a PROPFIND or REPORT body of at most 1 MiB | 413 |
| a requested property name or namespace of at most 256 bytes | 400 |

If `clients.json` cannot be read, CalDAV answers 503 with `Retry-After: 30` (logged once) instead of refusing every password: repair or restore it.
