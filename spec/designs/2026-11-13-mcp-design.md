# MCP adapter (M8) — design

- **Date**: 2026-11-13
- **Status**: design approved by the author 2026-11-13; built in M8 on 2026-11-13 (branch `m8-mcp`; live session pending, `spec/HUMAN-CHECKS.md` step 10). Where the built code differs, the paragraph says so, marked "*Built:*" with the controller ruling (`.superpowers/sdd/2026-11-13-m8-mcp/progress.md`, summarised in SPEC §18); the code wins, and SPEC §12 describes what is built
- **Decided by**: author (choices marked [author]); the rest is the agent's call, marked [call]. Two Sonnet reviews (architecture; security and tool surface) read the proposal against the code; their findings are folded in and marked [review].
- **Amends**: decision 0007 (MCP not planned) and SPEC §12 (the note "agents never on the control socket"), via a new decision 0016; SPEC §3 (MCP in "not planned"), §4 (A4), §4.1 (the loop rule and the adapters switch of decision 0014, §5.5b), §13 (`pcb mcp`), §14 (`[mcp]`), §18 (M8).

## 1. Intent

Decision 0004 already says the long-term main consumer of the bridge is AI agents, through an MCP adapter on the core, not through CalDAV. The author now wants that adapter [author]:

- a `pcb` subcommand, usable over **stdio** (an agent on this machine starts `pcb mcp`) or **remote** (an agent on another machine reaches an HTTP endpoint);
- configurable to expose either everything an agent may sensibly do or only reading;
- **login through MCP**: an agent can bring an account up without the person running `pcb login` first;
- multi-account from the start (M6 is merged).

MCP is developed in parallel with any further desktop work [author]; M7 (`pcb-ui`) merged on main as 07496da on 2026-11-13, so M8 branches from main and builds on M7's structure (daemon kinds, `internal/ui` wizard, keyring sealer).

## 2. Goals, non-goals, anti-goals

Goals:
- Agents read calendars and events of every account they are granted: lists over a range with recurring events expanded, one event in full, text search, free/busy.
- Agents create, update and delete events when granted, with no silent loss of what they did not touch (SPEC §8.4).
- One MCP implementation behind three entries: remote `/mcp` on the daemon's listener, stdio relayed to a running daemon, stdio served by `pcb mcp` itself when no daemon runs.
- Grants per remote agent (token) and per stdio profile, by capability and account.
- Login of a new or signed-out account started from an agent on this machine, the secrets typed by the person, never by the agent.

Non-goals (not designed here):
- OAuth 2.1 / claude.ai or mobile custom connectors [author: header-capable clients only].
- Login over remote MCP, in any form; a login web page on the daemon's listener [author: "never for headless"].
- Invitations, RSVP, attendee management (SPEC §3 non-goal); calendar creation, renaming, sharing; CalDAV client management (`pcb client ...`); host operations (install, service, uninstall); `account remove`; `account caldav` and `account restart` [call, review: little agent value, host-ish].
- A `refresh`/`sync now` tool: the loop syncs every 30 s (SPEC §8.1).
- Hand-over from a `pcb mcp` session daemon to `pcb-ui` or the service (a later improvement, §5.3).
- MCP resources, prompts, sampling; only tools.

Anti-goals (failure even if it works):
- A secret in a tool argument, a tool result, an MCP log, the audit log or the agent's transcript: the Proton password, a 2FA code or secret, a CAPTCHA token in a form the agent could replay, a CalDAV password, an MCP token after its creation.
- A listener reachable beyond loopback without TLS and authentication (SPEC §3) — `/mcp` inherits the listener's TLS rule and always needs a token.
- Agent-driven login storms or CAPTCHA loops (SPEC §2 good citizen).
- An agent edit that drops alarms, exceptions or unknown properties of the event it changed.
- `pcb` gaining cgo (the SDK and everything new stay pure Go; `cmd/pcb/arch_test.go`).
- Two processes serving one state directory at once (M6/M7 `serve.lock` rule).

## 3. Constraints

