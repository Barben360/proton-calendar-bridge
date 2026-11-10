# proton-calendar-bridge — specification (draft v1, 2026-11-10)

Status markers used below:
- **[decided]** the author decided it (see `decisions/`).
- **[call]** the spec author's call; the author may overturn it.
- **[verified: source]** a fact checked live or in code we trust.
- **[assumed → E#]** a belief not yet checked; experiment `E#` must settle
  it before any code relies on it (§17).

## 1. Purpose and success criteria

The calendar counterpart of Proton Mail Bridge. A daemon logs into a Proton
account, keeps an up-to-date, decrypted view of Proton Calendar, and serves
it through standard and agent-friendly adapters. Proton Calendar is
end-to-end encrypted and has no public API, no CalDAV, and no official
bridge [verified: proton.me/support/subscribe-to-external-calendar,
2026-11-10].

Success means:
1. Thunderbird shows every calendar of the account, and two-way edits
   (create, move, edit one occurrence, delete) show up in the Proton web app
   within the sync interval, and back [decided: Thunderbird is the reference
   client].
2. An AI agent reads and writes the calendars it is allowed to, through MCP,
   with no access to the others [decided: agents are the long-term main
   consumer].
3. Everything, install to uninstall, is done from a terminal, including on
   a server without a display [decided].
4. An expired session repairs itself with no human, unless Proton demands a
   CAPTCHA; then one URL to solve from any device and one command resume it.
5. No silent data loss: an event that cannot be represented faithfully is
   refused or flagged, never mangled (§8.4).

## 2. Principles

- **Headless first** [decided]. One Go binary. No GUI in v1; GUIs are later
  clients of the same local API.
- **Blocks** [decided]. Each block (§4) has a narrow interface, its own
  tests, and can be built, reviewed and replaced alone.
- **No unverified assumptions** [decided]. Facts about Proton or clients come
  from a live experiment, our own tests, or code we can trust and have read.
  The spec tags each one (status markers above).
- **Inspiration, not copies** [decided]. Logic from open-source projects is
  audited and rewritten with our own tests, then credited in `README.md`
  (§20). Proton's own MIT libraries (go-proton-api, gopenpgp, go-srp) are
  used as dependencies, not rewritten.
- **Least plaintext** [call]. Disk holds what Proton's servers already hold
  (ciphertext rows, plaintext time ranges and IDs) plus sealed secrets;
  decrypted content lives in memory only (§7).
- **Good citizen** [call]. One process per account, one shared rate limiter,
  backoff on 429/5xx, no busy polling, a truthful client identity (§5.1).
  A flagged or banned Proton account is a failure even if the code works.
- **Agent-drivable CLI** (backsight conventions) [call]: `--json` envelope,
  `--plan`, `--non-interactive`, fixed exit codes (§13).

## 3. Goals, non-goals, anti-goals

Goals (v1): CalDAV read/write for owned calendars; read-only for subscribed
and holidays calendars (the author's account has 1 owned, 1 holidays, 2
subscribed and no shared calendars [verified: `calendars` listing
2026-11-10]; shared calendars are phase 3 with E10/E14 and a second test
account); a REST API documented with OpenAPI
3.1; an MCP server; a complete CLI; `.ics` read-only feeds; install, update,
doctor, uninstall as commands; Linux (amd64, arm64) and a container image.

Non-goals (v1): GUIs, CardDAV/contacts, mail, macOS/Windows service
management (the binary may run there, unsupported), FIDO2 login, sending or
answering invitations (phase 3, §18), Proton sharing management (inviting
members), booking pages, "this and following" series splits (refused, §8.4).

Anti-goals (failure even if it works):
- Plaintext calendar data or reusable credentials on disk unsealed.
- A listener reachable from the network by default.
- An agent able to read a calendar it was not granted, or to widen its own
  grants.
- Behaviour that gets the Proton account flagged (login storms, CAPTCHA
  loops, unbounded polling).
- Silent lossy round-trips (an edit in Thunderbird that drops attendees,
  alarms or exceptions in Proton).

## 4. Architecture

```
                 ┌──────────────── pcb daemon (one per account) ────────────────┐
 Thunderbird ──▶ │ A1 CalDAV ─┐                                    ┌── A2 .ics  │
 scripts/CLI ──▶ │ A3 REST ───┼──▶ C3 service (use cases, policy) ◀┼── A4 MCP   │
 AI agents ───▶  │            │        │            │              │            │
                 │            │   C4 sync engine   C5 access       │            │
                 │            │        │            policy           │            │
                 │            │   C2 store (cache, state, sealed secrets)        │
                 │            │        │                                         │
                 │            └── C1 proton (transport, auth, keys, calendar API,│
                 │                      change feed, codecs: model ⇄ Proton)     │
                 └──────────────────────────────────────────────────────────────┘
 CLI (A5) talks to the daemon over the control socket (A3 on a unix socket).
```

Binary name: `pcb` [call; short, no "proton" in it, rename-proof].

Blocks and their contracts:

| Block | Owns | Exposes | Depends on |
|---|---|---|---|
| C0 model | Internal event model (JSCalendar, RFC 8984), iCalendar codec (RFC 5545), recurrence expansion, time zones | Go types + pure functions | stdlib, a vetted RRULE lib or our own |
| C1 proton | HTTP transport, client identity, rate limit, auth (SRP, 2FA, human verification, two-password, scopes), key hierarchy, calendar endpoints, change feed, Proton card codec (model ⇄ encrypted parts) | `proton.Session`, `proton.Calendars`, `proton.Feed` interfaces | go-proton-api, gopenpgp, go-srp, C0 |
| C2 store | SQLite cache of ciphertext rows and sync state; sealed secret store | `store.Cache`, `store.Secrets` | modernc.org/sqlite (pure Go) [call], C0 |
| C3 service | Use cases: list/get/search/create/update/delete events, calendars, free-busy (local), login flows; applies C5 | `service.API` (Go interface, the only thing adapters call) | C1, C2, C4, C5 |
| C4 sync | Initial and incremental sync, write queue, conflict detection, backoff | `sync.Engine` | C1, C2 |
| C5 policy | Clients (CalDAV users, API tokens, MCP agents), per-client calendar grants, disabled operations, audit log | `policy.Decide(client, op, calendar)` | C2 |
| C6 daemon | Process lifecycle, listeners, config, logs, metrics, health | `pcb serve` | all |
| A1–A5 | Adapters (§9–§12) | protocols | C3 only |

Rule: adapters never import C1, C2 or C4; they only call `service.API`. A
second account is a second `service` instance in the same daemon [call;
multi-account in v1 data model, one account tested in v1].

## 5. C1 — Proton block

Reference for endpoints and crypto: proton-cal `docs/` (api.md, crypto.md,
overview.md; live-verified by its author) and our own experiments. Every
endpoint we use gets a recorded, redacted fixture and a contract test (§16).

### 5.1 Transport and identity
- Base URL `https://mail-api.proton.me` [verified: proton-cal docs/crypto.md].
- Client identity header `x-pm-appversion`: which values Proton accepts and
  which scopes each yields is not settled. proton-cal uses `Other`, carbonate
  reports a `linux-mail@…` value working, caloxide reports an `external-drive`
  value losing calendar scope [assumed → E03]. The value is configuration,
  not code, with a tested default.
- One token-bucket rate limiter per account, shared by all callers; honour
  `Retry-After`; exponential backoff with jitter on 429/5xx; a circuit
  breaker that pauses sync and raises a status alert after repeated
  failures [call; Proton's limits → E06].

### 5.2 Authentication
State machine (persisted in C2):

```
logged-out ──login──▶ srp ──▶ [human-verification] ──▶ [2fa] ──▶ [mailbox-password] ──▶ ready
ready ──refresh fails (auth)──▶ relogin (unattended, if allowed) ──▶ ready | needs-human
needs-human: needs-captcha(url, token) | needs-2fa-code | needs-password | locked
```

- SRP via go-srp; TOTP; two-password mode; FIDO2 refused with a clear
  message [verified: proton-cal pkg/auth].
- Human verification (CAPTCHA): the challenge token can be solved on any
  device at `https://verify.proton.me/?methods=captcha&token=<token>` and the
  login retried with it [verified: interactive login 2026-11-10]. Whether the
  solved token survives across processes and for how long [assumed → E02];
  the design relies on it for `pcb login resume --captcha-token`.
- Unattended relogin [call, opt-in at login]: stores the login password and
  the TOTP seed (otpauth URI) sealed (§7.3), so an expired session is
  repaired without a human. Off by default; `doctor` says what it implies
  (whoever unseals the store owns the account).
- Session lifetime and refresh-token rotation rules [assumed → E01]: decide
  how often the daemon refreshes and when it must relogin.
- Locked ("password") scope for salts and calendar deletion via SRP to
  `/core/v4/users/unlock` [verified: proton-cal docs/overview.md].
- Admin channel [call]: anything that changes access or identity (login,
  grants, clients, config) needs an admin credential: the control socket
  checks the peer UID (SO_PEERCRED) **and** an admin token sealed in C2 that
  the CLI reads; grant changes additionally need a confirmation typed at a
  terminal unless `--non-interactive` comes with the admin token in a file
  descriptor. Agents never use the control socket: `pcb mcp stdio` connects
  with the agent's own client token. Rationale: an agent may run as the
  same OS user (§19).
- Credential inputs, in priority order: `--answer`/stdin for non-secrets;
  secrets only from a terminal prompt, a file descriptor (`--password-fd`),
  or environment (`PCB_PASSWORD`, `PCB_TOTP`, … for `pass-cli run`); never
  argv [call].

### 5.3 Keys
User keys → address keys → calendar passphrase (per member) → calendar key;
per-event session keys for shared and calendar parts [verified: proton-cal
docs/crypto.md, Proton security-model blog]. Unlocked keys live in memory
only; the salted key passphrase is sealed in C2.

Signature policy [call]: verify every signed part; a bad or missing
signature marks the event `integrity: unverified` (visible in REST/MCP as a
field, in CalDAV as `X-PCB-INTEGRITY`), never silently accepted or dropped.
Which parts are signed by whom, and whether shared-calendar parts verify
with the organiser's address key [assumed → E10].

### 5.4 Calendar API surface (v1)
- Calendars: list, bootstrap (keys, passphrase, settings), user settings
  (default calendar), create/update/delete owned calendars, leave holidays
  [verified: proton-cal docs/api.md].
- Events: windowed query (4 `Type` buckets, ≤ 93-day windows, `More` cursor)
  [verified: proton-cal docs/api.md]; unscoped full listing paged with
  `Total` (no `More`) [verified: live probe 2026-11-10]; get by ID (422/2501
  for an ID of another calendar [verified: live 2026-11-10]); get by UID;
  writes through `PUT /calendar/v1/{cal}/events/sync` only [verified:
  proton-cal docs/api.md].
- Member permissions and calendar type (normal 0, subscribed 1, holidays 2)
  [verified for types: proton-cal]; member `Permissions` bit values for
  view-only shares [assumed → E14].

### 5.5 Change feed
Goal: incremental sync without re-listing. Candidates:
- Calendar model events `/calendar/v1/{cal}/modelevents/latest` and
  `/modelevents/{eventID}` (used by carbonate and caloxide, polled) and the
  core event loop `/core/v4/events/{id}` for calendar list/membership
  changes [assumed → E04, E05].
- Push stream: reported 404 by caloxide [assumed → E04 checks it too].
Fallback if the feed proves unreliable: periodic windowed re-list of a
rolling window plus a nightly full list [call].

### 5.6 Proton card codec
Model ⇄ Proton parts (shared signed, shared encrypted, calendar signed,
calendar encrypted, attendee parts, per-member personal notifications).
Which iCalendar properties go in which part, and what Proton keeps or drops
on unknown properties [verified for the parts proton-cal writes; unknown
properties assumed → E13]. The codec has a round-trip property test: model
→ parts → model is identity for every supported field.

## 6. C0 — Model block

- Internal model: JSCalendar (RFC 8984) `Event` with `recurrenceRules`,
  `recurrenceOverrides`, `excludedRecurrenceRules`, `participants`, `alerts`,
  `timeZone`, `locations`, `virtualLocations`, `color`, plus a
  `proton` extension object (IDs, calendar ID, member ID, part signatures,
  integrity, sequence, raw unknown iCal properties) [decided: JSCalendar;
  call: the extension's content].
- Codecs: iCalendar ⇄ model (RFC 5545 + RFC 7986 color), JSCalendar JSON
  (the REST/MCP wire format, RFC 8984 + our extension).
- Recurrence: expansion for time-range queries and free-busy; library choice
  after an audit of candidates (teambition/rrule-go used by proton-cal) or
  our own on the RFC 5545 rules [call; audit → E16].
- Time zones: IANA names; VTIMEZONE generated from tzdata for CalDAV output
  (Thunderbird's need for it [assumed → E11]); floating and all-day handled
  as in RFC 8984.
- Fuzz tests on every parser; golden files from real Proton, Thunderbird and
  Apple exports.

## 7. C2 — Store block

### 7.1 Layout
`$XDG_STATE_HOME/pcb/` (or `/var/lib/pcb/` for a system service):
`pcb.db` (SQLite, WAL), `secrets.sealed`, `logs/`. Config in
`$XDG_CONFIG_HOME/pcb/config.toml` (or `/etc/pcb/`). Directories 0700,
files 0600; `doctor` checks them.

### 7.2 What is cached [call]
- Calendar list, members, settings, keys as Proton returns them (encrypted
  key material).
- Event rows as Proton returns them (ciphertext parts + plaintext IDs,
  UID, start/end, recurrence ID, modify time).
- Sync state: feed cursors, per-calendar sync token, per-event ETag.
- Nothing decrypted. Decrypted events live in an in-memory LRU, rebuilt on
  demand. Cold start = decrypt on first access; cost per 1 000 events
  [assumed → E09] decides whether we need a sealed decrypted index later.

### 7.3 Secrets
Sealed with a data key held by, in order of preference [call]:
1. systemd credentials (`systemd-creds`, TPM2 when present) for services;
2. the Secret Service (desktop keyring) when a session bus has one;
3. a key file 0600 on a separate path (headless fallback, `doctor` warns).
Availability on a headless Debian/morpheus-like host [assumed → E15].
Sealed content: session tokens, salted key passphrase, optional unattended
relogin credentials, client tokens (hashed, never stored in clear).

## 8. C3/C4 — Service and sync

### 8.1 Sync engine
- Initial sync: list calendars, then each calendar's full row set (unscoped
  listing), then follow the feed.
- Incremental: poll the feed every 30 s while a client is active, 5 min
  idle [call; tuned after E06]; apply deltas to the cache; bump sync tokens.
- Reads are served from the cache; a read never waits for Proton unless the
  calendar was never synced.
- Writes go to Proton synchronously (the client gets the real result), then
  the cache is updated from Proton's answer [call; no offline write queue
  in v1].

### 8.2 ETags and conflicts
ETag = hash of the row's ciphertext parts + modify time [call; what changes
on an edit → E07]. Writes carry `If-Match`; a mismatch returns 412 and a
fresh copy, never a blind overwrite.

### 8.3 Free-busy
Computed locally from cached own calendars (respecting `TRANSP`) [call];
other users' free-busy is phase 3.

### 8.4 Fidelity rules
A mapping table (`spec/mapping.md`, written during block C0/C1) lists every
iCalendar/JSCalendar property as: preserved, transformed, stripped with a
warning, or refused. Refused writes (CalDAV 403 with a precondition, REST
422 with a reason) [call]:
- "this and following" splits (RANGE=THISANDFUTURE);
- attendee changes that would need invitations (until phase 3);
- writes to read-only calendars;
- recurrence forms Proton cannot store [which → E12].

## 9. A1 — CalDAV adapter

- RFC 4791 core (MKCALENDAR off in v1 [call]), RFC 6764 discovery
  (`/.well-known/caldav`), RFC 6578 sync-collection, ctag, ETags, REPORT
  calendar-query / calendar-multiget / free-busy-query, RFC 6638 scheduling
  advertised only if a client needs it (Apple does per cal-gateway; to
  re-verify) [call].
- Thunderbird is the reference client: its exact requests and failure modes
  [assumed → E08], recorded as replayable traces used in our tests.
- Library: emersion/go-webdav after an audit (carbonate and caloxide had to
  patch its replies) or our own handlers on its XML types [call; audit →
  E17].
- Auth: HTTP Basic with per-client app passwords generated by `pcb client
  add` (like Mail Bridge's bridge password) [call].
- Listener: `127.0.0.1:1080` by default [call]; TLS (self-signed generated
  or user certificate) required for any non-loopback bind.

## 10. A2 — `.ics` feeds

Read-only `GET /ics/<token>/<calendar>.ics` per granted calendar, token per
client, for clients that only subscribe. Same listener as REST [call].

## 11. A3 — REST API

- OpenAPI 3.1 document in `api/openapi.yaml`, source of truth; server stubs
  and the CLI client generated from it (oapi-codegen or ogen after a short
  comparison [call → E18]); the document is served at `/v1/openapi.yaml`.
- Resources: `/v1/accounts`, `/v1/accounts/{id}/login` (start, answer:
  2FA, captcha, mailbox password), `/v1/calendars`, `/v1/calendars/{id}/events`
  (JSCalendar JSON, time-range and text search), `/v1/events/{id}`
  (occurrence addressing by recurrence ID), `/v1/freebusy`, `/v1/clients`,
  `/v1/status`, `/v1/health`.
- Errors: RFC 9457 problem details.
- Auth: bearer tokens per client (§12 shares them); the control socket
  (unix, 0600) needs none.
- Concurrency: ETag/If-Match like CalDAV.

## 12. A4 — MCP adapter

- MCP over streamable HTTP (and stdio via `pcb mcp stdio` that proxies to
  the daemon) [call], with the official Go SDK
  (modelcontextprotocol/go-sdk) after audit.
- Tools map to service use cases: list calendars, search/list events in a
  range, get event, create, update (incl. one occurrence), delete, free-busy.
  Output is JSCalendar-shaped JSON plus a short text summary.
- Safety [decided: agents must not reach ungranted calendars; call: the
  rest]:
  - grants per agent client (read / write per calendar), default none;
  - destructive tools off unless granted;
  - every field that comes from outside (title, description, location,
    attendee names) is returned inside a marked untrusted-data envelope,
    and invitations from outside the account are flagged — calendar
    content is a prompt-injection channel;
  - an audit log of every agent call (client, tool, calendar, result).

## 13. A5 — CLI and ops

Conventions (as backsight) [call]: every command has `--json` (one
envelope `{schema_version, command, ok, data | error}`); state-changing
setup commands have `--plan`; `--non-interactive` never prompts and exits 10
naming what is missing; exit codes `0` done, `2` bad invocation, `10` you
must act, `20` blocked by the machine, `30` failed and rolled back, `40`
partly done and resumable.

Command tree (v1):
```
pcb install [--system|--user] [--plan]     # unit files, dirs, config; no root needed for --user
pcb uninstall [--delete-data] [--plan]
pcb update check | apply [--plan]           # signed releases, migrations, rollback on failed health
pcb doctor                                  # every check, with the fix to apply
pcb serve                                   # the daemon (what the unit runs)
pcb service start|stop|restart|status|logs
pcb account add|list|remove
pcb login [--account] [--password-fd N] [--unattended-relogin]
pcb login resume --captcha-token T | --totp CODE
pcb logout
pcb status                                  # login state, sync state, listeners, alerts
pcb calendar list|get|create|update|delete
pcb event list|get|search|create|update|delete [--occurrence] [--from --to]
pcb event export|import (.ics)
pcb client add|list|remove|grant|revoke     # CalDAV users, API tokens, MCP agents
pcb mcp stdio                               # MCP over stdio, proxied to the daemon
pcb config get|set|validate
pcb version
```
Secrets are entered only at a terminal, a file descriptor or the
environment, never argv; tokens are printed once.

While the repository is private [call]: install from source (`make
install` in a clone) or `gh release download` (authenticated) of a
CI-built release; the container image goes to a private GHCR package.
Public signed releases come when the repository goes public.

Installation (public): `install.sh` downloads a signed release (minisign, as
backsight [call]), verifies it, places the binary, then runs `pcb install`.
Services: systemd `--user` unit (with lingering instructions) or system unit
with a dedicated user [call]; container image with the same binary and
`pcb serve`.

## 14. Configuration

One TOML file, strict decoding (unknown keys and empty lists refused —
lesson from the proton-cal fork audit). Policy (grants) lives in the store
and is managed by `pcb client grant/revoke`, not hand-edited [call], so the
CLI validates every change.

## 15. Observability

Structured JSON logs (no event content, no secrets; IDs only), `pcb status`,
`pcb doctor`, optional Prometheus endpoint (sync lag, Proton errors, rate
limit waits, login state), and alerts as status entries (session lost,
captcha needed, sync stalled) that `doctor` and `status` surface [call].

## 16. Testing

- Unit tests per block; every test must fail when its behaviour breaks
  (checked by sabotage in review, not mutation testing by default).
- **Fake Proton**: an in-repo fake of the calendar API driven by recorded,
  redacted fixtures from the test account; the same contract tests run
  against the fake (every CI) and the live test account (nightly canary +
  on demand) so a drift in Proton shows as a failing canary.
- **CalDAV**: replayed Thunderbird traces (E08), a scripted client suite
  (python-caldav or vdirsyncer in CI [call]), and a manual Thunderbird
  checklist per release.
- **Codecs**: round-trip property tests, fuzzing, golden files.
- **Live**: only against the dedicated test account [decided]; never the
  author's real account in automated runs.
- CI: GitHub Actions, lint (golangci-lint), `go test -race`, fuzz smoke.

## 17. Experiments (assumptions to settle before the code that needs them)

| # | Question | Needed by | Method |
|---|---|---|---|
| E01 | Access/refresh token lifetimes; refresh rotation; what ends a session | C1 auth | test account: log in, refresh on a schedule, record |
| E02 | Is a solved CAPTCHA token valid in another process, and for how long? | C1 auth, CLI resume | trigger HV on the test account, solve, retry from a new process after delays |
| E03 | Accepted `x-pm-appversion` values and resulting scopes | C1 transport | try candidates, record scopes and calendar access |
| E04 | Calendar modelevents semantics (cursor, payload, gaps); push stream status | C4 sync | test account: edit events, read the feed |
| E05 | Core event loop for calendar list / membership changes | C4 sync | same |
| E06 | Rate limits and 429 behaviour | C1, C4 | controlled bursts on the test account |
| E07 | What changes in a row on edit (modify time, sequence, parts) | ETags | edit fields one at a time |
| E08 | Thunderbird's CalDAV requests (discovery, sync, writes, errors) | A1 | proxy a real Thunderbird against a reference server (Radicale) and record |
| E09 | Decrypt cost per 1 000 events; cold-start time | C2 design | benchmark on synthetic + test-account data |
| E10 | Signature verification of each part, own and shared calendars | C1 keys | verify on test-account events, incl. a shared calendar |
| E11 | Does Thunderbird need VTIMEZONE? | C0 | E08 traces |
| E12 | Recurrence forms Proton refuses or alters | C0/C1 fidelity | write each RRULE form, read back |
| E13 | Unknown iCal properties: kept, dropped or refused by Proton? | C1 codec | write X- and standard extras, read back |
| E14 | Member `Permissions` values for view-only vs edit shares | C5, A1 | share a calendar from a second test account |
| E15 | Secret sealing on a headless Linux host (systemd-creds, Secret Service) | C2 | try on morpheus-like Debian 13 |
| E16 | RRULE library audit vs our own | C0 | read, test against RFC 5545 examples |
| E17 | go-webdav audit: fit, bugs, client compatibility | A1 | read, run against E08 traces |
| E18 | OpenAPI generator choice (oapi-codegen vs ogen) | A3 | build the same small API with both |

Each experiment gets `spec/experiments/E##-<slug>.md`: question, method,
raw results (redacted), conclusion, and the decision it feeds.

## 18. Roadmap (blocks, each reviewed and merged on its own)

| Milestone | Blocks | Done when |
|---|---|---|
| M0 Foundations | **E03 first (go/no-go: a client identity that gets calendar scope, per OPEN.md Q1)**, then repo, CI, conventions, E01, E02, E15 | experiments written up; skeleton `pcb version`, `doctor` |
| M1 Proton core, read | C0 (read side), C1 auth + keys + read endpoints, C2 secrets | `pcb login` (incl. captcha resume, unattended relogin) and `pcb event list` against the test account |
| M2 Store and sync | C2 cache, C4 initial + incremental (E04–E07, E09) | cache follows edits made in the Proton web app |
| M3 CalDAV read | A1 read, E08, E11, E17 | Thunderbird shows all calendars and stays in sync |
| M4 Writes | C1 codec write path, C4 writes, A1 writes, fidelity table (E12, E13) | Thunderbird two-way edits, incl. single occurrences |
| M5 Daemon and ops | C6, A5 complete, install/update/uninstall/doctor, signed releases, container | install from scratch on a fresh Debian with only the terminal |
| M6 REST API | A3 + OpenAPI, A2 feeds, E18 | CLI fully on the REST API; OpenAPI published |
| M7 Agents | A4 MCP, C5 grants, audit log | an agent with a grant on one calendar sees only it |
| Phase 3 | invitations in/out, shared calendars (E10 for shares, E14, second test account), other users' free-busy, JMAP adapter, GUIs | later |

## 19. Security model (summary)

Assets: Proton session and key passphrase (account takeover), decrypted
events (privacy), the ability to write (integrity). Adversaries: other local
users, network peers, malicious calendar content (invites), over-reaching
agents, a compromised client token. Controls: sealed secrets (§7.3), no
plaintext cache (§7.2), loopback by default and TLS otherwise (§9), per-
client credentials and grants (§12, §14), untrusted-content envelope for
agents (§12), signature verification (§5.3), audit log, no secrets in argv
or logs. Same-user agents [call]: an agent running as the daemon's OS user could
read a key file and unseal everything, so the recommended deployment when
agents share the user's account is the system service under a dedicated
`pcb` user (§13), and `doctor` warns when the sealing key is readable by
the user agents run as. Residual risk, stated in the docs: the bridge holds the decrypted
calendar in memory, like Mail Bridge does for mail.

## 20. Prior art and credits

To audit and learn from (MIT unless noted), credited in `README.md` when a
piece of logic is reused:
- cheeseandcereal/proton-cal (Unlicense): reverse-engineered API and crypto
  docs, write path, recurrence, login; our fork Barben360/proton-cal adds
  env/TOTP login, two-step CAPTCHA, allowlists, export, and an audit list of
  pitfalls (stale caches, fail-open config, shutdown).
- develonrails/carbonate: sync-collection on modelevents, go-webdav
  compatibility fixes, invitations through the Mail API.
- jmdlab/cal-gateway: Apple client compatibility, scheduling advertising,
  VTIMEZONE generation, fidelity matrix, RSVP endpoint.
- WentTheFox/caloxide (from protoxide/ferroxide/hydroxide): unattended
  relogin, CAPTCHA page, shared rate limiter, encrypted cache.
- ProtonMail/WebClients (GPL-3.0 [verified: GitHub licence 2026-11-10]:
  read for understanding only, never copy code), go-proton-api, gopenpgp,
  go-srp, go-webdav, rrule-go (MIT, candidate dependencies),
  modelcontextprotocol/go-sdk (MIT moving to Apache-2.0, both fine for an
  MIT project as a dependency).

## 21. Open questions for the author

See `OPEN.md`.