- Every repository rule (`AGENTS.md`): tests that bite and come first, no secrets anywhere, live calls only on the test account, `make lint test`, logic reused from elsewhere audited and credited, nothing from ProtonMail/WebClients.
- Adapters call only `service` (SPEC §4 rule; `arch_test.go`). `internal/ctl` imports only stdlib, `service`, `output`, `exitcode`, `buildinfo`: the MCP SDK cannot live in `ctl` [review].
- Writes run only during Serve, on Serve's writer handle (M4: `onWriter`, `internal/service/write.go`); this design keeps that invariant [author: session daemon, §5.3].
- The sealer of a state is fixed by the first command that creates it (M7, SPEC §7.3, `layout.json`).
- Library: the official Go SDK `github.com/modelcontextprotocol/go-sdk`, pinned at ≥ v1.8.0 (MCP 2025-11-25 support) [call]; audited and credited in `README.md`; its pure-Go dependency tree and Go toolchain need are checked when it is added (E19).

## 4. Approaches considered

| | Approach | Verdict |
|---|---|---|
| A | stdio only relays to a running daemon (`pcb serve` / `pcb-ui`) | Rejected: the author does not want a daemon to be a prerequisite. |
| B | **stdio relays to a running daemon, else `pcb mcp` runs the daemon machinery itself for the session** | **Chosen [author]**. Keeps M4's single writer, one owner per state, freshness from the loop. Cost: while an agent session serves, `pcb-ui` and `pcb serve` refuse to start. |
| C | Per-call hybrid: relay, else act directly under the account's `sync.lock`, writes included | First approved, then dropped [author, after review]: writes outside Serve reopen M4's invariants (writer handle, gate, read-back, key caches), a transient serving phase per write. |
| D | Direct reads with no daemon, writes require a daemon | Rejected [author]: write tools would need a daemon. |

Rationale correction [review, verified]: sharing one Proton session between processes is already safe (`refreshStored` adopts a token another process rotated, under the secrets lock, `internal/service/session.go`); the one-server rule exists for the cache writer, the sync cursor and Proton load, not for refresh tokens.

## 5. Design

### 5.1 Blocks [call]

```
              ┌──────────── daemon process (pcb serve | pcb-ui | pcb mcp session) ───────────┐
 remote agent ─▶ TLS listener ─┬─ /dav/...  A1 CalDAV ─┐                                     │
  (bearer)    │                └─ /mcp      A4 MCP ────┼──▶ service.Hub / service.API (C3)    │
 stdio relay ─▶ pcb.sock ──────┬─ /v2/...  CTL ────────┤                                     │
  (pcb mcp)   │                └─ /mcp     A4 MCP ─────┘                                     │
 stdio agent ═▶ (session mode) stdin/stdout A4 MCP ───────▶ (same Hub, in process)           │
              └───────────────────────────────────────────────────────────────────────────────┘
```

- `internal/mcp` (new, A4): the MCP server (tools, grants, untrusted marking, audit), built on the SDK and on `service` only. One constructor, `mcp.NewServer(hub, grantsFor)`, used by all three entries. Import rule as `caldav` plus the SDK.
- `internal/service` (C3) gains the structured event layer (§5.6); `internal/ical` (C0) gains recurrence expansion. No MCP types leak into either.
- `internal/daemon` mounts `/mcp` next to CalDAV on the listener (a mux in front of `caldav.Handler`) when `[mcp] remote` is on, and next to CTL on the control socket always (an `http.Handler` passed into `ctl.Server` options, so `ctl` stays SDK-free) [review].

### 5.2 Remote: `/mcp` on the listener [author: bearer tokens]

- Off by default: `[mcp] remote = true` turns it on (§5.9). Same listener, port, TLS rule and certificate as CalDAV (SPEC §9, §14): TLS whenever the bind is not loopback.
- Streamable HTTP transport, stateful sessions (needed for elicitation, §5.7). Its own server timeouts on `/mcp` (no 60 s write timeout for long-lived streams; an idle timeout instead) [review].
- **Tokens.** `Authorization: Bearer pcbm_<43 base64url chars>` (256 bits). Created by `pcb mcp token add NAME --allow CAPS [--account A ...] [--calendar ID ...] [--expires DURATION]`, shown once, stored as a salted hash only (never a sealed copy: no `show`) [review] in `<state>/mcp-tokens.json` (root level, since a token spans accounts; 0600; strict decoding; format version 1). `pcb mcp token list` (name, grants, created, last used, expiry; `--json`), `pcb mcp token remove NAME`. No rotate: remove and add. The `pcbm_` prefix keeps a CalDAV password from passing as a token and the reverse.
- Failures (no token, unknown, expired, revoked): 401; the CalDAV failure limiter's logic (`access.Limiter`: 5 per minute per IP, /64 for IPv6) applies to `/mcp` with its own instance, 503 + `Retry-After` beyond [review].
- Loopback without TLS: Host must be a loopback name or a configured hostname, and a present `Origin` must match it (DNS-rebinding guard), whatever the SDK does by default (its cross-origin options changed across versions, E19) [review]. *Built (R17): the Host check runs on every bind and also accepts the IP the request arrived on; a present `Origin` must equal `Host` exactly, `null` refused; a session is bound to the token that opened it and forgotten after 1 h idle (the SDK's session timeout).*
- Remote never exposes `login` or `logout` (§5.5, §5.4).

### 5.3 stdio: `pcb mcp` [author: session daemon]

`pcb mcp [--profile read-only|full] [--allow CAPS] [--account A ...]` speaks MCP on stdin/stdout; logs and warnings go to stderr; `--json` is refused (exit 2), as for `serve`.

At start, and again whenever its daemon disappears:
1. **Probe** the control socket (`ui.Probe`'s logic). A daemon answers → **relay mode**: forward the MCP session to the daemon's `/mcp` on the control socket, sending the profile (grants) and the entry (`relay`) in headers. The daemon serves **every** tool, `login` included, with its own Hub; `pcb mcp` is a plain byte bridge and calls no `/v2` route [call, builder check].
2. No daemon → try to **become the daemon**: `daemon.Run` with `Kind: "mcp"`, `WaitForLogin: true`, the control socket bound (so the CLI, `pcb-ui`'s probe and other `pcb mcp` processes see it), **no CalDAV listener and no remote `/mcp`** [call: an agent session does not open a network port]; the MCP server runs in process on stdio.
3. Lost the race for `serve.lock` (another `pcb mcp` or a daemon starting) → back off and probe again, bounded (≈ 10 s), then exit 20 [review: the lock is the authority, the socket a hint].
- **End.** stdin closed or the client disconnects → drain (5 s, as `serve`), release, exit 0. A relaying `pcb mcp` whose daemon ends sees the socket close: the call in flight fails as transient (the tool error says "retry"), and the next call re-probes and may become the daemon itself.
- **Blocking.** While a session daemon serves, `pcb serve` exits 20 and `pcb-ui` shows its "already serving" dialog, both naming the kind `mcp` and the PID, hint "end the agent session". `pcb status` and `pcb doctor` show kind `mcp`. A systemd unit started meanwhile fails, and `Restart=on-failure` retries it only 5 times in 5 minutes (`StartLimitBurst`): it then stays `failed` until the person runs `pcb service start` [call; documented, hand-over later]. *Final review (C I2): a `pcb mcp` therefore never becomes the session daemon while a pcb user unit is installed and systemd reports it active or starting (`activating`, its `Restart=` gap of `RestartSec=10s` included, also `reloading` or `deactivating`): it waits for the unit's daemon (≈ 30 s) and relays to it, else exits 20 with the hint `pcb service start`. A unit installed but inactive, failed or disabled (the person stopped it) changes nothing: `pcb mcp` serves the session as without a unit.*
- **Another user's daemon** [call, builder check]. `pcb mcp` finds only a daemon of its own UID and directories (`ctl` refuses another peer UID). When a system-scope pcb unit is installed (`pcb install --system`, detected as `pcb uninstall` does), `pcb mcp` never becomes a session daemon: exit 20, hint "pcb runs as a system service here: use its remote /mcp with a token (`[mcp] remote = true`, a loopback `listen` is enough)". Otherwise a second state could serve the same Proton account unseen by `serve.lock`. A container is supported over stdio by running `pcb mcp` inside it (`docker exec -i <container> pcb mcp`): same user and directories, so it relays.
- **Sealer guard** [review]. On a state with no `layout.json` yet, under `[secrets] backend = auto`, `pcb mcp` creates the state only when a Secret Service answers (the keyring is chosen); with no session bus or no answer, `login` refuses: exit-10-class tool error "pcb mcp will not choose how secrets are sealed: set [secrets] backend in <config path>, or run the first pcb login in a terminal". An explicit `backend` is honoured as by every command. Agent hosts that scrub `DBUS_SESSION_BUS_ADDRESS` cannot fix a desktop state to the key file by accident. *Built (R20): the guard runs when the session daemon starts, before it opens the state, since opening a new state already records its sealer: `pcb mcp` exits 10 with that message on stderr and creates nothing; the `login` tool asks it again.*
- **Trust** [review]. Over stdio, grants are a guard rail, not a boundary: any process of the same UID can run `pcb` or dial the socket. The control socket's `/mcp` route takes the grants the relaying process sends; the peer-UID check stays the only boundary. Decision 0016 supersedes the SPEC §12 note "agents never on the control socket" with "agents reach the control socket only through its `/mcp` route, under same-UID trust; never the `/v2` routes". `pcb mcp` uses no `/v2` route except the probe.
- **Profiles.** `read-only` = `read` + `login`; `full` = every capability. `--allow` replaces the profile with an explicit list; `--account` narrows accounts. Default: `read-only` [call: the safe default; the author's "configurable read-only or full"].

### 5.4 Grants [call, review]

Capabilities, each a set of tools:

| Capability | Tools | Note |
|---|---|---|
| `freebusy` | `status`, `calendars_list`, `free_busy` | busy intervals only, no titles or attendees |
| `read` | + `events_list`, `event_get`, `events_search` | implies `freebusy` |
| `write` | `event_create`, `event_update` | implies `read` (an edit needs the ETag it read) |
| `delete` | `event_delete` | needs `write`; separate so a token can edit but not destroy |
| `login` | `login` | stdio only (refused at token creation) |
| `manage` | `logout` | stdio only |

- Scope: a grant names accounts (address or local ID; none = every account, including ones added later) and optionally calendars (IDs; none = all). A tool asked about anything outside the scope answers as if it did not exist ("no account X"), so a token cannot probe other accounts. *Built: a signed-out (`logged-out`) or pending account is invisible too (T9); under a calendar grant an account whose calendars are not known yet counts as holding none, fail closed (R14); every `event_id` tool hides an invisible account as a missing event (R15).*
- `login` sits in the stdio `read-only` profile on purpose: the person must type the secrets on the page or in the wizard (§5.5), so an agent alone cannot attach an account; its risk is spam, bounded by the cooldown. It is never grantable to a remote token [review: a read-only remote token could otherwise attach accounts].
- Write tools refuse a calendar the write gate keeps read-only (`Calendar.ReadOnly`, M4-R1), whatever the grant.

### 5.5 Login and logout through MCP [author: login inside MCP; never for headless]

`login(username, unattended_relogin?)` is stdio only (entries `stdio` and `relay`; never `remote`). It starts a login of a new, pending or signed-out account (the Hub's rules decide which, §4.1) and returns where the person finishes it; it never takes a password, code or token as an argument. The process that owns the Hub runs it, whichever entry the call came from [call, builder check]:
- **That process is `pcb-ui`** (kind `ui`): it raises its own add-account wizard in process, with the username filled in (the wizard's steps, password, 2FA and CAPTCHA, are the login state machine; the window gains a "prefill username" entry point). Native window, no listener. *Built (amended at build time, rulings R18, R21): that state machine moved out of `internal/ui/wizard.go` into `internal/loginflow`, which both `internal/ui` (the wizard) and `internal/mcp/loginpage` (the page below) use; `internal/mcp` never imports `internal/ui` (that would be the cycle ui → daemon → mcp → ui), which its arch test enforces. `pcb-ui` reaches the wizard through a `ShowLogin` callback, and a login the person is already in keeps its step.*
- **Otherwise** (`pcb mcp` session daemon, or `pcb serve` of this user reached by relay): that process opens a **temporary loopback page** for this one login and returns its URL: ephemeral port on 127.0.0.1 only (never the `[caldav] listen` address), a path with a 128-bit single-use nonce, bound to the username (shown read-only), TTL 5 min, Host and Origin checked, `Cache-Control: no-store`, no secret ever echoed, closed when the login ends. The page runs the same steps as the wizard (password, then 2FA code or CAPTCHA) against the Hub in process. MCP URL-mode elicitation presents the link when the client declares it; otherwise the URL is in the tool result text (the nonce is not a secret: it authorises only typing credentials for that username, once) [review]. A `remote` call never reaches this code. *Built (T14): the page binds `127.0.0.1:0` and its path is `/login/<32 hex>`; single use is a cookie set by the first form post (any other browser: 410); URL elicitation declined or cancelled closes the page and answers `refused`, accepted it answers without repeating the URL.* *Final review (C M1, M4): the page's unattended-relogin box is always unticked, as in `pcb login` and `pcb-ui`; the agent's `unattended_relogin` shows as a note, and the box carries `pcb-ui`'s keyring warning; the nonce is compared in constant time.*
- The tool result also carries the CLI fallback (`pcb login --username U`), for a `pcb mcp` inside a container whose loopback the person's browser cannot reach.
- **Pending login across a daemon start** [review]: in session mode the pending login lives in the session daemon's Hub; when relaying, in the daemon's. The loopback page holds no login state of its own, so no daemon ever misses an account added behind its back.
- **Cooldown**: one `login` call per 60 s per MCP session, on top of the Hub's login guard and "one login at a time" [review].
- `login_status` is folded into `status` (the account's phase: `needs-totp`, `needs-captcha`, ready).
- `logout(account)` (`manage`, stdio only) signs one account out as `pcb logout` does; asked for confirmation by elicitation when the client supports it, else refused unless the profile grants `manage` explicitly (`full` does). *Built: without form elicitation the `manage` grant alone allows it; `manage` is only ever granted explicitly (`full` or `--allow`), so the effect is the same.*
- Over remote, `status` reports `needs-login` with the human step ("run `pcb login` on the server, or open pcb-ui"); there is no login tool.

### 5.5b Per-account MCP switch [author]

- `pcb account mcp on|off` beside `pcb account caldav on|off` (same selector, `--json`, through the daemon when one runs, M6 rules), stored as `mcp` (boolean; absent = on) in the account's `state.json`, as `caldav` is. `pcb-ui`'s per-account adapters gain the same toggle.
- **Loop rule change** (M6 `Hub.wantsLoop`, SPEC §4.1): an account's loop runs when it is not pending, not parked, and **either** adapter is on. `caldav off` then only makes CalDAV refuse the account's clients (401); the loop pauses only when both are off. `mcp off` makes the account invisible to every MCP entry (as if outside every grant) and is the person's kill switch for all agents on that account, whatever the tokens say.
- `pcb status` and `account list` show both switches; `serve`'s exit rule counts an account with either adapter on as running (today: "an account with CalDAV off counts as running" stays true).
- Tests: the existing `TestCalDAVOffPausesTheLoop` becomes "both off pauses"; new: CalDAV off + MCP on keeps syncing and accepts MCP writes while CalDAV answers 401; MCP off hides the account from `tools` results and from `status`.

### 5.6 Structured event layer (core work, before any tool) [call, review]

Today `Events()` returns times, UID and ETag only; content exists only as rendered iCalendar (`ObjectsByUID`); writes are whole VCALENDARs (`PutObject`). M8 adds, in `service` and `ical`, with no MCP type:
- **Event view**: decode a rendered object into a JSON-friendly struct: summary, description, location, start/end with TZID or floating or all-day dates, RRULE/RDATE/EXDATE, overridden occurrences, alarms, status, transparency, organizer and attendees (read only), calendar type, URL; unknown properties counted, not shown.
- **Occurrences in a range**: recurrence expansion in C0 — RRULE, RDATE, EXDATE, overrides matched by RECURRENCE-ID, all-day, TZID with DST, floating times in the requested zone. Library: settle E16 (RRULE library audit vs our own; `rrule-go` is already an indirect dependency via go-ical) before coding [call]. Caps: 1000 occurrences per series per call; range ≤ 366 days. *Built: RDATE is not expanded, since Proton refuses to store it (`ical.RDate` refusal at write time); expansion handles RRULE, EXDATE and overrides (plan Global Constraints). E16 settled on our own expander (rrule-go got DST gaps and overlaps wrong). An event that cannot be read is skipped and counted, never failing a page (R9, R12).*
- **Edits as patches**: read the stored object, apply the field changes to the parsed VCALENDAR (`ical.Patch` keeps unknown lines, `internal/ical/write.go`), `PutObject` with `If-Match` = the ETag the agent read. "This occurrence" of a recurring event writes an override (RECURRENCE-ID); deleting one occurrence adds an EXDATE; "series" changes the master. "This and following" is not offered. **Series edits and their exceptions** [call, builder check]: a series change that only moves the series in time (same duration change or same shift for every occurrence) shifts its EXDATEs and its overrides' RECURRENCE-IDs by the same delta, as Thunderbird does; any other change that would leave an EXDATE or override pointing at a time the new rule no longer produces (RRULE, RDATE or start-time-of-day changes) is refused (`refused`, reason `pcb:series-has-exceptions`, naming how many), and `dry_run` shows them. Nothing is dropped silently. *Built (R11): a shift that moves the date and would leave an exception, or DTSTART, off the rule kept is refused too (`pcb:series-has-exceptions`, or `pcb:bad-data` for a start off its own BYDAY or BYMONTHDAY); pcb never rewrites a BYDAY, the agent changes the rule and the start together. A pure shift also moves UNTIL.* Every M4 refusal applies unchanged (shared signed changes, attendee changes, unverified parts, untrusted key packets: `IsSharedSignedChange`, `AttendeeChanges`), mapped to a tool error naming the reason.
- **Cost of content reads**: rendering decrypts per object. `events_search` and `events_list` over a range decrypt every event they return or test; a per-call budget (events rendered) caps them, and the result says `truncated`.

### 5.7 Tools

Common rules [call, review]:
- Every result has `structuredContent` with an `outputSchema`, and a short text summary. Annotations: `readOnlyHint` on reads, `destructiveHint` on `event_delete` and `logout`, `idempotentHint` where true.
- **Ids**: one opaque `event_id` (base64url of account local ID, calendar ID, UID, optional RECURRENCE-ID). Agents never build ids.
- **Time**: inputs are RFC 3339 instants with an offset, or dates with a `tz` (IANA) argument; default zone = the process's local zone (`[mcp] timezone` overrides it, for containers). Outputs give each time with its own TZID and UTC offset, floating times marked `floating`, all-day as dates (end exclusive). `status` returns `now` (instant, zone, offset): agents have no reliable clock.
- **Paging**: list tools take `limit` (default 100, max 500) and `cursor`; results carry `truncated` and `next_cursor`. Descriptions are cut at 4 KiB in lists (full, up to 64 KiB, in `event_get`).
- **Untrusted content**: every calendar-derived free-text field (summary, description, location, URL, organizer and attendee names, alarm text) sits under an `untrusted` object, and each tool description says it is data written by third parties, never instructions. Events whose organizer is not the account's address, and events of subscribed or holiday calendars, are flagged. This is a mitigation, not a boundary; the controls are grants, ETags and confirmations.
- **Errors**: a tool error carries the pcb error's message and hint (secret-free by the existing `exitcode.Error` rule) and a class: `must-act`, `retry` (with seconds, from Proton's `Retry-After`), `refused`, `conflict` (stale ETag: re-read), `not-found`. *Built (T9): the error is the result's text, `{"error": {...}}`, with `isError`, not `structuredContent` (an error result would have to match the success `outputSchema`); an error that is not a pcb error says only `the call failed`.*
- **Accounts**: read tools take `accounts` (default: every granted account) and merge results; write tools need one explicit `event_id` or `account` + `calendar`.

| Tool | Cap. | Arguments → result |
|---|---|---|
| `status` | freebusy | → `now`; per granted account: address, phase (ready, needs-login, needs-totp, needs-captcha, logged-out, stopped), last sync age, stale calendars, alerts with their human step. *Built: a `logged-out` account is not listed (invisible, T9)* |
| `calendars_list` | freebusy | `accounts?` → id, account, name, colour, type, `read_only` (write gate), default |
| `free_busy` | freebusy | `from`, `to`, `accounts?`, `calendars?` → merged busy intervals per account (transparent and cancelled events excluded) |
| `events_list` | read | `from`, `to`, `accounts?`, `calendars?`, `limit?`, `cursor?` → occurrences: `event_id`, times, all-day, recurring, summary/location (untrusted), etag |
| `event_get` | read | `event_id` → the full event view (§5.6), etag |
| `events_search` | read | `text`, `from`, `to`, `accounts?`, `calendars?`, `limit?` → as `events_list`, matched case-insensitively in summary, location, description |
| `event_create` | write | `account`, `calendar`, `summary`, `start`, `end` or all-day `start_date`/`end_date`, `tz?`, `location?`, `description?`, `rrule?`, `alarms?` (minutes before), `busy?` → `event_id`, etag |
| `event_update` | write | `event_id`, `etag`, `scope` (`occurrence`\|`series`), `changes` (the create fields; `null` clears), `dry_run?` → new etag, or with `dry_run` the before/after of the changed fields |
| `event_delete` | delete | `event_id`, `etag`, `scope` → done; confirmed by form-mode elicitation (summary and time shown) when the client supports it; without elicitation support, the `delete` grant alone allows it [call: the grant is the person's decision]. *Built: a client declaring URL-mode elicitation only counts as one without (grant alone); the whole-series prompt names the series (R16); a stale ETag is `conflict` before anything is asked* |
| `login` | login | `username`, `unattended_relogin?` → URL or wizard raised, CLI fallback (§5.5) |
| `logout` | manage | `account` → as `pcb logout` (§5.5) |

### 5.8 Audit and observability [call]

- Every tool call is logged at `info` in the daemon log (M7 `logs/` file, journald for the service): tool, entry (`remote`/`relay`/`stdio`), token name or profile, account, calendar, event UID, outcome class, duration. Never arguments' free text, content, URLs with nonces or secrets.
- `pcb status` shows, under the process: `mcp: remote on|off`, number of live MCP sessions per entry, the session daemon's kind. `pcb doctor`: `mcp tokens` check (file readable, expired tokens listed as warnings), and a warning when `[mcp] remote` is on with a non-loopback bind and no token exists.
- Metrics as for CalDAV if SPEC §15 counters exist for it: calls per tool and outcome class. *Built: none, as for CalDAV (no Prometheus endpoint, M5-Rpre11); `pcb status` shows the `mcp:` line only when a daemon answers (R17).*

### 5.9 Configuration and CLI [call]

`config.toml`:
```toml
[mcp]
remote = false          # serve /mcp on the [caldav] listener
timezone = ""           # IANA zone for dates without one; empty: the process's local zone
```
CLI: `pcb mcp [--profile P] [--allow CAPS] [--account A]...` (stdio server), `pcb mcp token add NAME --allow CAPS [--account A]... [--calendar ID]... [--expires D] [--plan]`, `pcb mcp token list`, `pcb mcp token remove NAME [--yes]`. Every `token` command has `--json` and the exit codes of `internal/exitcode`; the token is printed once (stdout, or `data.token` in the envelope) and never logged. `pcb mcp` itself refuses `--json` (exit 2). *Built: `token add` refuses `delete` without `write` at creation (R19); `--expires` also takes whole days (`30d`); `pcb mcp --account` is not checked against the accounts (a grant may name one a later login adds).*

### 5.10 Delivery (stacked PRs on `m8-mcp`, each reviewed and merged alone) [call, review]

1. **E16, E19** settled (RRULE library; SDK version, CGO-freeness, cross-origin behaviour, which agent clients declare elicitation and URL mode).
2. **Event layer** (§5.6): event view, recurrence expansion, patch edits; service tests only, no MCP. **Per-account MCP switch** (§5.5b) in the same step: Hub loop rule, `pcb account mcp`, status, pcb-ui toggle.
3. **`internal/mcp` + remote `/mcp`**: tokens, grants, `freebusy` and `read` tools, untrusted marking, audit, limiter, Host/Origin guard.
4. **Write tools**: create, update, delete, elicitation confirmations.
5. **stdio `pcb mcp`**: relay over the control socket's `/mcp`, session daemon (kind `mcp`), race and failover, sealer guard, the `pcb-ui`/`serve` refusal wording.
6. **`login`/`logout`**: `pcb-ui` wizard raising, loopback page, cooldown.
7. Live checks (`spec/HUMAN-CHECKS.md` new step, test account only): Claude Code over stdio (session and relay to `pcb-ui`), remote with a token, one create/update/delete seen in the Proton web app, one login through the loopback page.

## 6. Tests [call]

Tests bite and come first (AGENTS.md). Beyond unit tests per block:
- Grants: each capability's tools present/absent in `tools/list`; a scoped token cannot see or name another account's calendars (same error as a missing one); `login`/`manage` refused at token creation.
- Tokens: shown once, hash-only on disk, prefix mismatch with CalDAV passwords, expiry, limiter 503 after 5 failures.
- Remote guard: wrong Host or Origin refused on a loopback bind; `/mcp` absent when `remote = false`.
- stdio: two `pcb mcp` started together end as one session daemon and one relay; a relay re-probes and takes over after its daemon ends; `pcb serve` and `pcb-ui` refuse while a session daemon serves, naming kind `mcp`; the sealer guard with and without a session bus (`internal/dbustest`).
- Event layer: expansion against RFC 5545 examples and Proton recurrence forms (E12 dataset), DST transitions, floating and all-day; a patch keeps alarms, exceptions and unknown properties (round-trip golden files); stale ETag → `conflict`; every M4 refusal surfaces as `refused` with its reason.
- Secrets: no password, code, CAPTCHA token, nonce URL or MCP token in any tool result, MCP log message, audit line or error (a scanner test over recorded outputs, as for the CLI).
- Loopback page: single use, TTL, Host/Origin, no-store, closed after the login.
- On the fake (`cmd/pcb-fake`, `make rehearse`) end to end: a stdio session lists, creates, updates and deletes.

## 7. Left to the builder

Package layout inside `internal/mcp`; the event view's exact field names and JSON schema; the opaque id encoding; how the relay forwards the stream (byte bridge vs SDK client/server pair); the exact back-off of the `serve.lock` race; budget numbers for rendering per call; the loopback page's HTML (minimal, no external assets, no JavaScript beyond what the CAPTCHA step needs); log line formats; whether metrics exist for MCP (follow what §15 has for CalDAV).

## 8. Documents to update

Decision 0016 (MCP adapter; amends 0007 and SPEC §12's socket note); `decisions/INDEX.md`; SPEC §1, §3, §4, §12 (replaced by the built behaviour), §13, §14, §15, §17 (E19), §18 (M8), §19 (security model: tokens, loopback page, same-UID trust); `README.md` (MCP usage, credits for the SDK and any RRULE library); `spec/HUMAN-CHECKS.md` (M8 step); `spec/OPEN.md` (residuals).

## 9. Settled by the author (2026-11-13)

- An MCP server in `pcb`, stdio and remote; read-only or more by configuration; login inside MCP; multi-account.
- Remote clients: header-capable only, one bearer token per agent; no OAuth.
- Developed in parallel with desktop work, building on M7's structure.
- stdio: relay to a running daemon, else `pcb mcp` runs the daemon machinery for the session (approach B), after the per-call hybrid was dropped on review.
- Login for headless (remote) use: never through MCP.
- A per-account MCP switch beside CalDAV's; an account's loop runs when either is on.

## 10. Calls made after the builder check (author to check)

- Relay mode: the daemon runs every tool, `login` included; `pcb mcp` is a byte bridge with no `/v2` call; the loopback page is opened by whichever process owns the Hub, on 127.0.0.1 only (§5.3, §5.5).
- With a system-scope pcb unit installed, `pcb mcp` refuses to become a session daemon and points to remote `/mcp` on loopback; containers are reached with `docker exec -i ... pcb mcp` (§5.3).
- Series edits: a pure time shift moves EXDATEs and overrides along; any other change that would orphan them is refused (`pcb:series-has-exceptions`), never dropped (§5.6).
- `daemon.Run` gains a "no CalDAV listener" option for the session daemon (today it always opens one).
