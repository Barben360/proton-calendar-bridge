# proton-calendar-bridge — specification (draft v2, 2026-11-10)

Status markers used below:
- **[decided]** the author decided it (see `decisions/`).
- **[call]** the spec author's call; the author may overturn it.
- **[verified: source]** a fact checked live or in code we trust.
- **[assumed → E#]** a belief not yet checked; experiment `E#` must settle
  it before any code relies on it (§17).

## 1. Purpose and success criteria

The calendar counterpart of Proton Mail Bridge. A daemon logs into a Proton
account, keeps an up-to-date, decrypted view of Proton Calendar, and serves
it as a CalDAV server. Proton Calendar is end-to-end encrypted and has no
public API, no CalDAV, and no official bridge [verified:
proton.me/support/subscribe-to-external-calendar, 2026-11-10].

**Scope of this spec: a connectable CalDAV interface** [decided 0007] **and,
since M8, an MCP adapter for AI agents** [decided 0016, which amends 0007
for MCP]. Other adapters (REST, `.ics` feeds) are not planned; the
architecture only keeps the door open for them (§4).

Success means:
1. Thunderbird shows every calendar of the account, and two-way edits
   (create, move, edit one occurrence, delete) show up in the Proton web app
   within the sync interval, and back [decided 0004].
2. It runs the same way on a desktop and as a headless container on a
   server, like the author's Proton Mail Bridge setup today [decided 0009]; where it is exposed is a network
   configuration, not a code path.
3. Everything, install to uninstall, is done from a terminal [decided 0001].
4. An expired session repairs itself with no human, unless Proton demands a
   CAPTCHA; then one URL to solve from any device and one command resume it.
5. No silent data loss: an event that cannot be represented faithfully is
   refused or flagged, never mangled (§8.4).
6. (M8) An agent reads and edits the calendars it is granted through MCP,
   on this machine over stdio (`pcb mcp`) or from another one with a bearer
   token, and never handles a Proton secret: a login it starts is finished
   by the person, in `pcb-ui`'s wizard or a one-time page on 127.0.0.1
   (§12) [decided 0016; built and tested against the fake Proton, with a
   real agent client `[assumed → step 10]`].

## 2. Principles

- **Headless first** [decided]. One Go binary. No GUI in `pcb`; the desktop app
  (`pcb-ui`, a separate cgo binary running the same daemon in process) was built in M7
  (decision 0015, `spec/designs/2026-11-12-multi-account-and-ui-design.md`), and `pcb` never
  gains a GUI dependency [verified: `TestPcbHasNoGUIDependency`].
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
- **Good citizen** [call]. One Proton session and rate limiter per account (since
  M6 every account of a state directory runs in one process, and one login guard
  spaces their logins), backoff on 429/5xx, no busy polling, a truthful client identity (§5.1).
  A flagged or banned Proton account is a failure even if the code works.
- **Scriptable CLI** (backsight conventions) [call]: `--json` envelope,
  `--plan`, `--non-interactive`, fixed exit codes (§13).

## 3. Goals, non-goals, anti-goals

Goals: CalDAV read/write for owned calendars; read-only for subscribed and
holidays calendars; login with password, or password + TOTP [decided
0010]; a CLI for everything about the bridge itself (install, service,
login, status, doctor, CalDAV client credentials, uninstall); several Proton
accounts in one process (M6, decision 0014); a desktop app for Linux, `pcb-ui` (M7, decision 0015: tray, account window, add-account wizard, notifications); an MCP adapter for agents (M8, decision 0016: stdio and remote, grants per agent, login finished by the person, §12); a container image; Linux amd64 and arm64. Those two are the supported platforms; macOS and
Windows only build, and the CLI there runs offline (§13, "Platforms").

Not planned (not designed here): REST API, `.ics` feeds, event
commands in the CLI (Thunderbird is the client), updates/signed
releases (while private), shared calendars, invitations, two-password mode,
FIDO2, CardDAV, macOS/Windows service management, a GUI on Windows or macOS (`pcb-ui` is Linux only; Windows is next, macOS is not planned). The desktop app
`pcb-ui` is built (M7; "GUIs not planned" before M6, "planned" before M7), designed in
`spec/designs/2026-11-12-multi-account-and-ui-design.md`. MCP is built (M8; "not planned"
before it), designed in `spec/designs/2026-11-13-mcp-design.md`; out of it stay OAuth (claude.ai and
mobile connectors), any login over remote MCP, invitations and RSVP, calendar management, and MCP
resources, prompts and sampling (tools only).

Anti-goals (failure even if it works):
- Plaintext calendar data or reusable credentials on disk unsealed.
- A listener reachable beyond loopback without TLS and authentication.
- Behaviour that gets the Proton account flagged (login storms, CAPTCHA
  loops, unbounded polling).
- Silent lossy round-trips (an edit in Thunderbird that drops alarms or
  exceptions in Proton).
- (M8) A secret in an agent's reach: a Proton password, 2FA code or CAPTCHA token in a tool
  argument, result or the agent's transcript; an MCP token anywhere after `pcb mcp token add`
  printed it (§12, §19).

## 4. Architecture

```
                 ┌──────────────── pcb daemon (every account) ───────────────────┐
 Thunderbird ──▶ │ A1 CalDAV ──▶ C3 service (use cases, access checks)            │
 remote agent ─▶ │ A4 MCP /mcp ─▶│  (M8: listener, bearer token)                 │
                 │                    │                                           │
 pcb CLI ──────▶ │ control API ──────▶│   C4 sync engine                          │
 (unix socket)   │ (internal; /mcp:   │        │                                  │
 pcb mcp relay ─▶│  A4 for stdio)     │                                           │
                 │               C2 store (cache, state, sealed secrets)          │
                 │                    │                                           │
                 │               C1 proton (transport, auth, keys, calendar API,  │
                 │                    change feed, codecs: model ⇄ Proton)        │
                 └───────────────────────────────────────────────────────────────┘
 pcb mcp with no daemon runs this same daemon itself (kind mcp, no listener, §12).
 Later adapters (REST, .ics) would plug into C3 like A1 and A4; not planned.
```

Binary name: `pcb` [call; short, no "proton" in it, rename-proof].

Blocks and their contracts:

| Block | Owns | Exposes | Depends on |
|---|---|---|---|
| C0 model | iCalendar-native event model (decision 0012), iCalendar codec (RFC 5545), recurrence expansion, time zones | Go types + pure functions | stdlib, a vetted RRULE lib or our own |
| C1 proton | HTTP transport, client identity, rate limit, auth (SRP, 2FA, human verification, two-password, scopes), key hierarchy, calendar endpoints, change feed, Proton card codec (model ⇄ encrypted parts) | `proton.Session`, `proton.Calendars`, `proton.Feed` interfaces | go-proton-api, gopenpgp, go-srp, C0 |
| C2 store | SQLite cache of ciphertext rows and sync state; sealed secret store | `store.Cache`, `store.Secrets` | modernc.org/sqlite (pure Go) [call], C0 |
| C3 service | Use cases: list/get/search/create/update/delete events, calendars, free-busy (local), login flows; applies C5 | `service.API` (Go interface, the only thing adapters call) | C1, C2, C4, C5 |
| C4 sync | Initial and incremental sync, write queue, conflict detection, backoff | `sync.Engine` | C1, C2 |
| C5 access | CalDAV client credentials (app passwords), read-only enforcement for subscribed/holidays calendars; per-client grants designed but not built; MCP bearer tokens with their grants (M8, `access.TokenStore`, hash only) | `access.Check(client, op, calendar)`, `access.TokenStore` | C2 |
| C6 daemon | Process lifecycle, listeners, config, logs, metrics, health | `pcb serve` | all |
| A1 | CalDAV adapter (§9) | CalDAV | C3 only |
| A4 | MCP adapter (§12, M8): tools, grants, untrusted marking, audit, the loopback login page; built on the official Go SDK | MCP over streamable HTTP (`/mcp`) and stdio | C3 only, `internal/access` (token types), `internal/loginflow` [verified: `TestMCPArch`, `TestLoginPageArch`] |
| CTL | Control API for the CLI: JSON over HTTP on a unix socket, internal, versioned, not a public API; since M8 it also carries A4's `/mcp` route for `pcb mcp` relays, as an opaque handler | `pcb` subcommands | C3, C6 |

Rule: adapters never import C1, C2 or C4; they only call `service.API` (A4 also `service.Hub`'s
`Agenda`, the structured event layer of M8: event view, occurrences in a range, edits as patches).
`internal/mcp` never imports `internal/ui`: the login state machine both use is `internal/loginflow`
(ruling R18) [verified: `TestMCPArch`, `TestLoginflowArch`]. Since M6
a second account is a second `service.Local` in the same process, held by
`service.Hub` (decision 0014; built, below).

### 4.1 One process, several accounts (M6)

`internal/service` (`Hub`, `Local`), `internal/daemon`, `internal/caldav`, `internal/ctl`.

- A `service.Hub` owns one `Local` per account directory (`accounts/<local-id>/`, §7.1)
  and what they share: the config, the sealer (one key file), the CalDAV failure limiter,
  the login guard (at the state root, so unattended relogins of several accounts are spaced
  by the same interval) and the root `tls/` and `pcb.sock`. `Local` stays single-account
  and runs on its own directory, with the guard directory and the limiter passed in
  [verified: `TestHubSharesTheLimiter`, `TestReloginsOfTwoAccountsAreSpaced`].
- **Locks.** The serving process (`pcb serve`) holds, for its whole life, the root
  `serve.lock` (it holds the PID as decimal text; a second serve exits 20 `pcb is already
  serving (pid N)`, hint `stop it first, or use pcb status`) and the root `sync.lock`, so
  an M5 binary run by mistake refuses instead of writing (ruling R7: M6 keeps the M5 name for
  the root lock). Each account keeps its own `sync.lock` in its directory, so an offline
  `pcb sync --account X` still excludes the daemon for that account only
  [verified: `TestSecondServeIsRefused`]. `OpenHub` takes the root `sync.lock` (and
  `layout.lock`, new) only while it migrates an M5 layout; with layout v2 it never
  touches the root `sync.lock`, so CLI commands work while a daemon serves (R2)
  [verified: `TestOpenHubIgnoresSyncLockOnLayoutV2`, `TestOpenHubRefusesWhileM5SyncLockHeld`].
- **One loop per account.** `Hub.Serve` runs one `Local.Serve` per account that is not
  pending and has either adapter (CalDAV, MCP; M8) on, each in its own goroutine and always waiting for a
  login when it has none (R3; the process exit rule below is the Hub's own). An account
  added, completed or switched on while serving starts its loop at once; a removed or
  switched-off one has its loop stopped (and waited for) first (R18)
  [verified: `TestServeRunsEveryAccount`, `TestAddedAccountStartsServing`,
  `TestRemovedAccountStopsFirst`, `TestBothAdaptersOffPausesTheLoop`]. One account's CAPTCHA or
  failure never stalls or stops another [verified: `TestOneAccountsCaptchaDoesNotStallTheOther`].
- **Parked accounts.** A loop that ends on an error its context did not cause (a damaged
  cache: exit code 10, as the single account of M5 (R21)) is **parked**: `AccountInfo.Stopped`
  carries the error's code, message and hint, the alert `account-stopped` (error, message the
  stop error's, fix its hint, §15) appears, and the account's CalDAV answers 503 with
  `Retry-After: 30`. It leaves the park when the person acts (`pcb account restart`, a login
  of that account) or at a process restart; there is no automatic retry. A Restart that races
  the loop's own park leaves the account shown stopped, and a later Restart works (R25)
  [verified: `TestDamagedCacheParksOnlyThatAccount`, `TestLoginRestartsAParkedAccount`,
  `TestRestartWhileALoopParksServesAgain`]. The one exception is a sync lock another process
  holds (an offline `pcb sync --account X`, a `--watch`): that is not a park, the loop retries
  every 30 s and logs it once per distinct cause (R23) [verified: `TestHeldSyncLockIsRetriedNotParked`].
- **Exit rule of `pcb serve`** (so the units' `Restart=on-failure`, `RestartPreventExitStatus=10`
  and the container keep their meaning). With `--wait-for-login` (what the units and the image
  run) the process never exits for an account's sake: it ends only when its context ends
  (also with no account at all, where it prints `Waiting for a login: run pcb login`). Without
  it, it refuses to start when no account is logged in (exit 10; the only account's own error when
  there is one) and exits with the first stopped account's code once **every** account that is not
  pending has stopped (parked, or waiting for a login); with one account that is the M5 behaviour.
  An account with either adapter on is served and counts as any served account does (M8: one with
  CalDAV off and MCP on whose login ends has stopped); one with both off, its loop paused, counts as
  running [verified: `TestServeExitRuleCountsMCPOnAsRunning`, `TestServeExitsWhenEveryAccountStopped`,
  `TestServeRefusesWithNoLoggedInAccount`, `TestServeWithoutTheFlagExitsWhenTheOnlyAccountNeedsALogin`,
  `TestServeWaitForLoginParksAnUnreadableAccount`, `TestServeWaitsForALoginWithNoAccount`].
- **Adding, relogin, pending.** `Hub.Login` takes the username: an account whose address matches
  (case-insensitive, pending or not) is relogged; an account whose `state.json` cannot be read
  matches by its sealed account's username and the login meets its fault (exit 10), and a login
  of its Proton user under another address is refused as a duplicate (R37). Else, when exactly
  one account is signed out with no address and a readable `state.json` (an M5 account migrated
  signed out, R16, R37), that one is **adopted** by this first login whose address matches no
  account, whoever it is: the login inherits the account's `clients.json`, so the old Thunderbird
  password then reads the new user's calendars. When you add a different account after signing
  an M5 account out, run `pcb client rotate NAME` for each of its clients (`pcb client list`)
  [verified: `TestLoginAdoptsTheSignedOutM5Account`, `TestLoginDoesNotAdoptAnUnreadableAccount`,
  `TestLoginDoesNotAdoptALoggedInAddresslessAccount`, `TestLoginOfAnUnreadableAccountsUserAddsNothing`].
  Else a new `<local-id>` directory is made, the login runs on it and, on success, the account joins the running set.
  One login runs at a time per process (`a login is already in progress`). A failed add (wrong
  password, refusal, a 2FA code not given within the 5 min TTL) removes the new directory. An
  add stopped by a CAPTCHA keeps its directory as a **pending** account (phase `needs-captcha`,
  address in `state.json`, no Proton user ID): listed by `account list` and `status`, chosen by
  `login resume --account` (or alone), not counted by the "exactly one account" selector rule,
  and removed at the first Hub open more than 24 h after `State.Created`. An add waiting for its 2FA
  code is pending too. An abandoned add (no Proton user, never logged in, no data, no CAPTCHA, no 2FA
  code held here) is removed lazily at `Accounts`, `Account` and `Login`, and by `OpenHub` when its
  directory is older than the 5 min TTL plus one minute (R11, R13: a younger one may be another
  process's add in flight). A login whose Proton user ID is already another account's is refused
  (exit 10, naming it, hint `pcb account remove --account ...`): the session it opened is revoked
  first (R12; `pcb could not revoke` adds the sessions page to the hint) and an add's directory is
  removed. A 2FA code goes through `Hub.SubmitTOTP` so these rules also apply to it (R10)
  [verified: `TestLoginAddsSecondAccount`, `TestLoginAddsSecondAccountWithTOTP`,
  `TestLoginExistingAddressRelogsThatAccount`, `TestFailedAddLeavesNothing`, `TestCaptchaAddIsPending`,
  `TestLoginSameAddressReusesPendingAccount`, `TestPendingExpiresAfter24h`,
  `TestSignedOutAccountStoppedByCaptchaDoesNotExpire`, `TestSameUserOtherAddressIsRefused`,
  `TestExpiredTOTPAddIsRemovedLazily`, `TestOpenHubRemovesOldAbandonedAdds`,
  `TestLoginAdoptsTheSignedOutM5Account`, `TestSecondLoginWhileOneRunsIsRefused`,
  `TestSubmitTOTPWrongCodesLeaveNothing`].
- **Sign out, remove.** `pcb logout` signs one account out: session revoked, `secrets.sealed` and
  `pcb.db` wiped, the login reset, the directory, the address and `clients.json` kept (CalDAV passwords
  stay valid and viewable, decision 0013); it is listed as `logged-out`. `pcb account remove` signs
  out, then deletes the directory, confirmed as `logout --yes` is (`--non-interactive` without `--yes`:
  exit 10); a sign-out the store refuses keeps the directory, and calls in flight finish first;
  a CalDAV request does not wait for a removal: the account being removed is skipped (R39)
  [verified: `TestRemoveDeletesOnlyThatAccount`, `TestRemoveWaitsForTheCallsInFlight`,
  `TestCallDuringRemoveFindsNoAccount`, `TestCompatibilityRouteSkipsAnAccountBeingRemoved`]. The first completed login of an account creates its `default`
  CalDAV client (decision 0013).
- **Adapters switch.** `pcb account caldav on|off` writes `caldav` (a boolean; absent, as in an M5
  `state.json`, means on) into the account's `state.json`; the design's `adapters: {"caldav": ...}`
  became this one key. M8 adds `pcb account mcp on|off`, the key `mcp` (absent means on), beside it
  (design `2026-11-13-mcp-design.md` §5.5b). CalDAV off: CalDAV refuses the account's clients (401,
  no charge, R28). MCP off: the account is hidden from every MCP entry. The loop pauses only once
  both are off; an adapter switched on resumes it with a pass at once, and a switch made while a
  relogin runs, or kept across a sign-out, is kept (R19) [verified: `TestSetCalDAVPersists`,
  `TestSetMCPPersists`, `TestBothAdaptersOffPausesTheLoop`, `TestCalDAVOffMCPOnKeepsSyncing`,
  `TestCalDAVSwitchDuringAReloginIsKept`, `TestCalDAVSwitchDuringACaptchaReloginIsKept`,
  `TestMCPSwitchDuringAReloginIsKept`, `TestMCPSwitchDuringACaptchaReloginIsKept`].
- **Daemon wiring** is `internal/daemon` (one entry point, `daemon.Run`: Hub.Serve, control socket,
  CalDAV listener with TLS, the 5 s drain), used by `pcb serve` and, later, `pcb-ui`; it follows the
  `cli` import rule and owns no account (the caller opens and closes the Hub, and closes it only after
  `Run` returns) [verified: `TestRunServesCalDAVAndControl`, `TestRunDrainsOnCancel`,
  `TestDaemonArch`].
- **Daemon kinds (M8).** `serve` (`pcb serve`), `ui` (`pcb-ui`) and `mcp` (`pcb mcp` serving one agent's
  session itself, §12): `daemon.Run` with `NoListener` opens the control socket only, no CalDAV listener
  and no remote `/mcp`, even with `[mcp] remote = true`. `serve.lock` holds `<pid> <kind>` (an older
  `<pid>` alone still reads), so a second server names what holds the state: `pcb is already serving
  (pid N, kind mcp)`, hint `end the agent session (pcb mcp), then try again` for an agent session, `stop it
  first, or use pcb status` otherwise; a kind is echoed only when it is a plain lower-case word
  [verified: `TestRunWithoutListenerOpensNoPort`, `TestServeLockNamesTheKind`, `TestServeLockReadsTheOldFormat`,
  `TestASecondServeNamesTheRunningDaemon`, `TestBridgeNamesAnAgentSession`; `make rehearse` step 22].

## 5. C1 — Proton block

Reference for endpoints and crypto: proton-cal `docs/` (api.md, crypto.md,
overview.md; live-verified by its author) and our own experiments. Every
endpoint we use gets a recorded, redacted fixture and a contract test (§16).

### 5.1 Transport and identity
- Base URL `https://mail-api.proton.me` [verified: proton-cal docs/crypto.md].
- Client identity header `x-pm-appversion`: `Other` is accepted and yields
  calendar access [verified: E03 gate, R13 evidence; scopes pending]
  (the author's proton-cal fork sends it and passed its live integration
  suite, read and write, 2026-11-10); the other candidates and the scope list
  are measured at the E03 live run (`spec/experiments/E03-client-identity.md`).
  Sending the identity of an official Proton client is accepted if nothing
  truthful gets calendar access [decided 0008]. carbonate reports a
  `linux-mail@…` value working, caloxide an `external-drive` value losing
  calendar scope [assumed → E03]. The value is configuration, not code, with
  a tested default [decided 0011, provisional].
- One token-bucket rate limiter per account, shared by all callers; honour
  `Retry-After`; exponential backoff with jitter on 429/5xx; a circuit
  breaker that pauses sync and raises a status alert after repeated
  failures [call; Proton's limits → E06]. `Retry-After` is read as delay-seconds
  or an HTTP date, clamped to 1 h, and a past date or a malformed value counts as
  none; a login 429 names the wait ("retry in 2m0s") instead of "a few minutes"
  [verified: `TestParseRetryAfter`, `TestRetryAfterDateHonoured`,
  `TestBeginKeepsRetryAfter`, `TestLoginRateLimitedNamesRetryAfter`].

### 5.2 Authentication
State machine (persisted in C2):

```
logged-out ──login──▶ srp ──▶ [human-verification] ──▶ [2fa] ──▶ [mailbox-password] ──▶ ready
ready ──refresh fails (auth)──▶ relogin (unattended, if allowed) ──▶ ready | needs-human
needs-human: needs-captcha(url, token) | needs-2fa-code | needs-password | locked
```

- SRP via go-srp; password only, or password + TOTP [decided 0010];
  two-password mode and FIDO2 refused with a clear message for now.
- Human verification (CAPTCHA): the challenge token can be solved on any
  device at `https://verify.proton.me/?methods=captcha&token=<token>` and the
  login retried with it [verified: interactive login 2026-11-10]. Whether the
  solved token survives across processes and for how long [assumed → E02];
  the design relies on it for `pcb login resume --captcha-token`. M1 builds this flow and proves it offline against a fake; the marker stays `[assumed → E02]` until the M1 live session (`HUMAN-CHECKS.md`, "M1 live session").
- Unattended relogin [call, opt-in at login]: stores the login password and
  the TOTP seed (otpauth URI) sealed (§7.3), so an expired session is
  repaired without a human. Off by default; `doctor` says what it implies
  (whoever unseals the store owns the account).
- Session lifetime and refresh-token rotation rules [assumed → E01]: decide
  how often the daemon refreshes and when it must relogin. M1 refreshes on demand, once across processes; the Proton answers it classifies as "session ended" or "credentials refused" are all `[assumed]` until the live session lists them (`HUMAN-CHECKS.md`, "signals to confirm").
- Locked ("password") scope for salts and calendar deletion via SRP to
  `/core/v4/users/unlock` [verified: proton-cal docs/overview.md].
- Control channel [call]: the CLI talks to the daemon over a unix socket
  (0600) that checks the peer UID (SO_PEERCRED). CalDAV clients never use
  it; they authenticate with their own app password (§9).
- Credential inputs, in priority order: `--answer`/stdin for non-secrets;
  secrets only from a terminal prompt, a file descriptor (`--password-fd`),
  or environment (`PCB_PASSWORD`, `PCB_TOTP`, … for `pass-cli run`); never
  argv [call].
- Login robustness (initial-version audit, `spec/audits/2026-11-12-initial-version.md`):
  - Each login call (`Begin`: AuthInfo and the SRP proof; the 2FA submit; the locked-scope
    unlock: AuthInfo and proof) has its own 30 s deadline, so a stalled Proton cannot hold the
    login lock; a timeout is exit 20 "the operation was cancelled or timed out" [verified:
    `TestLoginCallsHaveADeadline`, `TestLoginDeadlineIsBlocked`].
  - Only Proton code 8002 on a 422 means "wrong Proton username or password"; any other 422 is
    "Proton refused the request (code N)" and exit 20 [verified: `TestBeginOther422IsNotBadCredentials`,
    `TestLoginOther422IsNotWrongPassword`].
  - Code 9001 on an authenticated read is a human verification, like on a write: alert
    `human-verification`, hint "open the Proton web app from this machine's network and complete
    the verification" (exit 20; `pcb serve` keeps retrying) [verified: `TestReadHumanVerificationIsTyped`].
  - The login guard's stamp more than 5 min in the future (a clock set back) is stale: logged once at
    warning level, rewritten with now, never blocks; a stamp less than 5 min ahead still counts [verified:
    `TestFutureStampIsStale`, `TestStampSkewBoundary`].
  - An interactive login that meets a CAPTCHA while the stored login is `ready` leaves the
    stored state as it was [verified: `TestLoginCaptchaWhileReadyKeepsState`]; the key passphrase is
    zeroed once the login completes [verified: `TestLoginZeroesTheKeyPassphrase`].

### 5.3 Keys
User keys → address keys → calendar passphrase (per member) → calendar key;
per-event session keys for shared and calendar parts [verified: proton-cal
docs/crypto.md, Proton security-model blog]. Unlocked keys live in memory
only; the salted key passphrase is sealed in C2.

Signature policy [call] (M3-R33, narrowed by M3-R36): verify every part
authored by one of our addresses. Such a part with a bad signature, or a
shared or calendar part of ours (shared signed, shared encrypted, calendar
signed, calendar encrypted) sent without a signature, marks the event
unverified; an attendees or personal part of ours marks it only when it is
signed and the signature fails. In CalDAV each affected VEVENT (the master or
an exception) carries `X-PCB-INTEGRITY:unverified`, and `pcb serve` logs the
count (`unverified`, §15); the event is never silently accepted or dropped.
Scope of the marker [call, M3 final re-review]: it catches corrupted parts
and edits that break or strip our signatures, not a server that rewrites a
part's `Author` to an address not ours or adds an unsigned cleartext
attendees or personal part (those parts merge EXDATE, RDATE, ATTENDEE,
VALARM and missing single-value properties). Binding the author to the
signing key and refusing unsigned parts wait for E10 (`OPEN.md`,
`spec/experiments/E10-signatures.md`).
A part authored by an address that is not ours (an invitation, another
member of a shared calendar) is not verified in v1 and never marks the
event: there is no public-key lookup, and invitations and shared calendars
are out of scope (`OPEN.md`). An `X-PCB-INTEGRITY` a part carries itself is
dropped [verified: `TestObjectReads/UnverifiedSignatureIsMarked`,
`TestUnverifiedOnlyForOwnAuthoredParts`, `TestOpenTellsOwnAuthoredParts`,
`TestMarkIntegrity`]. Which parts are signed by which key [assumed → E10]:
if Proton signs a part of ours with another key than its author address's,
the event shows as unverified.

Member passphrase signature and the write gate [call] (research G, rulings
R17, M4-R1, M4-R39, M4-R40). A calendar is written only when its type is
normal, our member's `Permissions` has the Write bit (8 [assumed → E14]), the
member passphrase signature verifies, over the decrypted passphrase, with our
member's address keyring [assumed → E10], and both write keys exist (the
calendar key flagged primary, the primary key of our member's address).
Without the signature check the server could serve a calendar key it chose
and read what we encrypt to it. A signature served and checked that does not
verify, a Write bit off or another type closes the gate (read-only
privileges, 403 `need-privileges`); missing data (keys not unlocked, no
signature served, no write key) leaves it undecided (503 on a write, the last
decided privileges, none before any decision) [verified: `TestGateVerdicts`,
`TestUnverifiableSignatureLeavesTheGateUndecided`,
`TestUndecidedWriteGateIs503`, `TestPrivilegesFollowTheWriteGate`].

Session-key reuse on write [call] (research G, rulings M4-R14 to M4-R20,
M4-R30, M4-R35 to M4-R37). An update reuses a row's session keys and sends no
key packet (Proton keeps a row's key packets on update, research B.5). The
threat: the server substitutes a key packet, a key it chose wrapped to our
calendar key, and reads our update. Defences:
- **Primary, key-packet pinning (M4-R17, durable since M4-R30):** the cache's
  `pins` table keeps each row's `SharedKeyPacket`, `CalendarKeyPacket` and
  `AddressKeyPacket` as first stored, from every path that stores rows (sync
  passes, full listings, writes; rows already cached are pinned when the
  file is upgraded to schema 2). A pin is never replaced. A PUT's pre-write
  refresh compares each refreshed row with its pin (base64-decoded bytes)
  and refuses a difference with 403 `pcb:untrusted-key`, logged with the
  calendar, row and field only, without storing the refreshed rows. Pins
  outlive their rows and calendars and die only with a new cache epoch
  (M4-R35), so hiding a row once and returning it with new packets does not
  reset its pin. A row pcb creates is pinned with the packets pcb sent: by the
  row ID of Proton's answer, before the read-back (M4-R37), and the read-back
  must show those packets, else 503 (M4-R36). A DELETE checks no pin: it
  encrypts nothing (M4-R27) [verified: `TestChangedKeyPacketRefusesTheWrite`,
  `TestHiddenRowKeepsItsPin`, `TestEachKeyPacketIsPinned`,
  `TestCreatePinsTheSentPackets`, `TestPinRowsNeverReplacesAPin`,
  `TestPinsOutliveTheirRow`, `TestPinsDieWithTheEpochOnly`,
  `TestUpgradePinsTheStoredRows`].
- **Secondary, a signature proof (`cards.Event.SessionKeys`):** a key is
  reused only when every part encrypted under it verifies as ours and one of
  its shared or calendar encrypted parts (never the attendees part) proves
  it (`ical.ProvesKey`): Proton's fragment form (one VCALENDAR holding one
  VEVENT, and no VCALENDAR-level property but VERSION, PRODID or CALSCALE,
  M4-R20), the row's UID, and an encrypted-only property
  that the routing table puts in that very part. Otherwise
  `ErrUntrustedSessionKey`, and the write is refused (403
  `pcb:untrusted-key`). The proof rules out the texts the server holds with
  our signature [verified: `TestUntrustedSessionKeyIsRefused`,
  `TestProvesKey`]:
  - our cleartext signed parts: we never sign an encrypted-only property in
    clear. `cards.Seal` refuses a signed fragment holding one
    (`ErrMisroutedProperty`, M4-R16), and `ical.Patch` refuses a stored
    signed part holding one (403 `pcb:misrouted-stored-line`, M4-R19)
    instead of signing it or moving it into an encrypted part whose
    plaintext the server would then know [verified:
    `TestPatchSealReplayChain`, `TestPatchRefusesMisroutedStoredLines`];
  - attendees parts, whose plaintext (UID, DTSTAMP, ATTENDEE with tokens)
    the server knows, in their slot or replayed into another;
  - an `.ics` our address key signed outside the bridge (a Proton Mail
    invitation or RSVP, which carries METHOD at VCALENDAR level as iTIP
    requires, RFC 5546; a proof may not carry it);
  - another event's parts (the UID must be the row's).
- **Group checks:** a write to a group with any part of ours that is not
  `Verified`, unsigned parts of every kind included, or any part authored by
  an address not ours, is refused before planning (M4-R18, M4-R19; not the
  render's `unverifiedParts`, which spares unsigned attendees and personal
  parts), so an injected line or an unsigned cleartext attendees part never
  reaches `Patch`.
- **Confirmation by plaintext (M4-R32):** a write is answered success only
  when the rows read back open with the pinned packets (an update) or the
  packets sent (a create), every part is ours and verifies, and each part's
  plaintext is the one sent [verified: `TestReadBackComparesPlaintext`].

Residual risks, accepted (M4-R17, M4-R19, M4-R35 to M4-R37). Proton's own
clients reuse keys under the same trust model:
1. An encrypted plaintext the server can guess (a default title such as
   `New Event` with the UID and DTSTAMP it sees), confirmed offline with our
   public key against the part's clear detached signature, then replayed
   under a key it chose as the proof.
2. Rows whose key packet was already substituted when pcb first stored them:
   the pin is that first copy.
3. Content the server learnt elsewhere for the same UID, in the fragment
   form, signed by our address key (an invitation sent in clear to an
   external address).
4. Another client of the same account that signs an encrypted-only line in
   clear for that UID (a same-UID proof).
5. A create whose Sync call is ambiguous (the answer is lost, so no row ID)
   and whose read-back fails too leaves the first pin of that row to the next
   sync pass, which pins whatever Proton then serves (M4-R37: closing it needs
   a durable journal of pending creates). Likewise, when Proton's answer
   names no row, a create read back with other content and other packets is
   not recognised as ours: answered 503, and pinned from Proton on first
   sight.
6. A Sync answer that names a row ID pcb has not seen pins the packets pcb
   sent to that row (first seen wins). A lying server can only make that row
   read only for pcb (403 `pcb:untrusted-key`): denial of service, no
   disclosure.
7. Pins outlive their rows: the table grows slowly, a few hundred bytes per
   event ever seen, until a new epoch (pruning is parked, `OPEN.md`).
8. A legitimate key-packet change (should Proton or another client ever
   rewrite a row's key packets) makes that event read only for pcb: 403
   `pcb:untrusted-key`, logged, fail closed. Whether it ever happens is a
   live check (`HUMAN-CHECKS.md` §6, step 6.4.2).

Rows an update refuses rather than risk a substituted key (fail closed, 403,
M4-R19, M4-R20): a shared encrypted part holding only UID and DTSTAMP (an
untitled event), a calendar encrypted part holding only lines routed
elsewhere, a fragment carrying a VCALENDAR-level line other than VERSION,
PRODID or CALSCALE, a misrouted line stored in a signed part, a change that
needs a session key the row lacks (a first COMMENT on a row without a
calendar part, 403 `pcb:unsupported-change`, M4-R13), and (to be confirmed
live, `HUMAN-CHECKS.md` §6 W10) a UID containing `,`, `;` or `\` if Proton's
column holds it unescaped: the write path then compares UIDs unescaped, so such an
event's own edit is 503 `the event's UID is stored by Proton in another form than its
text` (never a 409 naming itself), and the same UID at another name is a 409
[verified: `TestAnEscapedUIDIsNeverItsOwnConflict`]; and a row that holds a personal part
(per-user data such as alarms stored before they moved to `Notifications`): 403
`pcb:personal-part`, because an update never re-sends that part and what Proton does to
an omitted one is live check L.4 #10; a DELETE is unaffected [verified:
`TestARowWithAPersonalPartIsNotUpdated`].

The full fix (`OPEN.md`): fresh session keys and their key packets on every
update, once live check L.4 #3 (research L.4) shows Proton accepts new key
packets on an update. That removes the reuse, so residuals 1 to 5 go, and
pinning with residuals 6 to 8 can go with it.

### 5.4 Calendar API surface (v1)
- Calendars: list, bootstrap (keys, passphrase, settings), user settings
  (default calendar), create/update/delete owned calendars, leave holidays
  [verified: proton-cal docs/api.md].
- Events: windowed query (4 `Type` buckets, ≤ 93-day windows, `More` cursor)
  [verified: proton-cal docs/api.md]; unscoped full listing paged with
  `Total` (no `More`) [verified: live probe 2026-11-10]; get by ID (422/2501
  for an ID of another calendar [verified: live 2026-11-10]); get by UID
  (`GET /calendar/v1/{cal}/events?UID=<uid>`, paged like the listing; M4's
  write refresh and read-back, `events.ListByUID`; that Proton matches the
  UID exactly [assumed → `HUMAN-CHECKS.md` §6 W1, W3]).
- Writes (M4): `PUT /calendar/v1/{cal}/events/sync` only [claim: proton-cal
  live; research A.1], body `{MemberID, Events: [{Overwrite: 0, Event}
  | {ID, Event} | {ID}]}` (create, update, delete). One call per CalDAV write,
  never sent twice (a 401 answered before processing is the one resend,
  M4-R23). Per-entry codes in a 1001 envelope; the client reads 2000
  (STATUS), 2001 (signature or exception SEQUENCE, not told apart, M4-R24),
  2011 (unsupported content), 2061 (unknown ID), 2501 (gone); a delete answers
  `Responses: []` [claim: research A.3, A.4; the fake implements them]. The
  `Event` body: the card parts, key packets on a create only, `Permissions`
  1, `Notifications`, `Color`, `Attendees`, `IsOrganizer`; never the
  plaintext time columns, `IsImport` or a personal part (`spec/mapping.md`
  §5).
- Bootstrap fields M4 reads: `Keys[].Flags` (bit 2: the primary calendar
  key, the only recipient of new key packets), `MemberPassphrases[].Signature`
  (R17, §5.3), `Members[].Permissions`.
- Member permissions and calendar type (normal 0, subscribed 1, holidays 2)
  [verified for types: proton-cal]; the Write bit of `Permissions` is 8
  [claim: caloxide; assumed → E14, `spec/experiments/E14-member-permissions.md`];
  the bits of view-only and edit shares are not needed (shared calendars are
  out of scope).

### 5.5 Change feed
Goal: incremental sync without re-listing. Source-based so far (third-party
clients, nothing Proton-documented; `spec/experiments/E04-calendar-feed.md`,
`E05-core-events.md`, evidence in `spec/experiments/raw/m2-feed-research.md`);
the live run is deferred (`spec/HUMAN-CHECKS.md`, "M2 live session").
- Calendar model events (M2 uses this): `GET /calendar/v1/{cal}/modelevents/latest`
  gives `{CalendarModelEventID}`; `GET /calendar/v1/{cal}/modelevents/{cursor}`
  gives `{CalendarModelEventID, More, Refresh, CalendarEvents:[{ID, Action, Event?}]}`.
  One feed per calendar [assumed → E04].
- **Action and Event are ignored** (M2-R3): every listed ID is re-read
  with `GET /calendar/v1/{cal}/events/{id}`; 404, or 422 with code 2501, means
  gone. Robust to unknown Action values and create-then-delete.
- Baseline cursor = `latest` read before the full listing; the cursor a feed
  read returns is stored in the same transaction as the row changes it
  produced [assumed → E04].
- The listing pages by offset, so a concurrent create or delete can shift a
  page [assumed → E04]: rows are deduped by ID and `latest` is read again
  after each listing. A listing is stable when `latest` did not move during
  it and its unique count equals the first page's `Total`; an unstable one is
  listed again, at most twice (M2-R17), and the first stable listing is the
  content. When none of the 3 is stable, the content is their union, and
  every cached row missing from all of them is re-read before it is
  tombstoned (gone: tombstoned; found: kept; unreadable: kept as cached, the
  rest stored, and the calendar carries a note). A quiet listing costs one
  extra `latest` and no row read.
- Full resync of a calendar when: never synced; `Refresh != 0`; the feed
  answers a 4xx other than 429 (cursor unusable); more than 50 pages in one
  pass; the last full sync is older than 24 h; a row re-read answers a
  non-gone 4xx other than 429/401 (M2-R8, so one unreadable row cannot stall the
  feed). Fallback resyncs (Refresh, refused cursor, M2-R8, more than 50 pages)
  are limited to one per calendar per 5 min (M2-R10); initial, `--full` and the
  24 h resync are not. A deferred calendar keeps its cursor, its feed is not
  read again until the gap is over (the pending cause is kept in the cache),
  then it is listed at once.
- Core event loop `/core/v4/events/{id}`: **not used in M2** (M2-R2). Whether it
  carries calendar list/membership changes is unverified in every source
  [assumed → E05]; the calendar list is re-read on every pass instead (one
  request).
- Push stream: reported 404 by caloxide; not used, polling only [assumed → E04
  checks it too].
Fallback if the feed proves unreliable: shorter full-resync interval, or a
windowed re-list plus a nightly full list [call].

### 5.6 Proton card codec
Model ⇄ Proton parts (shared signed, shared encrypted, calendar signed,
calendar encrypted, attendee parts, per-member personal notifications).
Which iCalendar property goes in which part on write is `spec/mapping.md`
(research B.1 [claim: proton-cal live, carbonate live]); what Proton keeps of
unknown properties [assumed → E13, `spec/experiments/E13-unknown-properties.md`].
`cards.Seal` is `cards.Open`'s inverse: `Open(Seal(x))` returns the sealed
parts byte for byte, signed, ours and verified [verified:
`TestOpenSealRoundTrip`]; the personal part is never written.

## 6. C0 — Model block

Decision 0012 (accepted 2026-11-11, supersedes 0006 for v1): the model is
**iCalendar-native**; there is no JSCalendar layer in v1.

- A decrypted Proton event is a set of iCalendar fragments (`internal/proton/cards`:
  shared signed, shared encrypted, calendar signed, calendar encrypted, attendees,
  personal). `internal/ical` merges them into one VEVENT line by line and keeps every
  property it does not interpret (`X-MOZ-*`, `X-PM-*`, unknown ones) byte for byte
  [claim: `internal/ical` has round-trip and fuzz tests, not re-listed here; the merge rules come from proton-cal and are live-verified only by its author [claim]].
  Row-level data joins in: `Color` to `COLOR`, `Notifications` to VALARMs (a `null`
  list means the calendar's default notifications), attendee `Status` to `PARTSTAT`
  [claim: `internal/service` render tests, names not re-checked].
- One CalDAV object per UID: master first, then exceptions by RecurrenceID; duplicate
  masters resolved to the most recently edited one, the others left out and counted
  (M3-R3). An event that cannot be decrypted or parsed is served as a placeholder
  VEVENT (`X-PCB-PLACEHOLDER:1`; the row's plaintext times and, for a master, its
  RRULE, so a series still shows every occurrence) rather than failing a report (M3-R4)
  [verified: `TestUndecryptableEventDoesNotFailTheReport`]. These counts live in the
  serving process only: `pcb serve` logs them (§15), `pcb status` does not show them
  (M3-R34).
- VCALENDAR: `VERSION:2.0`, `PRODID:-//pcb//proton-calendar-bridge//EN`,
  `CALSCALE:GREGORIAN`, CRLF, lines folded at 75 octets, **no VTIMEZONE** emitted:
  Thunderbird resolves TZIDs from its own database [verified: E08 source, E11]; other
  clients may want generated VTIMEZONEs, which stay unbuilt [call].
- JSCalendar (0006) returns with the first adapter that is not iCalendar-native, as a
  codec over this model. Cost if wrong: that layer is added later.
- Recurrence: no expansion in M3 (no `calendar-query`, no free-busy); expansion in
  `ical.Expand`, our own expander over Proton's set (E16).
- Fuzz tests on every parser (`internal/ical`, `internal/caldav` request bodies)
  [verified: `FuzzReportBody`, `FuzzPropfindBody`]; golden files from real Proton,
  Thunderbird and Apple exports are not yet collected.

## 7. C2 — Store block

### 7.1 Layout
`$XDG_STATE_HOME/pcb/` (`/var/lib/pcb/` for a system service, `/data/state` in
the container). M1 to M5 (one account at the root; since M6 an existing one is moved,
"Layout v2" below): `pcb.db` (SQLite, WAL), `secrets.sealed`, `state.json`,
`state.lock`, `clients.json`, `tls/`, `pcb.sock`, `logs/`. Config in
`$XDG_CONFIG_HOME/pcb/config.toml` (`/etc/pcb/`, `/data/config`). Directories
0700, files 0600; `doctor` checks them.

M5 additions:
- `PCB_CONFIG_DIR` and `PCB_STATE_DIR`, when set, are used verbatim (no `/pcb`
  suffix) and must be absolute: a relative value is refused, naming the variable
  (M5-P1). The units and the image set them; the user unit takes the directories
  resolved at install time, since the user manager does not see the shell's XDG
  variables (M5-P18) [verified: `TestStateDirOverride`, `TestConfigDirOverride`,
  `TestRelativeOverrideIsRefused`, `TestUserUnitGolden`, `TestSystemUnitGolden`].
- `pcb.sock` is the control socket of `pcb serve` (§15), mode 0600. Its directory
  must grant group and other nothing, else `pcb serve` exits 20. A path over 107
  bytes (103 on macOS and the BSDs) is refused, exit 20, hint `set PCB_STATE_DIR
  to a shorter path`. A stale socket is replaced without a probe: `pcb serve`
  holds `sync.lock` before binding, so no other daemon owns it (M5-P2)
  [verified: `TestSocketPath`, `TestSocketIsPrivate`, `TestLongSocketPathIsRefused`,
  `TestServeRefusesATooLongSocketPath`, `TestServeRefusesAnOpenStateDir`,
  `TestStaleSocketIsReplaced`, `TestNonSocketFileIsRefused`].
- `state.lock` is the leaf lock held by every writer of `state.json` (§7.3, R10m).
- Root guard (initial-version audit): a command that opens the state, run as root (euid 0) while the state
  directory belongs to another user (a system install), exits 10 `the state directory belongs to <user>: run
  pcb as that user`, hint `sudo -u <user> pcb …`, instead of leaving root-owned files the `pcb` user cannot
  read. A missing directory passes; `pcb doctor` (read only) and `install`/`uninstall` are not guarded
  [verified: `TestRootRefusesAnotherUsersState`].
- Stale files: at each start, regular `.tmp-*` and `.key-*` files older than 1 h left by a crash in the state
  and config directories (and `.key-*` only in a key file's own directory) are removed; links are never followed
  [verified: `TestRemoveStaleRemovesOnlyOldTemporaryFiles`, `TestRemoveStaleNeverFollowsSymlinks`,
  `TestOpenRemovesStaleTemporaryFiles`, `TestOpenSweepsOnlyKeyFilesInTheKeyDirectory`].

Layout v2 (M6, decision 0014; `internal/store/layout.go`, `internal/service/hub.go`):

```
<state>/
  serve.lock  sync.lock  layout.lock  sealer.lock  login.lock  last-login  layout.json  pcb.sock  tls/  logs/
  accounts/<local-id>/
    state.json  state.lock  secrets.sealed  secrets.lock  clients.json  clients.lock  pcb.db (+ -wal, -shm)  sync.lock
```

- `<local-id>`: 8 random lowercase hex characters, drawn when an account is added and never reused
  or renamed (it is a directory name and appears in the CalDAV URL, §9). Proton's user ID stays inside
  (`state.json`, the cache's `meta`) and is checked as before [verified: `TestLocalIDShape`].
- `layout.json` is `{"version": 2, "sealer": "keyfile" | "secret-service"}` since M7 (the sealer §7.3 chose when the state was created); a missing `sealer` (every M6 file) means
  `keyfile`, and any other value (an empty one included) is `ErrUnknownSealer`: exit 10, hint `upgrade pcb`, like a newer layout
  [verified: `TestLayoutWithoutSealerIsKeyFile`, `TestUnknownSealerIsRefused`, `TestUnknownSealerIsMustActUpgrade`, `TestAccountsOfAnUnreadableLayout`].
  `store.CreateLayout` writes it (with `accounts/`) for a new state, exclusively (O_EXCL, fsynced): an existing `layout.json` is never overwritten, so the first sealer stays;
  every `Migrate` path (a moved M5 state, a moved account) writes `keyfile`, since an existing state was sealed by a key file (ruling P2)
  [verified: `TestCreateLayoutWritesTheLayoutAndAccounts`, `TestCreateLayoutNeverOverwrites`, `TestCreateLayoutRefusesAnUnknownSealer`, `TestMigrationRecordsKeyFile`, `TestFreshStateHasNoLayout`].
  Anything else is refused (malformed or an unknown version: damaged,
  exit 10 `run pcb doctor`; a version above 2: `ErrNewerLayout`, exit 10 `upgrade pcb`, ruling R9; a layout pcb
  cannot read, exit 10 naming the file: a person must act, so a unit that does not restart on 10 stops instead of
  looping, ruling R36) [verified: `TestOpenHubRefusesDamagedLayout`, `TestOpenHubRefusesUnreadableLayout`].
  Root files: `serve.lock` (the serving process, holds its PID, §4.1), `sync.lock` (the root lock, kept
  under its M5 name, ruling R7: held by the serving process, and by `OpenHub` while it migrates), `layout.lock`
  (serialises migrations), `sealer.lock` (M7: held while the first seal creates the keyring item, §7.3), `login.lock` and `last-login` (the login guard, shared by every account).
  `logs/pcb-ui.log` and `logs/pcb-ui.log.1` are `pcb-ui`'s own log (§15).
- **Migration.** A state with no `layout.json` and account data at the root (`state.json`, `secrets.sealed`,
  `clients.json`, `pcb.db`, `-wal`, `-shm`: data files only, locks do not count, R6) is the M5 layout. The first
  command that opens the state (`pcb doctor` does not: it reads both layouts and never writes)
  moves the account's files into `accounts/<new-id>.migrating/`, renames the directory,
  then writes `layout.json`, each step fsynced; a start that finds the staging directory (or the account
  directory without `layout.json`) resumes with the same ID. It holds `layout.lock` and the root `sync.lock`
  without waiting: an M5 `pcb serve` or `pcb sync` running makes it exit 20 `another pcb holds the state`,
  hint `stop the running pcb first`; another pcb migrating (`layout.lock` held) is exit 20 too, `another pcb is
  moving the state to the per-account layout` (both may pass by themselves, R36)
  [verified: `TestOpenHubRefusesWhileM5SyncLockHeld`, `TestOpenHubRefusesWhileLayoutLockHeld`]. The root `sync.lock` is not moved (R7: it is v2's root lock); the
  other lock files move with the account. A root `state.lock`, `secrets.lock` or `clients.lock` found beside
  `layout.json` later (an older binary recreates them when it opens the state) is stale and harmless: lock
  files hold no data and never count as an older pcb's account files (R6)
  [verified: `TestLayoutRootLockFilesAreNoAccount`]. Session, event cache and CalDAV passwords come across untouched; a migrated M5 account has no
  address in its `state.json`, so `OpenHub` backfills it from the sealed account's username (R16)
  [verified: `TestMigrateMovesM5Files`, `TestMigrationResumesAfterEachStep`,
  `TestMigrateLeavesTheRootSyncLock`, `TestMigrateSyncsTheDirectories`, `TestOpenHubMigratesM5State`,
  `TestOpenHubBackfillsTheM5Address`, `TestMigratedThunderbirdKeepsItsTokens`,
  `TestAccountsOfAnM5StateLeaveItAsItIs`, `TestAccountsOfAnInterruptedMigration`].
- **No downgrade.** An M5 binary cannot be changed: on a v2 state it sees no root `state.json` (logged out) and,
  if it logs in, writes `state.json` and `secrets.sealed` at the root beside `layout.json`. M6 then refuses
  (exit 10, `account files of an older pcb are beside the per-account layout: <names>`, hint `remove them or
  move the state directory aside: this pcb version cannot be downgraded`, R36) and never guesses which copy is current; `pcb doctor`
  reports it on its `accounts` check. The root `sync.lock` held by a serving M6 process stops an older binary from
  writing meanwhile [verified: `TestLegacyBesideLayoutIsRefused`, `TestOpenHubRefusesLegacyBesideLayout`,
  `TestAccountsWithLegacyFilesBesideTheLayout`, `TestOpenHubRefusesNewerLayout`, `TestLayoutUnknownVersionIsRefused`].
- `state.json` gains `address` (the account's Proton address, kept after a sign-out so a signed-out account
  keeps its name; not a secret; the design wrote `username`, ruling R8), `caldav` (the CalDAV switch, §4.1; absent
  is on) and `created` (when the add began, for the 24 h pending expiry) [verified: `TestStateKeepsAddressAndSwitch`].
- The sealer is shared: every account's `secrets.sealed` and `clients.json` are sealed with the one key
  (`[secrets] key_file`, or the one keyring item since M7, §7.3); `secrets.sealed` keeps its v2 format and its generation mirror in the account's own
  `state.json` (§7.3).

### 7.2 What is cached [call]
What M2 stores, in `pcb.db` (SQLite, WAL, mode 0600, schema version 2 in
`PRAGMA user_version` since M4, which adds `aliases` and `pins` and migrates an
M3 file in place; `sync.lock` beside it), all keyed by account ID (the
cache records its account in `meta` and is emptied when opened for another):
- `meta`: the account ID, the change counter and its epoch: 128 random bits
  (hex) drawn when the file is created and again when it is emptied for
  another account (M2-R14). The counter restarts only under a new epoch, so M3's
  CTags and sync tokens are `epoch:seq` and never recur with another content,
  even after `pcb logout` removed the file.
- `calendars`: ID, name, type, feed cursor, last full sync time, last sync
  time, last error, and the cause of a deferred fallback resync.
- `events`: per row, calendar ID, event ID, UID, start, end, full-day flag,
  recurrence ID, RRULE, `LastEditTime`, ETag, a change sequence number and
  `raw`, the row exactly as Proton returned it (ciphertext parts and
  plaintext fields). Plus `tombstones` (deleted IDs with the row's UID and their
  sequence, indexed by calendar and sequence) so a later sync-token read can
  report deletions; CalDAV resources are per UID, so a deletion is
  reported as a 404 at the UID's served name (M2-R16, §9). `Changes(calendar, since)`
  reads the writes and tombstones after a sequence number, in order.
- `aliases` (M4-R6, M4-R22): (calendar, resource name) → UID, one name per
  UID, for a UID a client created at a name of its own; an alias outlives the
  UID's deletion (a tombstone is reported at the name the client knows) and
  dies with the calendar's removal and a new epoch.
- `pins` (M4-R30, M4-R35): (calendar, event ID) → the row's three key packets
  as first stored (§5.3); never replaced, outliving rows and calendars, gone
  only with a new epoch.
- Not stored: members, settings, key material (the key chain stays in
  M1's sealed store and is re-derived per session).
- **Nothing decrypted is ever written.** Event decryption, the iCalendar merge
  and E09 (decrypt cost per 1 000 events, cold start) moved to M3 (M2-R1).
  E09's offline measurement (0.2 to 0.4 ms per event, signatures checked
  [verified: `BenchmarkOpen`]) says no sealed decrypted index is needed.
  Rendered objects live in an in-memory LRU rebuilt on demand: **fixed at 2 000
  entries** (`lruCapacity`, not configurable), counted by objects not bytes and shared by
  every calendar, so a long DESCRIPTION or a large series costs more than a short event, and
  accounts holding more than 2 000 UIDs in all re-render on a full resync (read-M6, accepted: the render is
  0.2 to 0.4 ms per event); the live cold-start figure is pending.
- Durability (initial-version audit): `PRAGMA synchronous=FULL`, so a power cut cannot roll the
  change counter back inside an epoch (cost measured on 5 calendars of 1 000 events: an incremental pass
  18 to 23 ms with NORMAL, 22 to 33 ms with FULL; the initial pass is unchanged); the 1 → 2 migration (DDL,
  `user_version`, the pins backfill) is one transaction, and a v2 file that has rows and an empty
  `pins` table is backfilled when opened; `pcb.db` and its `-wal`/`-shm` are chmodded 0600 on open; the directory
  is fsynced after the cache file or `secrets.sealed` is removed [verified: `TestOpenSyncsEveryCommit`,
  `TestMigrationStoppedHalfWayIsRedone`, `TestOpenBackfillsAnEmptyPinsTable`, `TestOpenRepairsTheFileModes`,
  `TestRemoveSyncsTheDirectory`, `TestWipeSyncsTheDirectory`].
- Read-only opens (`pcb status`, `pcb event list`) never create or migrate
  the file; with no `-wal` present they read it as `immutable=1`, which may
  return a stale or inconsistent snapshot if a writer starts meanwhile. Fine
  for one-shot CLI reads; **M3's CalDAV read path must not use it** and opens
  a normal shared-lock reader instead.
- Errors: a corrupt cache is "you must act" (exit 10, hint: remove `pcb.db`,
  then `pcb sync --full`); a permission or disk error is blocked (exit 20)
  with the OS reason (M2-R13), whether at open or during any read or write
  (SQLite's result code is classified on every operation). A cache failure
  during a sync stops the whole pass, never just one calendar.
  `pcb doctor` checks the file and its mode.

### 7.3 Secrets
Sealed with a data key held by, in order, per environment (what each backend
protects against: `spec/experiments/E15-secret-sealing.md`):
- Desktop user service: 1. Secret Service (unlocked keyring of an interactive
  login; built in M7); 2. user-scope `systemd-creds` (not built); 3. key file 0600.
- Server system service: 1. system-scope `systemd-creds` (host key); 2. key
  file 0600 on a separate path, `doctor` warns.
- Container: 1. compose secret file at `/run/secrets/<name>`; 2. key file 0600
  on a volume separate from the data.
Built: the key file since M5 (`[secrets] key_file`, default `<config>/secrets.key`, exactly 32 random bytes,
refused when group or other can read it) and the Secret Service sealer since M7 (below); `systemd-creds` is not built, and in a container the
compose secret is just the path `key_file` points at (README, "Run in a container"). `doctor` reports the backend chosen (`sealer:` line, §15). Host-key `systemd-creds` is no stronger than
a key file against root or a same-user process; only TPM2 binding, or a keyring encrypted
under a strong login password (untested), helps against offline theft. [verified: E15 for user-scope systemd-creds, Secret Service presence
(interactive session), key file; TPM2 and desktop system scope untested]
**The Secret Service sealer (M7, decision 0015; `internal/store/seal`, pure Go over D-Bus with godbus).**
- Name `secret-service`. The key is one item of the default collection (the `default` alias): label `pcb sealing key`, attributes `application=pcb` and
  `state=<absolute path of the state directory>`, secret 32 random bytes, content type `application/octet-stream`, D-Bus session algorithm `plain` (the session bus is local).
  One item per state directory serves every account in it; two state directories get two items, and a blob sealed under one is `ErrCorrupt` under the other. The key file's format is unchanged
  [verified: `TestDBusKeyringStoresOneItemPerStateDir`, `TestSecretServiceRoundTrip`, `TestSecretServiceRefusesAShortKey`, `TestDBusKeyringSetReplacesTheSameAttributes`, `TestKeyFileUnchangedByTheHelper`].
  Its name is in the AAD of `secrets.sealed` and `clients.json` like the key file's, so a file sealed by one sealer is not opened by the other even with the same key bytes [verified: `TestSecretServiceBindsItsName`].
- **Choice, once.** `[secrets] backend` (§14) is `auto` (default), `keyfile` or `secret-service`. It is read only when a state is created (`OpenHub` finds no `layout.json`, no account, no M5 file and no `.migrating`
  directory): `keyfile` never asks the keyring; `auto` picks `secret-service` when `DBUS_SESSION_BUS_ADDRESS` is set, a Secret Service answers within 2 s and the default collection is unlocked, else `keyfile`
  (no bus, no service, a hung service or a locked keyring at that moment all give `keyfile`); `secret-service` refuses the creation, exit 10 `[secrets] backend is secret-service but the desktop keyring cannot be used: <cause>`,
  hint `unlock the keyring and start a Secret Service (GNOME Keyring, KWallet), or set [secrets] backend to keyfile`. The choice is recorded in `layout.json` (§7.1) and never probed again: an existing state keeps its sealer whatever `backend` says,
  and an M5 or M6 state keeps its key file. An invalid `backend` is exit 10 `[secrets] backend must be auto, keyfile or secret-service`, hint `fix <config path>` (ruling R2: the plan said 2, but every config fault is 10)
  [verified: `TestBackendDefaultsToAuto`, `TestBadBackendIsRefused`, `TestFreshStatePicksTheUnlockedKeyring`, `TestFreshStateWithoutKeyringUsesKeyFile`, `TestFreshStateWithLockedKeyringUsesKeyFile`,
  `TestBackendKeyFileNeverAsksTheKeyring`, `TestBackendSecretServiceWithoutKeyringIsMustAct`, `TestExistingStateKeepsItsKeyFile`; the real GNOME keyring `[assumed → step 9]`].
  The first command that opens a fresh state decides for good: a `pcb status` over SSH without a session bus fixes `keyfile`, and `pcb doctor` (which never writes) says `not chosen yet` until then (§15).
  Moving a keyring-sealed state to a host without that keyring leaves `secrets.sealed` unopenable (the messages name the keyring); no command re-seals from one sealer to the other.
- **`sealer.lock`.** The keyring's `CreateItem` replaces an item with the same attributes, so two processes creating the key at once could overwrite a key the first had already sealed with, losing its data.
  The first seal therefore runs under an exclusive lock on `<state>/sealer.lock` (30 s wait; a no-op off Unix, where the first-seal race remains), re-reads the stored item after writing and uses what stands
  [verified: `TestSecretServiceFirstSealsAgreeOnOneKey`: without the lock it failed 5 runs in 5].
- **pcb never prompts.** It reads the collection's `Locked` property first and never calls `Unlock` or a prompt; a prompt path the service returns is `ErrLocked` too. A missing service, a connection failure or no answer within 2 s is `ErrNoService`;
  an unset bus address is `ErrNoService` as well (godbus's `dbus-launch` fallback is never used)
  [verified: `TestSecretServiceLockedIsErrLocked`, `TestDBusKeyringLockedNeverPrompts`, `TestSecretServiceMissingItemIsErrNoKey`, `TestDBusKeyringMissingItemIsErrNoKey`, `TestProbeSucceedsOnAnUnlockedService`,
  `TestProbeFailsWithoutService`, `TestDBusKeyringHungServiceIsErrNoServiceWithinTheBound`].
- **A locked keyring waits.** A keyring that is locked (a user service started before the desktop login, or a lock in Seahorse) is neither a park nor a login failure: `Status.Secrets` is `locked`, the command that needs the secrets says
  `the desktop keyring is locked` (exit 20, hint `unlock it (log in to the desktop session), pcb retries every 30 s`; the hint also shows on a one-shot command, which does not retry), and the account's serve loop waits and asks again every 30 s,
  also when the lock comes mid-serve (a token refresh that must seal), with the account alert `keyring-locked` (§15). While it waits pcb makes **no Proton login** (the stored login is unreadable, so neither a relogin nor an unattended
  relogin can start), re-seals nothing with another backend, and recovers by itself, with no Restart, when the keyring unlocks. A locked wait does not end `pcb serve` (with or without `--wait-for-login`) or `pcb sync --watch`
  [verified: `TestLockedKeyringWaitsAndRecovers`, `TestKeyringLockedWhileServingWaitsAndRecovers`, `TestLockedKeyringDoesNotEndServeWithoutWaitForLogin`, `TestWatchSyncSurvivesALockedKeyring`, `TestKeyringLockedAlert`;
  a real lock in Seahorse `[assumed → step 9]`].
- **No keyring answering is not a lost session** (ruling R15). On a keyring state with no Secret Service on the session bus (or no bus: `pcb` over SSH, a service outside the desktop session), `ErrNoService` is
  `the desktop keyring is not available` (exit 20, hint `run pcb inside the desktop session that holds the keyring (pcb doctor checks it); [secrets] backend only chooses for a new state`), never a logout or permissions hint;
  `Status.Secrets` is `keyring-unavailable`, the phase is kept (no `needs-login`), the account alert is `keyring-unavailable` (§15) and `pcb-ui` shows `keyring unavailable`. Unlike a locked keyring, the serve loop does not wait on it
  [verified: `TestKeyringUnavailableIsNotANeedsLogin`, `TestNoKeyringServiceIsNotANeedsLogin`, `TestShortTexts`, `TestNotifyCodes`].
- **A login checks the keyring before SRP** (ruling R16): on a keyring state (a new state's sealer is chosen when it is created, so this covers it too) `Login` asks the keyring first (`Locked`, the service present, 2 s) and returns
  `the desktop keyring is locked` or `the desktop keyring is not available` without any Proton call: no SRP, no Proton login and no login-guard slot is spent on a seal that cannot succeed, and an add leaves no account
  [verified: `TestLoginChecksTheKeyringBeforeSRP`].
- **Messages name the keyring, not a key file**, on a keyring state: `the sealing key is not in the desktop keyring` and `the stored secrets cannot be opened with the key in the desktop keyring` (both hint `pcb logout then pcb login`);
  `pcb client add` and `rotate` hint `check that the desktop keyring is running and unlocked (pcb doctor)`; `pcb client show` says `the desktop keyring has no key for it` with a restore hint; a file error on the state is
  `cannot use the stored secrets: <cause>`; `Status.Sealer` is `secret-service` and `Status.KeyFile` is empty. The key-file wording is unchanged on a key-file state
  [verified: `TestSecretServiceStateNamesTheKeyring`, `TestSecretServiceStateWordsNameTheKeyring`, `TestStoreErrorPathErrorWording`].
- What it protects (E15): the key is not on disk beside the data, so a copied state directory, a backup or a stolen disk is useless without the user's keyring. A process of the same login session can read the item (Secret Service has no per-application access control),
  so against a same-user process it is no stronger than the key file; against offline theft it relies on the keyring being encrypted under the login password (untested). `pcb uninstall --delete-data` removes the item when `layout.json` records `secret-service` (ruling R13): a step `remove keyring <state>` after the state directory deletes that state's item and no other; a locked or absent keyring
  does not fail the deletion, the step's note names the cause and how to delete the item by hand in Seahorse (`pcb sealing key`, attributes `application=pcb`, `state=<dir>`), and a state directory that is kept keeps its item
  [verified: `TestUninstallDeleteDataRemovesTheKeyringItem`, `TestUninstallDeleteDataWithoutTheKeyringSaysHowToRemoveTheItem`, `TestDBusKeyringDeleteKeyRemovesOnlyThisState`; the real keyring `[assumed → step 9]`];
  `pcb account remove` keeps it, since the key belongs to the state, not to one account [verified: `TestAccountRemoveKeepsTheKeyringItem`].
Restoring an older `secrets.sealed` is detected (M5, R3m): format version 2 seals a monotonic generation with the accounts under AAD `pcb-secrets/v2/<sealer>`, mirrored as `secrets_generation` in `state.json`; a sealed generation below the mirror is refused (`pcb status` shows `rolled-back`, `pcb logout` wipes it without revoking, then `pcb login`). Removing the file (logout) moves the mirror one above, so a removed file is refused if restored. A file ahead of the mirror is accepted (a crash between the two writes). Restoring the whole state directory, `state.json` included, is not detected. A v1 file reads as generation 0 and is rewritten as v2 at the next write. Every writer of `state.json` holds the leaf lock `state.lock` (R10m; an in-process mutex off Unix).
Keys and tokens opened in M1 are exercised offline only: whether a real Proton session accepts them stays `[assumed]` until the live run.
Sealed content: session tokens, salted key passphrase, optional unattended
relogin credentials, and, since decision 0013, a copy of each CalDAV client password (below).
CalDAV client passwords are kept twice in `clients.json` (format version 2): a salted SHA-256 for the check, and the
password sealed with the same key under the AAD `pcb-clients/v2/<sealer>/<client id>` so `pcb client show` can print
it again (decision 0013, §9). `pcb logout` and a rollback wipe `secrets.sealed` but keep the key file, so those copies
survive them; only `pcb uninstall --delete-data` removes the key (§13) [verified:
`TestPasswordStaysRecoverableAcrossLogout`, `TestRevealRefusesAnotherClientsAAD`]. With no key file but with
`secrets.sealed` present, `pcb client add` and `pcb client rotate` refuse (exit 10, naming the key file; the fix is
`pcb logout` then `pcb login`), because minting a key would turn "key missing" into "secrets corrupt" [verified:
`TestClientsRefuseToMintAKeyWhileTheSessionsKeyIsMissing`]; the stat-then-seal window (a key deleted between the check
and the seal) is accepted (`spec/OPEN.md`, "Parked by M5").
A `secrets.sealed` this pcb cannot open is told apart from a damaged one (initial-version audit): written by a
newer pcb (envelope version above 2) or sealed by another sealer. `Status.Secrets` is then `unsupported`
with the cause as its reason, every command that reads it exits 10 with hint `upgrade pcb`, and `pcb serve` stops
with 10. `state.json` with a field this pcb does not know is "written by a newer pcb" (exit 10, `upgrade pcb`, not
"logout"); the decoding stays strict. `pcb logout` cannot revoke a store it cannot read, so it refuses
(exit 10, nothing touched) and says `end the session in the Proton web app (Settings → Security → Sessions), then
run pcb logout --yes`; on a terminal it asks to type `discard`, and `--yes` discards the local files without
asking. The logout also removes the event cache under `sync.lock` taken without waiting; when another process holds
it (a one-shot `pcb sync`, or a serve that is not this process) the logout still completes and says so
(`sync_running` in `--json`: run `pcb logout` again once it stops) [verified: `TestUnsupportedStoreNamesTheCause`,
`TestStateWithUnknownFieldsIsNewer`, `TestNewerStateSaysUpgrade`, `TestLogoutOfAStoreItCannotReadNeedsConfirmation`,
`TestLogoutOfAStoreItCannotReadAsksOnATerminal`, `TestLogoutRefusalKeepsAPendingLogin`, `TestLoginOverAStoreThisPcbCannotRead`,
`TestLogoutKeepsAStoreItCannotRead`, `TestLogoutSaysWhenAnotherProcessSyncs`]. The control socket's
`POST /v2/accounts/{id}/logout` (`/v1/logout` before M6) takes an optional body `{"discard": bool}`; an empty body is the zero request, so an older pcb
still works (§15).
M5 verified the generation and the lock: [verified: `TestGenerationIncrementsAndIsMirrored`, `TestRestoredOlderSecretsAreRefused`,
`TestFileRemovedByLogoutIsRefusedIfRestored`, `TestV1FileIsReadAndUpgraded`, `TestSealedAheadOfMirrorIsAccepted`,
`TestEditedVersionFieldDoesNotDowngrade`, `TestUpdateStateWaitsForTheLock`, `TestHumanNeededDoesNotOverwriteAFinishedLogin`,
`TestRolledBackSecretsNeedALogin`, `TestLogoutWipesRolledBackSecrets`]. On a rollback the session pcb holds in memory
is revoked best effort before it is dropped (M5-R3). Inside a systemd service and a container: the smoke tests never log in,
so no sealed session was exercised there; that is the live step 7 (`spec/experiments/E15-secret-sealing.md`) [assumed → step 7].

## 8. C3/C4 — Service and sync

### 8.1 Sync engine
- Initial sync (delivered in M2): list calendars (re-read on every pass), then
  each calendar's full row set (unscoped listing, page size 100: Proton
  refuses more with 400/2021 [claim: proton-cal docs/api.md:312]) after
  reading `latest`, then follow the feed (rules in §5.5).
- Incremental (delivered in M2): `pcb sync` is one pass; `pcb sync --watch`
  repeats every `--interval` (default 30 s, at least 10 s, a smaller value is
  refused with exit 2) until Ctrl-C (exit 0, M2-R12) or until only a person can
  fix what failed: log in again or a damaged cache (exit 10), a cache written
  by a newer pcb or a system without file locks (exit 20). Session-wide
  failures (429, 5xx, network, session ended) stop the pass and, in watch,
  back off x2 up to 10 min with jitter, or wait the 429's `Retry-After` when
  longer (up to 1 h; the one-shot `pcb sync` exits 20 with "retry in X";
  the watch prints "next try in X" after a failed pass instead, and the
  error it stops on once); any
  other per-calendar failure is recorded on that calendar, the others
  continue, and the pass exits 40 (partial); in watch a partial pass is
  reported but does not back off (the other calendars keep the interval). A calendar deferred by the
  5-minute fallback gap also counts as partial, and the message names the
  cause (Proton asked to refresh, refused the cursor, refused a row, feed too
  long) and when it retries (M2-R11). All requests go through the one per-account client and its
  limiter. Tuning after E06 stays open; the daemon's idle slow-down is below.
  `--full` lists every calendar again (with `--watch`, until one pass gets
  through, then the feed is read); a re-read row whose ETag is unchanged
  changes nothing (no counter bump).
- Daemon cadence (M5, `pcb serve`; M5-Rpre9, P6, R1): a pass every `ActiveInterval` 30 s, every `IdleInterval` 5 min
  once idle. Idle means no request authenticated by `CheckClient` for `IdleAfter` 35 min since Serve began serving;
  35 min is longer than Thunderbird's default 30-minute refresh, so a running Thunderbird keeps 30 s (E08 §3.7).
  A refused password is no activity. The first authenticated request while idle starts a pass at once; a wake never
  shortens a back-off or a `Retry-After` wait. Back-off is unchanged (x2 up to 10 min, +-10 %). A partial pass counts
  as a success for the cadence (M5-R1): only a pass in which no calendar synced backs off, and the failing calendars
  show in `pcb status` and as `sync-stalled` alerts; `pcb sync --watch` shares the rule [verified:
  `TestIdleServeSlowsToFiveMinutes`, `TestAuthenticatedRequestWakesAnIdleLoop`, `TestRefusedPasswordIsNotActivity`,
  `TestWakeCutsOnlyASuccessSleep`, `TestWakeDuringBackOffDoesNotShortenIt`, `TestPartialPassKeepsTheCadence`,
  `TestWatchKeepsGoingAfterPartial`, `TestSetIntervalReplacesTheActiveCadence`]. Live: that Thunderbird at its
  default settings keeps the active cadence and that reopening it brings a pass within seconds [assumed → step 7.4].
  The standby idea is caloxide's (README, Credits).
- Waiting for a login (M5, `pcb serve --wait-for-login`, the default of the units and the image; M5-Rpre3, R8, R9,
  R10, P8, P9, P22): CTL and the CalDAV listener bind at once, and while there is no usable session every
  authenticated CalDAV request that reads or writes calendars answers 503 `Retry-After: 30` with no warning line;
  authentication still comes first (no credentials: 401). The principal, the root and `/.well-known/caldav` call no
  service and answer 207 or 308; a GET on a collection is 405, as when serving. A login is noticed at once when it
  completes in the daemon, and by reading `state.json` every 5 s when another process's `pcb login` wrote it. Serve
  goes back to waiting only for a stop a login fixes (not logged in, session ended or revoked, needs login or CAPTCHA,
  secrets rolled back); any other stop needing a person (a damaged cache, a config error) ends `pcb serve` with exit 10,
  and the units carry `RestartPreventExitStatus=10`. Until the first successful pass after a cache opens, an unknown
  calendar answers 503, not 404. `pcb logout` during Serve stops the sync loop and closes the cache before the wipe
  (M5-P10). A second SIGINT or SIGTERM while stopping exits 20 at once (`a second signal cut the stop of pcb serve`)
  and the units set `TimeoutStopSec=30s` (M5-P22). Without the flag, M3's behaviour stays (exit 10 logged out,
  Proton's code at a session end) [verified: `TestServeWaitsForALogin`, `TestEveryReadAndWriteWaitsForTheLogin`,
  `TestServeNoticesALoginFromAnotherProcess`, `TestSessionEndWaitsInsteadOfExiting`, `TestLoginFixableStopsWait`,
  `TestDamagedCacheEndsServeUnderTheFlag`, `TestServeWithoutTheFlagKeepsM3`, `TestLogoutDuringServeClosesTheCacheFirst`,
  `TestWaitingForLoginIs503WithoutAWarning`, `TestServeWaitForLoginAnswers503ThenServes`, `TestSecondSignalForcesExit`].
- Reads are served from the cache; a read never waits for Proton unless the
  calendar was never synced (M2: `pcb event list` for a calendar the cache
  does not hold reads Proton's calendar list once, to tell "not synced yet,
  run `pcb sync`" from "no such calendar"). First use is explicit: `pcb sync`
  on an account without calendars says so, `pcb status` logged in but never
  synced shows `sync: never (run pcb sync)`, and `--interval` without
  `--watch` is refused (exit 2).
- Writes (M4) go to Proton synchronously, during `pcb serve` only, and the
  client gets the real result; no offline write queue in v1 [call]. The flow
  of a PUT or DELETE (ruling M4-R5, amended by M4-R31, R32, R34, R36, R37,
  R42 to R45; `internal/service/write.go`):
  1. Resolve the resource name to a UID (its alias, else the UID whose
     canonical or alternative name it is; §9) and parse the body
     (`spec/mapping.md`; a refusal here sends nothing).
  2. The write gate (§5.3; 403 `need-privileges` when closed, 503 when
     undecided, M4-R39).
  3. Take the calendar's lock: one write of a calendar at a time, never
     beside the syncer's pass of that calendar. Waiting ends with the
     request's context or the pre-send budget (503). Steps 2 to 6 (the
     keys and the gate, the lock, the refresh, the checks and the plan)
     run within a 20 s budget (M4-R42): past it the write answers 503 and sends nothing,
     so no Sync call leaves when its answer could no longer reach the
     client.
  4. Refresh the UID's rows from Proton (`ListByUID`; rows of another UID
     in the answer are ignored and logged by ID, M4-R45). A PUT compares their
     key packets with the pins (403 `pcb:untrusted-key`, §5.3); then the rows
     are stored (`ApplyWrite`: the change counter moves, the feed cursor does
     not).
  5. Conflicts and the precondition, on that fresh state: the UID served at
     another name, or the name serving another live UID → 409 (UIDs compared
     unescaped: the same event at its own name is never a 409 against itself, it is
     503 `ErrUIDStoredOtherwise`, §5.3); `If-Match` (`*`, or a comma-separated
     list, several headers being one list, compared strongly: a weak `W/` tag never
     matches; present but blank → 412 before the service is asked) and
     `If-None-Match` (`*`, or ETags compared weakly) on the fresh object ETag → 412
     [verified: `TestIfMatchListsCompareStrongly`, `TestIfMatchListsReachTheService`,
     `TestPutRequestChecksComeFirst`].
  6. Plan the row operations (`Plan`, `PlanDelete`; refusals are 403 with a
     pcb precondition, `spec/mapping.md` §8) and seal them. A plan with no
     operation answers 204 without calling Proton. A create at a name that is
     not the UID's canonical one records the alias first, so a create that
     reaches Proton is served at the client's name whatever happens next.
  7. One `Sync` call holding every operation, master first; never sent
     again (`api.PutOnce`; a 401 answered before processing is resent once,
     M4-R23), within 60 s (past it the call is ambiguous and read back).
     Rows Proton answered as created are pinned with the packets sent
     (M4-R37); a pin that cannot be stored is logged and the read-back
     still decides (M4-R43).
  8. Read the UID's rows back (`ListByUID`; the pins, the read-back and the
     store bounded by 30 s, even if the client hung up) and store them: the
     cache holds what Proton holds, whatever the answer. Never the row Proton
     echoed (the ETag is the whole listed row).
  9. Decide (M4-R31, R32, R34, R36): every operation shown done by the rows
     read back, by decrypted, verified plaintext per part plus the row fields
     sent → success (201 for a create, else 204, with no `ETag` header),
     whatever Proton answered, an ambiguous failure included. Otherwise:
     after a refusal (an entry's code, or the whole call refused with a 4xx
     that is not session-wide) or after a DELETE, rows changed by someone else
     since the plan → 412 (Thunderbird refetches), else a refusal → 403
     `pcb:proton-refused` (Proton's code only, never its text), also when
     other entries of the same call landed: Proton then holds that part, and
     the client's next read shows it; a row that
     came back under other key packets than sent or pinned → 503; anything
     else (an ambiguous call not shown done, a dropped edit, a read-back that
     failed) → 503 "not confirmed", logged. A write shown done whose rows the
     cache then cannot store is still a success, logged: the next sync pass
     stores them (M4-R43).
  Proton 429, 5xx, a network failure or an ended session before the call
  left: 503 with `Retry-After` (Proton's own on a 429), nothing retried; Proton
  asking for a human verification (422, code 9001) on any of a write's calls:
  503, logged as needing a person, never a 403 (M4-R44)
  [verified: `TestPutCreatesAndReadsBack`, `TestRefreshBeforePrecondition`,
  `TestProton5xxIs503AndNeverRetried`, `TestAmbiguousFailureReadsBack`,
  `TestEntryRefusalIsReadBack`, `TestDroppedEditIsNeverASuccess`,
  `TestWritesSerialiseWithTheSyncer`, `TestDeleteWithARowLeftIs412`,
  `TestOccurrenceDeleteRaceIsTruthful`, `TestSlowPreparationSendsNothing`,
  `TestLandedWriteSurvivesACacheFailure`, `TestPinFailureStillReadsBack`,
  `TestHumanVerificationIsUnavailable`, `TestUIDListingKeepsOnlyThatUID`].
  The lock is held across a GET's backoff (a 429's `Retry-After`, up to 60 s
  and at most twice, plus 5xx retries) so that nothing is stored between a
  write's refresh and its plan; a write waiting behind it gives up at its
  pre-send budget. A write thus runs up to 110 s (20 + 60 + 30), longer than
  the listener's 60 s write timeout: PUT and DELETE extend their response's
  write deadline to 150 s, so a write that landed delivers its answer
  (M4-R42) [verified: `TestSlowWriteStillDeliversItsAnswer`].
- Read-your-writes: the written rows are in the cache before the answer, and a
  later feed read of the same row finds the same ETag and changes nothing
  [verified offline: `TestFeedRereadOfAWrittenRowIsANoop`; live:
  `HUMAN-CHECKS.md` §6 W2, that the row `ListByUID` returns equals the one the
  feed reads].

### 8.2 ETags and conflicts
ETag = `"v1-"` + the first 32 hex characters of the sha256 of the canonical
JSON of the whole raw row (numbers kept as written, keys sorted) (M2-R4): every
field counts, so an edit that does not move `LastEditTime` (notification-only,
colour-only: unknown) is still seen [assumed → E07]; `v1` is the render
version, bumped when our rendering changes so clients re-download.

An object's ETag (the CalDAV resource, one per UID) hashes the render version,
its rows' IDs and row ETags and the calendar settings used (§9). Writes (M4)
evaluate `If-Match` (a list, strong comparison, `*`; §8.1 step 5) and `If-None-Match` on that ETag
**after** refreshing the UID's rows from Proton (§8.1), never on a stale
cache: a mismatch is 412 (Thunderbird then opens its conflict dialog and
refetches, E08 §4.2), never a blind overwrite. A write without a
precondition (Thunderbird's "overwrite" choice) replaces what Proton holds.
A concurrent change Proton shows during a write (another client edited the
same UID between the plan and the call) is 412 too (M4-R34). A PUT answer
carries no `ETag` header: the object served is what Proton stored, which
differs from the body (`spec/mapping.md`), and Thunderbird reads it back
with a multiget anyway (E08 §4.1).

### 8.3 Free-busy
Computed locally from cached own calendars (respecting `TRANSP`) [call];
other users' free-busy is phase 3.

### 8.4 Fidelity rules
`spec/mapping.md` lists every VEVENT and VCALENDAR property a PUT can carry
as routed (to which card part), transformed, stripped (with a debug note), or
refused (403 with a `urn:pcb` precondition and a short reason of the
bridge's own, never Proton's text). The rule [call, research J]: accept and
normalise where nothing the user sees is lost; refuse where writing would
lose or corrupt data or reach the Proton web app in a form it cannot show.
Accepted and normalised: `STATUS:TENTATIVE` dropped (Proton refuses it),
unsupported VALARMs dropped, the client's `COLOR` ignored, `DURATION` made
`DTEND`, Mozilla TZIDs mapped, two RRULE forms rewritten (M4-R5, M4-R12),
missing `DTSTAMP`/`DTEND` defaulted (M4-R8), attendee parameter edits
ignored (M4-R9). Refused (each with its code in `spec/mapping.md`):
- "this and following" splits (`RANGE=THISANDFUTURE`), RDATE, EXRULE;
- an RRULE outside Proton's supported set (`spec/mapping.md` §4; whether
  Proton alters the accepted ones [assumed → E12,
  `spec/experiments/E12-recurrence-forms.md`]);
- floating date-times, TZIDs Go's zone database cannot resolve;
- adding or removing attendees, any attendee on a new event, a new
  occurrence whose attendees differ from its series' (invitations are not
  planned; pcb never notifies attendees);
- bodies that are not one VCALENDAR of VEVENTs of one UID, or over 100
  VEVENTs;
- events pcb cannot rewrite faithfully or safely: placeholders,
  undecryptable or unverified parts, parts of another author, someone else's
  invitation, untrusted session keys, a change needing a key the row lacks
  (`spec/mapping.md` §8);
- writes to read-only calendars (`DAV:need-privileges`);
- edits of a row that holds a personal part (`pcb:personal-part`, until live check L.4 #10; a DELETE is not refused).
Each refusal (every 403, 409 and 412 of a write) is logged at warning level: `refused a CalDAV write`
(§15), the same code and href at most once per 10 minutes (the next line after the window carries `repeated=N`, the refusals swallowed; at most 1 000 distinct refusals are remembered) [verified: `TestEachRefusedWriteLogsOneWarning`, `TestRefusedWritesAreLogged`, `TestRepeatedRefusalsAreLoggedOncePerWindow`, `TestRefusalLogMemoryIsBounded`].
A Thunderbird calendar with offline support keeps a refused change locally
and retries it silently (E08 §3.8, §4.2): refusals are few and logged. One
without offline support shows an error and is disabled until the user
enables it again (E08 §4.2): the README recommends offline support (checked
by default in Thunderbird's New Calendar dialog).

## 9. A1 — CalDAV adapter

As built in M3 and M4 (`internal/caldav`, our own handler on `net/http` and
`encoding/xml`; it imports `service` and the standard library only [verified:
`TestCalDAVImportsOnlyTheServiceAndTheStandardLibrary`]). Reads since M3,
writes (PUT, DELETE) since M4.

- Surface: `/.well-known/caldav` 308 to the principal (behind auth); PROPFIND Depth 0
  and 1 (infinity: 403 `propfind-finite-depth`); REPORT `sync-collection` and
  `calendar-multiget`; GET, HEAD, PUT and DELETE on an object's name (below);
  OPTIONS on any path with `DAV: 1, 3, calendar-access` (never the scheduling
  classes). PUT and DELETE on a collection, PROPPATCH and MKCALENDAR: 405. Not built:
  `calendar-query`, `free-busy-query` (403 `supported-report`), RFC 6638 scheduling
  [verified: the contract tests listed in E08, "What the bridge does (M3)"], nor 507
  paging [source: ruling M3-R23; no test asserts the absence].
- Thunderbird is the reference client: its requests and failure modes were read from
  its source [verified: E08 source]; the MUST/SHOULD/AVOID list of
  `spec/experiments/E08-thunderbird-caldav.md` is the contract and every item maps to
  a test there. No live session yet: it runs with the M4 live session (M3-R1,
  `spec/HUMAN-CHECKS.md` step 6) [claim: Thunderbird behaves as the source says].
- Identifiers (rulings M3-R9, R11, R12, R16): object ETag
  `"v1-" + hex32(sha256(render version, sorted (row ID, row ETag), the calendar settings
  used))`; CTag `<epoch>:<calendar max seq>:<settings hash>`; sync token
  `urn:pcb:sync:1:<epoch>:<seq>:<gen>`, where `gen` hashes the render version, the
  calendar ID, the settings and the key IDs with their lock state. A token of another
  epoch, above the current sequence or below the calendar's floor is refused with 403
  `<D:valid-sync-token/>`; a valid token with another `gen` yields every live UID as
  changed plus the deletions (one full refetch) [verified:
  `TestSyncTokenOfAnotherEpochIsRefused`, `TestMust07InvalidSyncTokenIs403ValidSyncToken`;
  the gen rule: `TestObjectChanges/TokenOfAnotherCalendarListsInFull`, `/KeyRotationChangesETagsAndGeneration`, `/KeyReactivationChangesETagsAndGeneration` in `internal/service/render_test.go`]. Deletions are response-level
  404 per UID, at the UID's served name (M2-R16, M4-R22) [verified:
  `TestSyncCollectionReportsDeletionsPerUID`, `TestAnAliasIsWrittenAndDeletedThere`].
- Resource names (rulings M4-R6, R22, R33, R38, R41; `internal/service/names.go`):
  every UID has exactly one served name and every served name one UID, in every
  read (listings, multiget, sync-collection, deletions) and every write:
  - its **alias**, when a client created it with a PUT at a name of its own
    choice (Thunderbird names an imported or invited UID with `@` by replacing
    each character outside `[A-Za-z0-9_.-]` with `_`, E08 §4.4); the alias is
    recorded before the create is sent and outlives the UID's deletion, so the
    deletion is reported where the client knows it (R22);
  - else its **canonical name**: the UID itself when it is 1 to 200 bytes of
    `[A-Za-z0-9._@-]`, else `~` + its unpadded base64url; then `.ics`;
  - but when another UID's alias already holds that canonical name (an alias
    made before this UID existed), its **alternative name** `~~` + unpadded
    base64url + `.ics` (R38). A PUT may not take another UID's alternative name
    (403 `pcb:bad-resource-name`). When an alias moves away from the canonical
    name of a UID served at its alternative name, that UID keeps its
    alternative name as its own alias, so no href changes (R41).
  The canonical name of an aliased UID serves nothing (404). A PUT whose UID is
  served at another name, or at a name serving another live UID, is 409 with
  `<C:no-uid-conflict><D:href>` naming the object that holds it. A name may be
  up to 255 bytes when it becomes an alias; a served canonical or alternative
  name may be longer. Aliases and served names come from the same cache
  snapshot as the rows (R33) [verified: `TestCreateAtASanitisedNameIsServedThere`,
  `TestUIDConflictNamesTheOtherHref`, `TestALaterUIDMovesAsideForAnAlias`,
  `TestAMovedAliasLeavesTheDisplacedObjectWhereItIs`,
  `TestAMovedAliasLeavesADeletedObjectsHrefToo`, `TestServedNamesAreOneUIDPerName`,
  `TestLongServedNamesWorkOnEveryMethod`].
- Writes (M4; flow in §8.1, mapping in `spec/mapping.md`). A PUT needs
  `Content-Type: text/calendar` (else 415) and at most 8 MiB (else 413). The
  status mapping (`internal/caldav/write.go`):

  | Answer | When |
  |---|---|
  | 201, no body, no `ETag` | a PUT created the event |
  | 204, no body, no `ETag` | a PUT replaced it, or changed nothing; a DELETE removed every row of the UID |
  | 412 | `If-Match` or `If-None-Match` does not hold on the refreshed object (a blank `If-Match` header never holds); a concurrent change Proton showed during the write (M4-R34); an `If-None-Match` naming no ETag |
  | 409 + `<C:no-uid-conflict><D:href>…</D:href>` | the UID lives at another name, or the name holds a genuinely different UID (compared unescaped; the same event at its own name is a 503, never this) |
  | 403 `<D:need-privileges>` | the write gate is closed, decided from good data (write-content on the object for a PUT, unbind on the calendar for a DELETE) |
  | 403 `<pcb:CODE xmlns:pcb="urn:pcb"/><pcb:reason xmlns:pcb="urn:pcb">…</pcb:reason>` | the bridge refuses the change (among others `pcb:personal-part`: the row holds a personal part pcb cannot rewrite yet), or Proton refused it (`pcb:proton-refused`, its code only); the reason is a fixed text of ours, never event content or Proton's text (Thunderbird never shows it, E08 §4.2; it is for curl and logs) |
  | 404 | a DELETE of a name that serves nothing (Thunderbird: success) |
  | 503 + `Retry-After` | anything else: not serving, a pending calendar, an undecided write gate (M4-R39), Proton unavailable or rate limiting (its own `Retry-After` on a 429), a write not confirmed by the read-back, an event whose UID Proton stores in another form than its text, a `clients.json` that cannot be read; never a 4xx for a state that may pass (M3-R22) |

  [verified: `TestMust10PutIs201Or204WithoutAnETag`,
  `TestMust10PreconditionsAre412AndUIDConflictsAre409`,
  `TestMust11DeleteIs204Or404Or412AndHeadAnswers`,
  `TestWriteRefusalsAre403WithTheirPCBPrecondition`,
  `TestWritesAre503WhenTheServiceCannotAnswer`,
  `TestRefusedWritesNeverReachProton`, `TestUnavailableWritesAre503WithRetryAfter`,
  `TestGoWebdavClientWritesAndReadsBack`].
- Privileges (rulings M4-R1, M4-R39, M4-R40, replacing M3-R1): a calendar
  advertises read and write (`write`, `write-content`, `bind`, `unbind`) when
  its write gate is open (§5.3), read alone when the gate is closed. While the
  gate cannot be decided (keys not unlocked, no passphrase signature, no write
  key) it keeps the last decision this process made, and before any decision
  advertises **no** `current-user-privilege-set` (a 404 propstat):
  Thunderbird persists a read-only flag and never clears it (E08 §5), so a
  transient bootstrap failure must not make a subscription read only for
  good; a write meanwhile is 503 [verified: `TestShouldPrivilegesFollowReadOnly`,
  `TestPrivilegesFollowTheWriteGate`, `TestUndecidedWriteGateIs503`,
  `TestUndecidedGateKeepsTheLastDecision`].
- Availability (M3-R22, R25, R26, R30, R35): while the service cannot answer (logged out, no
  cache yet, Proton 5xx), a calendar is not yet synced, or an IP is rate limited, the
  answer is 503 + `Retry-After`, never 4xx: a 4xx on the first PROPFIND, OPTIONS or
  empty-token sync disables the calendar in Thunderbird (E08). A calendar path that
  names no known calendar is 503 too while any calendar is pending (the first pass
  records them one by one), 404 otherwise [verified:
  `TestAvoid4xxWhenTheServiceIsUnavailable`,
  `TestPendingCalendarsAre503AndLeftOutOfTheHomeSet`,
  `TestUnknownCalendarIs503WhileACalendarIsPending`,
  `TestMust01BlockedClientsGet503WithWholeSecondsRetryAfter`].
- Accounts and URLs (M6, decision 0014, rulings R26 to R30; `internal/caldav`, `service.Hub.Route`).
  Each account is served under `/dav/<local-id>/` (`service.AccountPath`): its root answers
  `current-user-principal`, then `/dav/<local-id>/principals/me/` and `/dav/<local-id>/calendars/me/<calendar>/...`,
  the M3/M4 tree under that base; every 401 there carries `WWW-Authenticate: Basic realm="pcb <local-id>"`. The
  password must be one of that account's clients, else 401: account B's password on account A's paths is refused,
  and a request is never served by another account's API. An unknown but well-shaped local ID (8 lowercase hex) is a
  401 with that realm and a limiter charge, as a wrong password is (no probe oracle for which IDs exist). The
  compatibility URLs, M5's, stay: `/`, `/.well-known/caldav`, `/dav/principals/me/`, `/dav/calendars/me/...`,
  realm `pcb`; there the password alone picks the account (each account's client store verifies it: salted SHA-256 over
  192-bit random passwords, a few hashes per request, no ambiguity), so a migrated install's Thunderbird needs no
  change; a first segment under `/dav/` that is not a local ID is a compatibility path. On either kind of root,
  `/` and `/.well-known/caldav` point at the authenticated account's own per-account principal, so a new setup ends on
  per-account URLs. The URL printed by `pcb serve` and `pcb client show` is `<scheme>://<host>:<port>/dav/<local-id>/` and
  the username shown is the account's address (any username is accepted). Responses are in the namespace the request
  came on (hrefs of another namespace in a multiget are 404). Why the prefix and the realm: one Thunderbird profile holding
  two accounts on the same host would otherwise see one origin, one realm and the same paths, and its HTTP auth cache
  could send account A's password on B's URLs [assumed → `HUMAN-CHECKS.md` step 8: that Thunderbird keeps the two
  passwords apart; nothing here depends on it, a wrong password is a 401]. One failure limiter serves every account,
  charged once per request and only for a password no account holds; a client of an account whose CalDAV adapter is off is
  401 with no charge and no activity (every Thunderbird shares 127.0.0.1, a client left configured would otherwise
  lock every account out, R28); while any account's `clients.json` or `state.json` cannot be read, a password no other
  account holds is 503 and not charged, on an account's URLs only that account's store counting (R29, a 192-bit
  password makes guessing moot). A client of an account whose loop is parked or waits for a login is 503 with
  `Retry-After: 30` after authentication; a removal waits for the requests in flight; the adapter's request log
  carries `account=<local-id>` (R30)
  [verified: `TestPrefixedURLServesOnlyThatAccount`, `TestOtherAccountsPasswordIsRefusedOnPrefixedURL`,
  `TestCompatibilityURLRoutesByPassword`, `TestRootPointsToPrefixedPrincipal`, `TestCalDAVOffAccountIs401`,
  `TestBrokenClientsFile`, `TestParkedAccountIs503`, `TestSignedOutAccountIs503`, `TestLimiterIsSharedAcrossAccounts`,
  `TestUnknownAccountIs401`, `TestRouteToAnotherAccountIsRefused`, `TestMultigetHrefsOfAnotherNamespaceAre404`,
  `TestRouterErrorIs503`, `TestWarningsNameTheAccount`, `TestRouteKeepsEachAccountToItsOwnURLs`,
  `TestRouteChargesTheLimiterOncePerRequest`, `TestRouteCalDAVOffClientIsNotCharged`,
  `TestRouteCalDAVOffMatchDoesNotTouch`, `TestRouteUnreadableStateIs503Uncharged`, `TestTwoAccountsSideBySide`].
- Auth (M3-R17 to R21, amended by decision 0013; per account since M6): HTTP Basic, any username; the password is an
  app password (24 random bytes, base64url) from `pcb client add NAME`. It is kept in
  `<state>/accounts/<local-id>/clients.json` (`<state>/clients.json` before M6; 0600, format version 2) twice: as a salted SHA-256, compared in
  constant time, and sealed with the key that seals the session (§7.3), so it can be shown
  again. This amends M3-R17 ("printed once, salted hash only"). Every `pcb login` that
  completes (not an unattended relogin) creates a client named `default` when none exists, so a fresh
  install has credentials (also after all clients were removed: a new random password, decision 0013's rule); `pcb client show [NAME] [--reveal]` prints them (§13); `pcb client rotate NAME`
  replaces one and the old stops working at once; clients made before M5 keep only their hash and
  `show` names `rotate`. The file is reloaded when it changes, so `remove` and `rotate` bite at once.
  A pcb from before M5 refuses a version 2 `clients.json` (no downgrade; M5-R14 (1)). Passwords never
  appear in logs, argv or errors, and `--json` carries one only with `--reveal` [verified:
  `TestRevealReturnsTheAddedPassword`, `TestRotateInvalidatesTheOldPassword`,
  `TestV1ClientsStillCheckAndAreNotRecoverable`, `TestFirstLoginCreatesTheDefaultClient`,
  `TestDefaultIsNotRecreatedOnceAnyClientExists`, `TestShowClientPicksDefaultElseTheOnlyClient`,
  `TestShowClientMadeBeforeSealingIsNotRecoverable`, `TestClientCredentialsRedactThemselves`, `TestNoPasswordInLogs`,
  `TestClientShowJSONHidesThePasswordWithoutReveal`]. 401 with
  `WWW-Authenticate: Basic realm="pcb", charset="UTF-8"` (realm `pcb <local-id>` on an account's URLs, above); 5 failures per minute per IP
  (IPv4 /32, IPv6 /64) then 503. A `clients.json` that cannot be read or parsed is not a wrong password: the
  request answers 503 with `Retry-After: 30`, charges no limiter slot, and `pcb serve` logs the load error at
  error level once per distinct failure (path and cause, no content; a repaired file works at once) [verified:
  `TestCorruptClientsFileAnswersARetryLogsOnceAndChargesNoLimiter`]. OPTIONS is answered without auth (M3-R23)
  [verified: `TestMust01UnauthenticatedRequestsGet401WithTheBasicChallenge`; the client
  store and limiter: `internal/access` tests, e.g. `TestAddedPasswordChecksAndIsNeverStored`, `TestRemovedClientIsRefused`].
- Limits: PUT bodies 8 MiB, PROPFIND and REPORT bodies 1 MiB, a requested property name or namespace 256 bytes, XML names and attributes capped, multiget 1 000 hrefs,
  `calendar-data` in a PROPFIND Depth 1 or a sync-collection rendered for the first
  1 000 objects only (the others keep `getetag` and get `calendar-data` in a 404
  propstat, M3-R35), a name or namespace over 256 bytes refused 400 before any work, no cap on concurrent connections (parked, `OPEN.md`), `ReadHeaderTimeout` 10 s, no error text, Proton text or other
  users' IDs in bodies (a write refusal's `pcb:reason` is a fixed text of ours)
  [verified: `TestPropfindAndReportBodiesOver1MiBAre413`,
  `TestPropertyNamesAndNamespacesOver256BytesAre400`, `TestPropertyListsOver64NamesAre400`, `TestCalendarDataInListingsIsCapped`,
  `TestAvoidErrorTextInBodies`].
- Library (E17, M3-R5): our own server. emersion/go-webdav v0.7.0 has no server-side
  sync-collection, `getctag` or `supported-report-set`, hard-codes privileges and
  re-encodes iCalendar (so it cannot serve it verbatim); it is used as an independent
  **test client only**, never in the binary [verified: `go version -m` of a build lists
  no go-webdav; `TestGoWebdavAndGoICalAreTestOnly`;
  `spec/experiments/E17-go-webdav-audit.md`].
- Listener: `127.0.0.1:1080` by default; an IPv4 literal binds `tcp4` only (`0.0.0.0` does not also listen on `[::]`; `[::]` stays dual stack) [verified: `TestListenNetworkIsTCP4OnlyForIPv4Literals`]; a failed TLS handshake (a port scan) is logged at debug, not warning [verified: `TestServerErrorLogPutsFailedTLSHandshakesAtDebug`]; a stored certificate that is replaced by a new one logs a warning naming the old and the new fingerprint (the first generation and a reuse stay silent) [verified: `TestListenerTLSWarnsWhenAStoredCertificateIsReplaced`]; TLS (self-signed generated or user
  certificate) required for any non-loopback bind, decided on the resolved bind address;
  built in `pcb serve`, `[caldav]` in §14: TLS 1.2 or later; the generated certificate
  is ECDSA P-256, 825 days, in `<state>/tls/` (directory 0700, key 0600, checked at each
  start), SANs = hostnames + `localhost` + bind IP (every interface IP for
  0.0.0.0/[::]), replaced when a hostname or specific bind IP is no longer covered or
  under 30 days remain; its SHA-256 fingerprint is printed by `pcb serve` and
  `pcb doctor` for the exception dialog (M3-R29, R31, R32) [claim: Thunderbird offers
  the exception over the network, E08 §2.3, live check pending; the certificate rules
  are tested in `internal/tlsutil` and `internal/cli` [claim: test names not listed here]].
- Plain HTTP inside a container behind a reverse proxy: not built (`OPEN.md`, later); the image serves the
  self-signed TLS of any non-loopback bind on 1080.

## 10. A2 — `.ics` feeds

Not planned [decided 0007].

## 11. A3 — REST API

Not planned [decided 0007]. The control API (CTL, §4) is internal: JSON
over HTTP on a unix socket (0600, peer UID checked), versioned
(`/v2/…` since M6), used only by `pcb`. It is documented in code, not in
OpenAPI, and may change between versions.

## 12. A4 — MCP adapter

Built in M8 (decision 0016, design `spec/designs/2026-11-13-mcp-design.md`, rulings in §18):
`internal/mcp` (server, grants, tools, audit, remote and relay handlers, stdio),
`internal/mcp/loginpage`, `internal/loginflow` (the login steps `pcb-ui`'s wizard and the page share),
`access.TokenStore`, `service.Agenda` and, in `internal/ical`, recurrence expansion, the event view and
edits as patches. It replaces decision 0007's "MCP not planned" and the old note "agents never on the
control socket": agents reach the control socket only through its `/mcp` route, under the socket's
same-UID trust, never through `/v2` routes. Everything below is tested against the fake Proton and the
SDK's own client; a real agent client (Claude Code) and the Proton web app are `spec/HUMAN-CHECKS.md`
step 10, not run yet.

- **One server, three entries.** One `mcp.Server` per daemon, on `service` only and the official Go SDK
  (`modelcontextprotocol/go-sdk` v1.8.0, E19), serves:
  - **remote**: `/mcp` on the daemon's CalDAV listener, for bearer-token holders, only when `[mcp] remote = true`
    (§14). Off (the default), `/mcp` is just another CalDAV path, never MCP [verified: `TestRemoteMCPOffByDefault`,
    `TestRemoteMCPServesTools`, `TestServeServesRemoteMCP`, `TestBridgeServesRemoteMCP`];
  - **relay**: `pcb mcp` forwards the agent's session to a running daemon (`pcb serve`, `pcb-ui`, or another
    `pcb mcp`) on the control socket's `/mcp` route, every daemon kind serving it; the grant travels in the
    `X-Pcb-Mcp-Grant` header, re-checked by the daemon, and the audit names it `stdio:read-only`, `stdio:full`
    or `stdio:custom` from its capabilities, whatever name was sent. The peer-UID check of §15 runs first
    [verified: `TestMCPStdioRelaysToARunningDaemon`, `TestControlSocketMCPInServe`, `TestRelayAuthNeedsTheGrantHeader`,
    `TestControlSocketServesMCP`, `TestControlSocketMCPRefusesOtherUID`; `make rehearse` step 19];
  - **stdio**: with no daemon, `pcb mcp` becomes the daemon for the session (kind `mcp`, control socket, no network
    listener) and serves the agent in process (§13) [verified: `TestMCPStdioBecomesTheDaemon`,
    `TestRunWithoutListenerOpensNoPort`; `make rehearse` step 22].
- **Tools**, exactly: `status`, `calendars_list`, `free_busy` (capability `freebusy`); `events_list`, `event_get`,
  `events_search` (`read`); `event_create`, `event_update` (`write`); `event_delete` (`delete`); `login` (`login`);
  `logout` (`manage`). `tools/list` holds only what the grant allows, and `login` and `logout` are never built for
  remote [verified: `TestToolsListFollowsTheGrant`, `TestWriteToolsAbsentForReadGrant`, `TestLoginAndLogoutNeverRemote`].
  Every result carries `structuredContent` with an `outputSchema` and a one-line text; annotations: `readOnlyHint`
  on the reads, `destructiveHint` on `event_delete` and `logout`. A tool error is a result with `isError` whose text
  is `{"error": {"class", "message", "hint", "reason"?, "retry_after_s"?}}`, the class one of `must-act`, `retry`,
  `refused`, `conflict`, `not-found`; the message and hint are those of a pcb error, and an error that is not one
  says only `the call failed` (built: a JSON text, not structured content, which would have to match the success
  schema) [verified: `TestErrorClasses`].
- **Grants.** Capabilities `freebusy` < `read` < `write`, plus `delete` (needs `write`: refused otherwise),
  `login` and `manage` (stdio only, refused on tokens); `read` brings `freebusy`, `write` brings `read`
  [verified: `TestParseCapsAppliesImplications`, `TestMCPTokenRefusesLoginCap`, `TestMCPTokenRefusesDeleteWithoutWrite`].
  stdio profiles: `read-only` (`read` + `login`, the default) and `full` (all six); `--allow` replaces the profile
  [verified: `TestProfiles`]. A grant names accounts (address, case-insensitive, or local ID; none: every account,
  later ones included) and, for a token, calendar IDs (none: all). An account outside the grant, pending, with
  MCP off (`pcb account mcp off`, the person's kill switch for every agent) or signed out (`logged-out`) is
  invisible: a call naming it or one of its `event_id`s answers as for something that does not exist (`no account
  X`, `no event X`), and its agenda is never asked [verified: `TestScopedGrantCannotSeeOtherAccounts`,
  `TestMCPOffAccountIsInvisible`, `TestAccountRemovedMidSessionIsNotFound`, `TestEventIDOfAnotherAccountOutsideGrant`,
  `TestEventIDToolsHideAccountsLikeMissing`]. An account that needs a login stays visible, so `status` can name the
  step. Under a calendar grant an account counts only when it holds a granted calendar, and one whose calendars are
  not known yet (before its first sync) counts as holding none (ruling R14: fail closed); named in a call's `accounts`
  or `account`, such an account answers `not-found` exactly as an account pcb does not have, so a calendar-scoped token
  cannot probe which accounts exist [verified: `TestCalendarGrantHidesAccountsWithoutThem`,
  `TestCalendarGrantNamedAccountAnswersAsUnknown`]. Over stdio the grant is a guard rail, not a boundary (§19).
- **Ids, times, paging.** `event_id` is opaque (base64url of the account's local ID, calendar ID, UID and
  RECURRENCE-ID); an occurrence's part keeps the series' own form (a date, a floating wall clock or a zoned
  instant), so an id does not depend on the zone a call reads in (ruling R10) [verified: `TestEventIDRoundTrip`,
  `TestOccurrenceIDsKeepTheSeriesForm`]. Inputs: RFC 3339 with an offset, or a wall clock or date read in `tz` (an
  IANA name; `Local` refused), else `[mcp] timezone`, else the process's zone; a malformed one is `refused`, naming
  the argument [verified: `TestNaiveTimeUsesTzThenZone`, `TestMalformedTimeIsRefused`]. Outputs give each time with
  its TZID and UTC offset, floating times marked `floating`, all-day as dates (end exclusive); `status` gives `now`
  (instant, zone, offset) [verified: `TestStatusGivesNow`, `TestEventsListGivesTimesIdsAndFlags`]. Limits: a range of
  at most 366 days, `limit` 100 by default and 500 at most with `truncated` and `next_cursor`, 1000 occurrences per
  series per call, 2000 objects decrypted per call (a page cut by it is `truncated`), descriptions cut at 4 KiB in
  lists and 64 KiB in `event_get` [verified: `TestOccurrencesRefusesTooLongRange`, `TestOccurrencesPagesWithoutGapsOrRepeats`,
  `TestOccurrencesRenderBudgetPages`, `TestExpandCapsPerSeries`, `TestDescriptionCutAt4KiBInLists`]. Read tools merge
  every visible account in start order; an account that fails while others answer is listed under `unavailable`
  [verified: `TestEventsListMergesAccountsWithoutGaps`, `TestOneAccountFailingDoesNotHideTheOthers`]. An event pcb
  cannot read is skipped and counted (`skipped`; logged with its calendar and UID only), never failing the page, and
  `free_busy` says those events' times are unknown (rulings R12, R13); transparent and cancelled events are free
  [verified: `TestOccurrencesSkipsUnreadableEvents`, `TestFreeBusyMergesAndReportsSkipped`, `TestFreeBusySkipsTransparentAndCancelled`].
- **Recurrence** is pcb's own expander over Proton's recurrence set (E16: rrule-go not adopted): RRULE, EXDATE,
  overrides by RECURRENCE-ID, all-day, TZID with DST gaps and overlaps, floating times in the call's zone. RDATE is
  never expanded, since Proton refuses to store it (the design listed it); a shape outside Proton's set is refused,
  and the event skipped as above [verified: `TestExpandRFC5545Examples`, `TestExpandDSTTransitions`, `TestExpandProtonForms`,
  `TestExpandFloatingUsesLoc`, `TestExpandAllDayRecurring`, `TestExpandRefusesShapesOutsideProtonsSet`, `TestOccurrencesExpandsAndMerges`].
  A TZID wall clock in a DST overlap or gap means one instant everywhere, by RFC 5545 §3.3.5 (the first of two, the
  offset before a gap): the expander, the event view, the agent's edits and deletes and the CalDAV write path read it
  alike, so an override at an ambiguous time is found by the list, an update and a delete [verified:
  `TestWallClocksResolveAsExpandDoes`, `TestDSTOverlapOverrideIsTheOccurrence`].
- **Untrusted content.** Every calendar-derived free text (summary, description, location, URL, organizer and
  attendee names, calendar names and descriptions) sits under an `untrusted` object, and every tool description says
  it is data written by third parties, never instructions. `external_organizer` marks an event organized by an
  address that is none of the account's; `third_party` also marks subscribed and holidays calendars [verified:
  `TestUntrustedFieldsAreGrouped`, `TestEventGetAgreesWithTheListOnTheOrganizer`, `TestOccurrencesFlagExternalOrganizer`].
  A mitigation, not a boundary: the controls are the grants, the ETags and the confirmations.
- **Writes** run only while the process serves (M4: Serve's writer handle) [verified: `TestAgendaWritesNeedServe`].
  `event_create` (`account`, `calendar`, `summary`, `start`/`end` or all-day `start_date`/`end_date`, `tz`, `location`,
  `description`, `rrule`, `alarms` in minutes, `busy`) returns `event_id` and `etag` [verified: `TestCreateReturnsIDAndETag`,
  `TestCreateEventLandsInProton`]; when the ETag cannot be read back after a write that landed, the result succeeds
  without it and says to call `event_get` and not to write again. `event_update` takes `etag`, `scope` (`series` or
  `occurrence`), `changes` (the create fields; `null` clears summary, description, location, rrule or alarms) and
  `dry_run` (the changed fields before and after, nothing written); new times keep the event's own zone unless `tz` is
  given [verified: `TestUpdateChangesNullClears`, `TestUpdateDryRunShowsBeforeAfter`, `TestUpdateEventDryRunWritesNothing`,
  `TestUpdateTimesKeepTheEventZone`]. Edits patch the stored iCalendar, so what the call did not touch (alarms,
  exceptions, unknown properties) stays [verified: `TestApplyEditKeepsUntouchedLines`]; `alarms` given keeps an existing
  alarm at the same minutes as it is (an e-mail notification stays an e-mail), and only a new minute value becomes a
  device (DISPLAY) alarm [verified: `TestReplaceAlarmsKeepsTheActionOfAnAlarmSentAgain`]; one occurrence becomes an
  override, deleting one adds an EXDATE [verified: `TestApplyEditOccurrenceCreatesOverride`, `TestDeleteOccurrenceAddsExdate`,
  `TestDeleteOneOccurrence`]. A series that only moves in time takes its EXDATEs, its overrides' RECURRENCE-IDs and its
  UNTIL along; a change that would leave an exception, or the start, off the rule kept is refused (`refused`,
  reason `pcb:series-has-exceptions`, or `pcb:bad-data` for a start off its own BYDAY or BYMONTHDAY; ruling R11), never
  dropped [verified: `TestApplyEditSeriesShiftMovesExceptions`, `TestApplyEditShiftMovesUntil`, `TestApplyEditShiftOffTheRuleIsRefused`,
  `TestApplyEditRRuleChangeWithExceptionsIsRefused`, `TestUpdateSeriesWithExceptionsRefused`]. A stale ETag is `conflict`
  and writes nothing; a calendar the write gate keeps read-only is `refused` before any write; every M4 refusal is
  `refused` with its `pcb:` reason [verified: `TestUpdateStaleETagIsConflict`, `TestUpdateEventStaleETagIsConflict`,
  `TestWriteToReadOnlyCalendarRefused`, `TestErrorClasses`].
- **Delete confirmation.** When the client declares form elicitation (or elicitation with no mode), `event_delete`
  checks everything first (a stale ETag is `conflict`, asked nothing), then asks `Delete the event on <date>: «<summary>»?`
  (one occurrence: `Delete one occurrence, on <date>, of the series «<summary>»?`; the whole series of a recurring
  event: `Delete the whole series (every occurrence, first on <date>): «<summary>»?`, ruling R16). What goes comes
  first; the summary, a third party's text, comes last without control or format characters (bidirectional,
  zero-width) and with `«»` replaced by `‹›`, so it cannot rewrite the question [verified:
  `TestDeletePromptResistsSpoofing`]. Only `accept` deletes,
  `decline` or `cancel` is `refused` and nothing is deleted. A client that declares no form elicitation (none, or URL
  only) deletes with the `delete` grant alone [verified: `TestDeleteAsksWhenElicitationDeclared`,
  `TestDeleteWithoutElicitationNeedsDeleteCap`, `TestRelayCarriesAConfirmation`]. What real clients declare is
  `[assumed → step 10]`.
- **Login and logout** (stdio and relay only). `login(username, unattended_relogin?)` takes no secret (the schema refuses
  any other argument; the username must look like an address, so no shell, page or log can misread it). The
  process that owns the accounts acts:
  - `pcb-ui` raises its add-account wizard with the username filled in (a login the person is already in keeps its
    step) and answers `where: pcb-ui`, no URL [verified: `TestLoginToolRaisesTheWizardInPcbUI`,
    `TestBridgeRaisesTheWizardForAnAgentLogin`, `TestShowLoginOpensTheWizardPrefilled`, `TestShowLoginKeepsALoginInProgress`];
  - any other (a session daemon, `pcb serve` reached by relay) opens a page for this one login at
    `http://127.0.0.1:<ephemeral port>/login/<128-bit nonce>` (never the `[caldav]` address): Host and Origin
    checked (403), `Cache-Control: no-store`, the username shown read-only, then the password, the 2FA code or the
    CAPTCHA link; the first form post claims the page with a cookie and any other browser gets 410; it closes when the
    login ends, on Cancel, after 5 minutes or when replaced, removing a half-way add [verified: `TestLoginPageBindsLoopbackWithANonce`,
    `TestLoginPageSingleUse`, `TestLoginPageExpires`, `TestLoginPageRefusesForeignHostAndOrigin`, `TestLoginPageNoStore`,
    `TestLoginPageNeverEchoesThePassword`, `TestLoginPageRunsTOTPAndCaptchaSteps`, `TestLoginPageStopCancels`]. A client that
    declares URL elicitation is handed the link that way (declined or cancelled: the page closes, `refused`); otherwise
    the URL is in the result. Every result carries the CLI fallback `pcb login --username U` (the `cli_fallback` field, and
    the hint of a refusal), for a `pcb mcp` in a container whose loopback the browser cannot reach [verified:
    `TestLoginToolResultHasNoSecret`, `TestLoginToolRaisesTheWizardInPcbUI`].
  - One `login` per 60 s per MCP session (`retry`, with `retry_after_s`), on top of the Hub's login guard and its one
    login at a time; a later login of the same session replaces its page [verified: `TestLoginToolCooldown`,
    `TestLoginToolReplacesTheSessionsPage`]. A session daemon asks its sealer guard (§13) first [verified: `TestLoginToolAsksTheGuard`].
  - `logout(account)` signs one account out as `pcb logout` does, confirmed by form elicitation when the client
    declares it (decline: nothing changes), else on the `manage` grant alone [verified: `TestLogoutConfirms`].
  - Over remote there is no login: `status` shows `needs-login` with the step "run `pcb login` on the server, or open
    pcb-ui" [verified: `TestStatusGivesNow`].
- **Remote access.** `Authorization: Bearer pcbm_<43 base64url characters>` (256 bits), made by `pcb mcp token add`
  (§13), kept in `<state>/mcp-tokens.json` (0600, version 1, strict decoding) as a salted SHA-256 only, never sealed or
  shown again; the prefix keeps a CalDAV password from passing as a token [verified: `TestTokenAddStoresOnlyAHash`,
  `TestTokenFileStrictDecoding`, `TestTokenFileMode0600`, `TestTokenWrongPrefixRefused`]. No token, or a bad, unknown or
  expired one: 401 with `WWW-Authenticate: Bearer`, counted by the remote's own failure limiter (`access.Limiter`: after 5
  failures in a minute from one IP, IPv6 keyed by its /64, further requests get 503 with `Retry-After`); a token whose stored
  grant is refused: 403 [verified: `TestRemoteMCPNeedsAToken`, `TestRemoteMCPLimiter503AfterFiveFailures`,
  `TestLimiterKeysIPv6ByPrefix64`, `TestTokenExpired`]. `Host` must be a loopback name, a `[caldav] hostnames`
  entry or the IP the request arrived on, and a present `Origin` must equal it (`null` refused): else 403, a
  DNS-rebinding guard of pcb's own (the SDK checks no Origin, E19) [verified: `TestRemoteMCPRefusesForeignHost`,
  `TestRemoteMCPRefusesForeignOrigin`, `TestE19HostCheckedOriginNot`]. Sessions are stateful, bound to the token that opened
  them (another token: 403), forgotten after 1 h idle (their ID then answers 404, and the client initialises again), and their
  streams outlive the listener's write timeout [verified: `TestRemoteMCPSessionBoundToItsToken`,
  `TestRemoteMCPSessionEndsWhenIdle`, `TestRemoteMCPStreamOutlivesWriteTimeout`]. The token never reaches a tool [verified: `TestRemoteMCPKeepsTheTokenFromTools`].
  TLS follows the listener's rule (§9: TLS on any non-loopback bind).
- **Audit.** One `info` line `mcp call` per tool call in the daemon's log (stderr of `pcb serve` and `pcb mcp`, `pcb-ui.log`):
  `tool`, `entry` (`remote`, `relay`, `stdio`), `grant` (token name or stdio profile), `account` (local IDs), `calendar`,
  `uid`, `outcome` (`ok` or the error class), `ms`; never an argument's text, content, a page URL or a secret; a
  tool name pcb does not serve is `(unknown)`. A run that only asks the person writes none; the call's completion writes
  its one line [verified: `TestAuditLineHasNoContent`, `TestDeleteAsksWhenElicitationDeclared`; `make rehearse` step 19].
- **No secret in any output.** An end-to-end scan (logins with 2FA and CAPTCHA through the page, every tool, remote with a
  token, a relayed logout) finds no password, otpauth secret, 2FA code, CAPTCHA token, MCP token or page nonce in any tool
  result, MCP log message, audit line, stderr or file under the state and config directories, the nonce allowed only in the
  URL elicitation itself [verified: `TestNoSecretInAnyMCPOutput`].

## 13. A5 — CLI and ops

Conventions (as backsight) [call]: every command has `--json` except `serve`, `service logs` and `mcp` (M8: stdout carries the MCP session), which refuse it (exit 2) (one
envelope `{schema_version, command, ok, data | error}`; the four keys are always present, and `data` and `error` may both be set,
see "Initial-version audit behaviour" below); state-changing
setup commands have `--plan`; `--non-interactive` never prompts and exits 10
naming what is missing; exit codes `0` done, `2` bad invocation, `10` you
must act, `20` blocked by the machine, `30` failed and rolled back, `40`
partly done and resumable. Credential and verification failures (wrong password, 2FA code rejected, CAPTCHA not accepted) exit 10, not 20: the user must act. A user cancel (Ctrl-C at a prompt) exits 20 with the hint "run the command again".

Command tree:
```
pcb install [--system|--user] [--plan]     # M5: unit file, dirs, default config; --user (default) needs no root
pcb uninstall [--system|--user] [--delete-data [--yes]] [--plan]   # M5
pcb doctor                                  # every check, with the fix to apply (M3: listener, TLS certificate expiry and fingerprint, CalDAV clients; M5: service, daemon, alerts)
pcb serve [--listen HOST:PORT] [--wait-for-login] [--log-level L]   # sync loop (30 s, 5 min idle) + CalDAV listener + control socket in one process (M3 reads, M4 writes, M5 daemon; foreground; the unit and the container run it); not with --json. CalDAV writes are accepted only while it runs. Without --wait-for-login: refuses to start logged out (exit 10) or while `pcb sync` runs (exit 20), stops with Proton's error code when the session ends (M3-R28); with it: waits for a login (§8.1). Prints `Thunderbird URL: ...` and, over TLS, `TLS certificate SHA-256 fingerprint: ...`; SIGINT/SIGTERM drain in-flight requests for 5 s, exit 0, a second one exits 20
pcb service start|stop|restart|status|logs [-f] [--system|--user]   # M5: systemctl and journalctl, scope from what is installed
pcb login [--username U] [--password-fd N] [--unattended-relogin]   # M5: through the running daemon when there is one; M6: no --account, the username decides: a known address is relogged, a new one adds an account (§4.1)
pcb login resume (--captcha-token T | --totp)   # --totp takes no value: the 2FA code of the login the daemon holds comes from PCB_TOTP or a prompt, never argv (M5-P11)
pcb logout [--yes]                          # M6: for one account (--account); revokes the session; --yes discards a store pcb cannot read, without the confirmation (§7.3)
pcb status [--daemon]                       # login state; with a daemon: its live state, listener, per-calendar sync and write gate, render counters, alerts (§15); --daemon exits 20 when none answers
pcb calendar list                           # what Thunderbird will see; read_only is the write gate (M4-R1: false only for a normal calendar with the Write bit and a verified passphrase signature)
pcb sync [--full] [--watch [--interval 30s]] # M2: fill/refresh the event cache (--watch not with --json)
pcb event list --calendar ID|NAME [--from D] [--to D]   # M2: cached events, no network call
pcb client add NAME|list|remove NAME|ID      # CalDAV credentials (M3: app passwords; add works logged out; `clients.json` survives `pcb logout`, M3-R17)
pcb client show [NAME|ID] [--reveal]         # M5, decision 0013: URL (M6: the account's /dav/<local-id>/), username (M6: the account's address; any username is accepted), password and, over TLS, the certificate fingerprint; without NAME: `default`, else the only client
pcb client rotate NAME|ID                    # M5, decision 0013: a new password, the old one stops working at once
pcb account list                            # M6: local ID, address, phase, CalDAV on/off (M8: and MCP on/off), last sync; pending adds included; the daemon's view when one answers
pcb account caldav on|off                   # M6: the account's CalDAV switch (§4.1)
pcb account mcp on|off                      # M8: the account's MCP switch (§4.1)
pcb mcp [--profile read-only|full] [--allow CAPS] [--account A]...   # M8: MCP on stdin/stdout for the agent that runs it (§12): relayed to a running daemon, else serving the session itself (kind mcp); not with --json
pcb mcp token add NAME --allow CAPS [--account A]... [--calendar ID]... [--expires D] [--plan]   # M8: a remote agent's bearer token, printed once (§12)
pcb mcp token list                          # M8: name, grants, created, last used, expiry (expired ones marked)
pcb mcp token remove NAME [--yes]           # M8: the token stops working at once
pcb account restart                         # M6: serve the account again after its loop stopped on an error (§4.1, "Parked accounts")
pcb account remove [--yes]                  # M6: sign out, then delete the account's directory (cache and CalDAV passwords included)
pcb config get|set|validate                  # not built (M5 out of scope: edit config.toml, run pcb doctor)
pcb version
```
Several accounts per process since M6 (decision 0014, §4.1); the "one account per daemon in v1" rule is gone.
The account selector (`--account A`, else `PCB_ACCOUNT`) is taken by `logout`, `login resume`, `status`, `calendar list`,
`event list`, `sync`, `client add|list|show|rotate|remove` and `account caldav|mcp|restart|remove`; every other command
refuses `--account` (exit 2, `pcb login takes no --account: the username decides ...`).
Secrets are entered only at a terminal, a file descriptor or the
environment, never argv; tokens are printed once.

Initial-version audit behaviour (`spec/audits/2026-11-12-initial-version.md`; `internal/cli`, `internal/service`, `internal/doctor`):
- Exit codes, one per failure in every command (`2` usage, `10` the user must act, `20` transient or no daemon): a key file that is
  missing, of the wrong size, refused for its mode or unreadable by this user, a `secrets.sealed` that does not open or that a newer pcb wrote
  (§7.3), an unreadable (`the login state file <path> cannot be read by this user`, naming owner and mode, hint `make it readable by <user> (or run pcb as its owner)`; only malformed JSON hints `pcb logout`), malformed or newer `state.json`, the root guard (§7.1) and a unit-unsafe path under `pcb install` (also under `--plan`) are all `10`;
  another cause of a store failure (a full disk) stays `20`. `pcb status` is the exception: it reports a key fault (`secrets: unreadable`) and exits 0,
  since it is the diagnosis command (an unreadable `state.json` still fails it, 10); `pcb logout` of a store it cannot read is the way out
  [verified: `TestKeyAndStateFaultsAreExit10InEveryCommand`, `TestInstallPlanRefusesAPathAUnitCannotHoldWithExit10`].
- `pcb sync` (and `--watch`) checks the login before it takes `sync.lock`: logged out it is exit 10 `not logged in to Proton` /
  `run pcb login`. While `pcb serve` holds the lock and answers on its control socket, `pcb sync` is not needed and exits **0** with
  `pcb serve is running and already syncs: see pcb status`; `--json` data is `{"calendars":[],"daemon_syncs":true}`. A holder that is not a
  daemon (`pcb sync --watch`) keeps `a sync is already running` / `wait for it or stop pcb sync --watch` (exit 20). A second `pcb serve` is exit 20 `pcb serve is already running` / `run pcb status to see it` [verified:
  `TestSyncAndASecondServeNameTheRunningDaemon`, `TestSyncChecksTheLoginBeforeTheRunningDaemon`, `TestServeRefusesWhileSyncRuns`].
- Messages: one sentence for the cause, then `Hint:` with one runnable step; no Go error chain (a flag value that does not parse says
  `invalid value "x" for --password-fd: it takes a whole number`, hint `run pcb login --help`; a port in use says `cannot listen on A: address already in use`);
  an empty `pcb client list`, `calendar list` or `event list` says so and names the next step (`No CalDAV client yet.`, `No calendar yet.`, `No event cached for this calendar.` /
  `No cached event in that range.`; the JSON is unchanged); Ctrl-D at a prompt ends the prompt line before the error; `login --help` shows
  `--password-fd N` and `--username NAME` without a sentinel default [verified: `TestBadFlagValueSaysWhatTheFlagTakes`, `TestServePortInUseSaysWhyOnce`,
  `TestEmptyListsSaySo`, `TestOSTerminalEndOfInputEndsThePromptLine`, `TestHelpOffersOnlyWhatTheCommandTakes`].
- Commands that open the service print its warnings on **stderr** as `Warning: MESSAGE (key: value, …)` (and `Hint: …` when the record carries one;
  `Error:` for an error record; nothing below warning), in text and in `--json` mode alike, so stdout stays one envelope. An `error` attribute prints
  the message of a pcb error, else only the innermost cause, never the Go chain [verified: `TestCommandsPrintTheServiceWarningsOnStderr`,
  `TestNoteLoggerPrintsWarningsInPcbsStyle`, `TestNoteLoggerPrintsNoGoChain`].
- The JSON envelope: `data` carries what happened and `error` what stopped it, so both may be present: a CAPTCHA puts `captcha_token` and `captcha_url` in
  `data` and the CAPTCHA error in `error` (`HUMAN-CHECKS.md` M1.3 reads `.data.captcha_token`), and a partial `pcb sync` (exit 40) carries its counts beside its error.
  A usage failure before a command is resolved reports `"command": "pcb"`. A consumer reads `ok` first, then `error` when present, and uses `data` when present.
- `pcb doctor` exit code: 0 when no result is an error (warnings never change it); otherwise 20 only when every error is transient (today only the `daemon` result when the control
  socket cannot be dialled or answers badly), else 10. A control socket pcb refuses (a file that is not a socket, a symlink, another user's socket) is 10. The summary is `pcb doctor found N error(s)` with
  hint `fix what the failed checks name (marked ✗), then run pcb doctor again`. The `needs-login` alert is a warning (`!`) while the daemon waits for a login, an
  error otherwise; a bad `config.toml` is 10; per CalDAV client whose sealed password the key no longer opens, a warning `caldav client NAME: password cannot be shown`, fix
  `pcb client rotate NAME` (not shown when the key file cannot be read or is missing beside `secrets.sealed`: the key file and secrets checks report that)
  [verified: `TestDoctorExitCode`, `TestDoctorNeedsLoginWhileTheDaemonWaitsIsAWarning`, `TestDoctorTransientDaemonErrorIsExit20`, `TestDoctorRefusedControlSocketIsExit10`,
  `TestSealedClientsCheckWarnsPerClientTheKeyNoLongerOpens`, `TestDoctorWarnsAboutPasswordsTheKeyNoLongerOpens`].
- Platforms: **Linux amd64 and arm64 are supported** (§3). macOS (amd64, arm64) and Windows (amd64) build [verified: `GOOS=darwin go build ./cmd/pcb`, `GOOS=windows go build ./cmd/pcb`; CI vets
  Windows] but are untested and unsupported (`pcb doctor` says `platform: GOOS/GOARCH: runs, unsupported`). Off Unix there is no file lock and no control socket, so the CLI runs
  offline there: `version`, `status`, `doctor` and `client add`/`list` ran under wine in the audit, while `login`, `sync` and `serve` exit 20 (`login locking needs a Unix system`, `sync locking needs a Unix
  system`, hint `run pcb on Linux or macOS`) [source: `filelock_other.go`, `listen_other.go`, the ux audit's wine run; no test runs off Linux]. macOS has the Unix lock and socket code but no service management:
  `install` and `service` need systemd (exit 20, `systemd is not running here`), and the control socket path limit is 103 bytes there [source: §7.1; not run on macOS].

M5 behaviour (`internal/install`, `internal/cli`; rulings M5-Rpre5 to Rpre7, R4 to R7, P16 to P19, P23):
- `make install` puts `bin/pcb` at `$(PREFIX)/bin/pcb` (`PREFIX ?= $(HOME)/.local`).
- `pcb install` is `--user` by default (the caller's systemd user manager, no root; refused as root with the hint
  `use --system`) and idempotent: a second run keeps every file. It creates the config and state directories (0700),
  a default `config.toml` (never replaced once it exists), the unit (`ExecStart=<this binary> serve --wait-for-login`),
  then `daemon-reload` when the unit changed, `enable --now`, and a restart when the unit or the system binary changed.
  It prints the steps and what to do next: `loginctl enable-linger <user>` unless lingering is already on, `pcb login`,
  `pcb client show`. `--plan` changes nothing and needs no root. No systemd: `--plan` says so, applying exits 20.
- `--system` needs root: it creates the `pcb` user and group (`useradd`), uses `/etc/pcb` and `/var/lib/pcb`, copies
  the running binary to `/usr/local/bin/pcb` (root, 0755) instead of running a user-writable one as `pcb`, and prints the
  login and client show lines to run as that user: `sudo -u pcb env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb /usr/local/bin/pcb login`,
  then the same with `client show` (the control socket accepts only the daemon's own UID, §15). `--system --plan` as non-root prints the plan and
  a note that applying needs root, exit 0 (M5-R4).
- The user unit carries only hardening that needs no user namespace, so it starts under any user manager (M5-P17);
  the system unit carries the full set (`ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, an empty capability set
  and more). Both set `PCB_CONFIG_DIR`/`PCB_STATE_DIR`, `Restart=on-failure`, `RestartPreventExitStatus=10`,
  `TimeoutStopSec=30s`, `UMask=0077`, and `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`: Go lists the
  network interfaces over netlink, which the self-signed certificate of an unspecified listen address (`0.0.0.0`)
  needs (final review I1; `make systemd-smoke` serves on `0.0.0.0` under the user unit). Only the system unit orders
  itself after `network-online.target`: a user manager has none.
- `pcb uninstall` stops and disables the unit and removes it (data kept). `--delete-data` needs a confirmation or
  `--yes`, then logs out (revoking the Proton session, best effort) and deletes the config and state directories, and
  with `--system` the `pcb` user; it reads the installed unit's own `PCB_*_DIR` (`systemctl show`, never the
  caller's environment; unreadable: exit 10 naming the directories to pass) and removes a directory only when its
  base name is exactly `pcb` (otherwise it is kept and the `rm` command printed, M5-R5). A key file outside the
  config directory is kept and named; `/usr/local/bin/pcb` is left, with the `rm` line printed (M5-P16, P19).
  With no unit installed and no `--user`/`--system`, applying exits 10 ("nothing to uninstall"), while `--plan`
  exits 0 with "Nothing to remove" and no step (M5-R16) [verified: `TestUninstallPlanWithNothingInstalled`].
- `pcb service start|stop|restart` on the system scope needs root (exit 10, `sudo pcb service <verb>`); `status`
  and `logs` do not (M5-R7).
- First login: a `pcb login` that completes while no client exists creates the `default` CalDAV client; `pcb client show` prints the URL,
  the username (the client's name), the password and, over TLS, the fingerprint. With a daemon, the URL and fingerprint
  are its listener's; offline, `show` builds the URL from `config.toml`, so it ignores a `--listen` override and a
  port 0 (M5-R14 (4)), and the fingerprint reads "not known yet" until `pcb serve` created the certificate. No CTL
  endpoint serves `show` or `rotate` in M5: they work on `clients.json`, which the daemon reloads (M5-R13); a CLI under
  another UID than the daemon needs `sudo -u`.
- No client yet: `pcb serve`'s intro and `pcb doctor`'s `caldav clients` fix say `pcb login` (it creates the client
  default), then `pcb client show`, while not logged in; once logged in with no client (all removed), `pcb client add NAME`
  (final review I2) [verified: `TestClientsCheck`, `TestDoctorTextOK`, `TestServeIntroHintsClientAddWhenLoggedIn`, `TestServeStopsOnSignal`, `TestInstallUserCreatesThenKeeps`, `TestInstallSystemCreatesTheUserOnce`].
[verified: `TestUserUnitGolden`, `TestSystemUnitGolden`, `TestDefaultConfigGolden`, `TestInstallUserPlanChangesNothing`,
`TestInstallUserCreatesThenKeeps`, `TestInstallKeepsAnEditedConfig`, `TestInstallUpdatesAChangedUnitAndRestarts`,
`TestInstallSystemNeedsRoot`, `TestInstallSystemCreatesTheUserOnce`, `TestInstallSystemCopiesTheBinary`,
`TestInstallUserAsRootIsRefused`, `TestInstallWithoutSystemdIsBlocked`, `TestInstallFailureMidwayIsPartial`,
`TestUninstallKeepsDataByDefault`, `TestUninstallDeleteDataNeedsConfirmation`,
`TestUninstallDeleteDataStopsThenLogsOutThenDeletes`, `TestUninstallKeepsAKeyFileOutsideTheConfigDir`,
`TestUninstallSystemDeleteData`, `TestDeleteDataRemovesOnlyDirectoriesNamedPcb`,
`TestUninstallDeleteDataNeedsTheUnitsDirectories`, `TestServiceSystemNeedsRoot`, `TestServiceSystemLogsWithoutRoot`,
`TestServiceDetectsTheScope`, `TestServiceStatusParsesShow`, `TestServiceLogsFollow`,
`TestClientShowPrintsWhatThunderbirdNeeds`, `TestClientRotatePrintsTheNewPasswordAndTheHint`,
`TestClientShowNamesRotateForAnUnrecoverablePassword`; `make systemd-smoke` (a Debian container with systemd as
PID 1, both scopes, install, active, 503 with a client, uninstall) and `make container-smoke` passed locally on
commit e5faeb8; the CI jobs have not run until the branch is pushed. Real systemd on a real host, linger across a
reboot and a real login are [assumed → step 7].]

M6 behaviour (decision 0014; `internal/cli`, `internal/service`, `internal/ctl`, `internal/doctor`; rulings in §18):
- Selector: `A` is the account's address (case-insensitive) or its local ID (`pcb account list`), pending adds included; empty, it is the only account
  that is not pending. With several and none given: exit 2, `several accounts: name one with --account`, hint listing them, and `--json` `data.accounts` carries the
  list (also over the control socket, R31); a selector that names none: exit 2, `no account X`, hint `see pcb account list`; none given and no account
  to choose: exit 10 (a person must add one, R38), `no account`, hint `add one with pcb login` (`finish the pending login (pcb login resume), or add an
  account with pcb login` while only pending adds exist). `PCB_ACCOUNT` is read when the flag is not
  given [verified: `TestSelectAccount`, `TestSelectorRules`, `TestSelectorRequiredWithTwoAccounts`, `TestPCBAccountEnv`,
  `TestOnlySelectingCommandsTakeAccount`, `TestLoginRefusesAnAccountSelector`, `TestSeveralAccountsCrossTheSocket`, `TestNoAccountIsExit10`].
- `pcb login` takes the username first and decides (§4.1); the result names its account (`data.account`, the local ID); `pcb login resume` takes the selector
  (a lone pending add is found without one; with several waiting and none named it lists them, exit 2); `pcb logout` of a state whose only account is a pending add acts on it
  (R34) [verified: `TestLoginAddsAccountByUsername`, `TestLoginResumeSelectsThePendingLogin`, `TestLoginResumeNamesTheWaitingLogins`,
  `TestLogoutOfTheOnlyPendingAccount`].
- With a daemon, every account-changing command (login, 2FA code, logout, `account remove`, `caldav`, `mcp`, `restart`) goes through the control socket, which runs the
  Hub's own rules: a running Hub does not rescan `accounts/`, so an offline add would stay unseen until the next start (R15). Offline, `account restart` only
  says the account is served again at the next `pcb serve` [verified: `TestAccountChangesGoThroughTheDaemon`, `TestAccountMCPThroughTheDaemon`, `TestAccountRestartOffline`].
- `pcb account remove` asks you to type `remove` (or `--yes`; without a terminal or with `--non-interactive` and no `--yes`: exit 10) [verified: `TestAccountRemoveNeedsYes`];
  `pcb account caldav on|off` and `pcb account mcp on|off`; `account list` and `status` show both switches [verified: `TestAccountCalDAVOff`,
  `TestAccountListShowsBothSwitches`].
- `pcb status`: the process (daemon, listener, process alerts: `needs-login` with no account at all (ruling R44), the TLS certificate's) once, then a block per account (name, URL, login lines, serve state, calendars,
  render counters, its own alerts); `--account` narrows it to one. Offline, an account that cannot be read keeps its entry with an `account-unreadable` alert and its error
  is the command's (the `pcb status` key-fault exception of §13 stands). `pcb status --daemon --json` is still the container's health check [verified:
  `TestStatusShowsEveryAccount`, `TestOfflineStatusDropsOnlyAccountsTheHubSaysAreGone`, `TestStatusShowsTheDaemon`].
- `pcb doctor`: process-wide checks once; per account, results named `<address>: <check>` (the local ID when the account has no address) (`sealed secrets`, `secrets`, `cache`, `caldav clients`, the sealed-client
  warnings); an `accounts` result for the layout (no account: a warning; a move to layout v2 stopped midway, or `.migrating`: a warning naming it; files of an older
  pcb beside the layout, a newer or an unreadable layout: a failed check with the fix, exit 10 as every failed file check, R36) [verified: `TestDoctorNamesTheAccount`, `TestAccountsOfTheV2Layout`, `TestAccountsOfAStateWithNothing`,
  `TestAccountsOfAnUnreadableLayout`].
- `pcb serve`'s intro prints one `Thunderbird URL (<address>): <base>/dav/<local-id>/` line per account CalDAV serves (a pending add and an account with CalDAV off are left
  out), the TLS fingerprint once, and, per served account with no client, the `client add`/`login` hint; with `--wait-for-login` and no account, `Waiting for a login: run pcb login`
  [verified: `TestServePrintsEveryAccountURL`, `TestServeIntroSkipsPendingAndCalDAVOffAccounts`, `TestServeWaitsForALoginWithNoAccount`]. `pcb client show` prints the account's own
  URL and its address as the username, with a daemon's listener or the configuration's [verified: `TestClientShowPerAccountURL`].
- `pcb sync` takes one account's own `sync.lock` (the daemon holds it while it serves the account); refused while `pcb serve` answers, it exits 0 `pcb serve is running and already syncs` as in M5.
- `pcb uninstall --delete-data` signs out every account (revoking each session; a session that cannot be revoked is reported) before deleting the directories [verified: `TestUninstallSignsOutEveryAccount`].
- `output.SchemaVersion` is 2 since M6 (the shapes of `status`, `doctor` and `client show` changed: `status` data is `{daemon, listener, accounts[], alerts}`, each account its
  `AccountInfo` fields flat plus `login` (the former flat fields), `health`, `url`, `alerts`; the former top-level fields moved into `accounts[].login`); a consumer of version 1
  reads `schema_version` first, and there is no compatibility shim (private, pre-release) [verified: `TestSchemaVersionIs2`].

M7 behaviour (decision 0015; `cmd/pcb-ui`, `internal/ui`, `internal/ui/desktop`, `internal/logfile`, `internal/doctor`; rulings in §18):
- `pcb-ui [--background] [--version]`: the desktop app, Linux only (other systems get a stub that says so). It needs a graphical session: with neither `DISPLAY` nor `WAYLAND_DISPLAY` it prints
  `pcb-ui needs a graphical session (no DISPLAY or WAYLAND_DISPLAY)`, exit 2, and touches no state; `--version` and `--help` exit 0 before any toolkit starts
  [verified: `TestParseArgs`, `TestCheckDisplay`; the same three runs by hand, R11]. It runs the daemon in process (`ui.Bridge`, kind `ui`, wait for login, the same listener, control socket and `serve.lock` as `pcb serve`), shows
  a tray icon (three icons of our own: `ok`, `syncing`, `attention`; no Proton name or logo) and an 800 x 550 window (accounts on the left; per account the CalDAV switch, the CalDAV connection with the password masked behind Show and Copy, New password, Sign out or Sign in,
  Remove, the calendars and the alerts; "+ Add account" opens the wizard: address and password, an unattended-relogin opt-in with the one-line warning `pcb doctor` gives, the 2FA code, the CAPTCHA with "Open verification", then the CalDAV connection to copy;
  Settings: start at login, log level, open the logs folder or `config.toml`, Restart bridge, the doctor report). The logs folder opens as a `file://` URL with spaces and specials escaped [verified: `TestSettingsOpenFiles`, `TestFileURLEscapesSpacesAndSpecials`]. Closing the window hides it; Quit in the tray menu drains (5 s) and stops everything, after which `pcb status` says offline.
  `--background` starts in the tray only; when no tray host answers (GNOME without the AppIndicator extension) the window opens anyway and says once `your desktop shows no tray icons; on GNOME install the AppIndicator extension`,
  and closing it quits through the same drain as the tray's Quit, since nothing could bring a hidden window back (ruling R14) [verified: `TestCloseQuitsOnlyWithoutATray`].
  A Restart (or Start) that fails, for example on a `config.toml` edited with an error, shows `bridge stopped: <error>` with the `attention` tray, the actions refuse with the hint `restart the bridge (Settings → Restart bridge), then try again`,
  and the right pane says the bridge is stopped instead of offering an add; a later good Restart clears it [verified: `TestFailedRestartShowsTheBridgeStopped`, `TestBridgeStoppedPaneDoesNotOfferAnAdd`, `TestActionsRefuseWhenTheBridgeIsNotRunning`].
  What the window shows is a view model with no toolkit (`internal/ui`, the `cli` import rule, never `fyne.io`); the Fyne layer (`internal/ui/desktop`) only draws it and calls `ui.Actions` and `ui.Wizard`
  [verified: `TestUIArch`, `TestDesktopArch`, `TestTrayAttentionOnErrorAlert`, `TestTraySyncingBeforeFirstPass`, `TestTrayOK`, `TestShortTexts`, `TestAccountFields`, `TestCalendarBadgeFromGate`, `TestAccountListRendersView`,
  `TestTrayMenuFromView`, `TestTrayMenuWithoutAccounts`, `TestTrayIconFollowsState`, `TestIconsAreValidOwnArtwork`, `TestWindowSizeAndNoBranding`, `TestCloseHides`, `TestNoTrayLineShownOnce`; the real window, tray and Quit `[assumed → step 9]`].
- Single instance: `serve.lock` held. A second `pcb-ui` facing a `pcb-ui` sends `POST /v2/ui/show` (§15) and exits 0, and the first raises its window; facing `pcb serve` (or the service) it shows a dialog, then exits 20 with
  `pcb serve (PID <pid>) already serves this state directory`, hint `stop it (pcb service stop when installed as a service) and start pcb-ui again`. A failed start (this, a bad config, a listener that cannot bind) is a dialog
  with the error's message and hint; closing it, by its button or by the title bar's X, quits the app with the error's exit code
  [verified: `TestSecondInstanceShowsTheFirst`, `TestProbeNoDaemon`, `TestProbeNamesARunningServe`, `TestStartBlockedByAServeLeaksNothing`, `TestQuitDrainsAndReleases`, `TestRestartReadsTheConfig`, `TestBridgeNoticesTheDaemonEnding`;
  the dialog and the X `[assumed → step 9]`: Fyne's source says a master window's close quits, no test driver can show it].
- The window never holds a secret longer than a call: the wizard's password lives in the `Wizard` until the login ends (ready, an error or a 2FA step) and is then zeroed, except while a CAPTCHA waits (its retry needs it); Cancel zeroes it and removes an account the wizard created
  (an existing account is only signed out); a shown CalDAV password is hidden again when the account pane changes or the window hides, and one that arrives after that is dropped; a notification never carries a password or a URL (the CAPTCHA link is replaced by `the link in the pcb-ui window`)
  [verified: `TestWizardPlainLogin`, `TestWizardTOTP`, `TestWizardKeepsPasswordForCaptchaThenZeroes`, `TestWizardZeroesOnError`, `TestWizardZeroesTheBufferWhenTheCaptchaRetryFails`, `TestWizardCopiesThePassword`,
  `TestWizardCancelDuringTOTPLeavesNoAccount`, `TestWizardCancelDuringCaptchaLeavesNoAccountAndZeroes`, `TestWizardCancelKeepsAnExistingAccount`, `TestWizardErrorIsTheCLIText`, `TestWizardRefusesWhileTheBridgeIsNotRunning`,
  `TestWizardCancelAfterTheBridgeRestarted`, `TestShownPasswordHidesOnAccountChange`, `TestShownPasswordHidesOnHide`, `TestPasswordArrivingAfterPaneChangeStaysHidden`, `TestWizardClearsPasswordEntryOnSubmit`,
  `TestNotificationCarriesNoSecret`]. The wizard is the CLI's login (`Hub.Login`, `SubmitTOTP`; its errors are the CLI's message and hint); a CAPTCHA retry logs in again with the token Proton returned (ruling P4) and may wait up to 60 s on the login guard, which the window shows
  [verified: `TestWizardShowsWaitingAfterCaptcha`]. Account changes go through the Hub's own methods and are refused (`the bridge is not running`) once the daemon has ended [verified: `TestAccountActionsGoThroughTheHub`,
  `TestActionsRefuseWhenTheBridgeIsNotRunning`, `TestActionsRefuseAfterTheDaemonEnded`, `TestPasswordCreatesDefaultWhenMissing`, `TestNewPasswordStopsTheOldOne`].
- Notifications (the desktop's `org.freedesktop.Notifications`, over the session bus): one per occurrence of `needs-login`, `needs-captcha`, `human-verification`, `sync-stalled`, `keyring-locked` and `keyring-unavailable` (R15), whatever the alert's severity (ruling R9: `sync-stalled` is a warning, the code list is
  the contract); an alert that clears and comes back notifies again; the title is the account's address, the body the alert's message and fix. A click opens the window on that account; only the notification server's own signals count (the bus owner is checked, a forged click opens nothing)
  [verified: `TestNotifyOncePerOccurrence`, `TestNotifyTitleAndBody`, `TestNotifyCodes`, `TestNotifyStalledSyncWarning`, `TestNotifyIgnoresOtherCodes`, `TestNotifierIsSafeConcurrently`, `TestNotifyClickOpensAccount`, `TestNotifyIgnoresForgedSignals`,
  `TestNotifyForgetsIdsWhenServerChanges`, `TestNotifyNilOnClick`, `TestNotifyWithoutBus`; a real notification daemon `[assumed → step 9]`].
- Autostart (Settings, "Start at login"): `$XDG_CONFIG_HOME/autostart/pcb-ui.desktop` (default `~/.config/autostart`, the directory 0700) with `Exec=<absolute path of the running pcb-ui> --background`; off deletes only that file. A `%` in the path is escaped, a path with a space is quoted, a newline is refused
  [verified: `TestAutostartWritesDesktopFile`, `TestAutostartDefaultDirAndQuoting`, `TestAutostartExecEscaping`, `TestAutostartDirPrivateAndOffKeepsOthers`, `TestSettingsAutostartWritesTheResolvedExecutable`, `TestExecutableIsAbsoluteAndResolved`; a real login `[assumed → step 9]`].
- Build and install: `make ui` builds `bin/pcb-ui` (cgo, `CGO_ENABLED=1`; the GL and X development headers are build dependencies, README "Build"; it is not part of `make build` or the image, but `make lint test` compiles it: `go vet ./...`, golangci-lint and `go test -race ./...` build `cmd/pcb-ui` with cgo, so the gate needs a C compiler and the GL/X headers). `make install-ui` installs `$(PREFIX)/bin/pcb-ui`,
  `$(PREFIX)/share/applications/pcb-ui.desktop` and `$(PREFIX)/share/icons/hicolor/scalable/apps/pcb-ui.svg` (our own artwork, `internal/ui/desktop/icons/app.svg`; `packaging/` holds only the desktop file, ruling P7). The installed entry's `Exec=` is the absolute
  `$(PREFIX)/bin/pcb-ui`, written by `sed` from the template's `Exec=pcb-ui`, because launchers often lack `~/.local/bin` on their `PATH`; a `PREFIX` with spaces or `|` is not supported. `DESTDIR` is honoured.
  [verified: `make install-ui DESTDIR=<tmp> PREFIX=/usr` by hand: `Exec=/usr/bin/pcb-ui`, mode 644; CI builds `pcb-ui`, runs `--version` under `xvfb-run`, and its tests under `dbus-run-session`].
- `pcb doctor` (and the window's doctor report) warns `pcb-ui and the pcb service are both installed: only one can serve this state directory` (fix `uninstall one (pcb uninstall, or remove pcb-ui.desktop)`) when a pcb systemd unit is installed and `pcb-ui.desktop`
  lies in `$XDG_DATA_HOME/applications` (default `~/.local/share/applications`), `/usr/local/share/applications` or `/usr/share/applications` (ruling P3: a custom `PREFIX` install is missed, it is a warning only); quiet otherwise
  [verified: `TestDesktopCheckWarnsWhenBothAreInstalled`, `TestDesktopCheckIsQuietWithOnlyOne`, `TestDoctorWarnsBothInstalled`, `TestDoctorQuietWithOnlyOne`, `TestDoctorChecksShared`].

M8 behaviour (decision 0016; `internal/cli` `mcp*.go`, `internal/mcp`, `internal/access`; rulings in §18):
- `pcb mcp` speaks MCP on stdin and stdout and writes nothing else there; its logs and the audit lines of a session it
  serves go to stderr, at `[log] level`. `--json`, an unknown profile or capability, an empty `--allow`, `delete` without
  `write`, and `--profile` with `--allow` are exit 2; `--account` (repeatable) is not checked against the accounts, since a
  grant may name one a later login adds [verified: `TestMCPStdioWritesNothingButProtocolOnStdout`, `TestMCPStdioRefusesJSON`].
- At start, and whenever the daemon it relays to goes away: a daemon answering on the control socket gets the session
  relayed (a stopping one counts as none); else, with a system-scope pcb unit installed (detected as `pcb uninstall` does),
  exit 20 `pcb runs as a system service here`, hint `use its remote /mcp with a token ([mcp] remote = true; a loopback listen
  is enough)`, so a second state never serves the same Proton account unseen; else `pcb mcp` becomes the session daemon
  (kind `mcp`, §4.1). Two started together end as one session daemon and one relay. A start another pcb blocks (the
  state's locks) or a daemon that refuses the relay is retried after 250 ms, doubling, 10 s in all, then exit 20
  [verified: `TestMCPStdioRelaysToARunningDaemon`, `TestMCPStdioRefusesNextToASystemUnit`, `TestMCPStdioBecomesTheDaemon`,
  `TestTwoMCPStdioEndAsDaemonAndRelay`, `TestMCPLockRaceBacksOffAtMostTenSeconds`, `TestRelayRefusedIsNotADaemonGone`].
- A relayed daemon that ends mid-session: a tool call in flight is answered `retry` (it may or may not have taken effect:
  read before writing again), other requests are re-sent, and the next message probes again and either relays to another
  daemon or makes this `pcb mcp` the session daemon, without the agent initialising again (pcb replays its handshake)
  [verified: `TestRelayAnswersRetryWhenItsDaemonEnds`, `TestRelayTakesOverAfterItsDaemonEnds`, `TestStdioMovesFromARelayToTheProcess`].
  The end of stdin ends the session: answers still due get up to 5 s, the session daemon drains and releases the state,
  exit 0 [verified: `TestMCPStdioBecomesTheDaemon`; `make rehearse` steps 19 and 22].
- **Sealer guard** (ruling R20): on a state not created yet (no `layout.json`) under `[secrets] backend = auto` with no
  Secret Service answering (no session bus, none on it), the session daemon refuses to start, exit 10 on stderr, `pcb mcp
  will not choose how secrets are sealed: set [secrets] backend in <config path>, or run the first pcb login in a terminal`,
  and creates nothing; an explicit backend, an answering keyring or an existing state pass. It runs at start, not only at
  `login` as the design said, because opening a new state already records its sealer (§7.3) [verified:
  `TestSealerGuardRefusesWithoutAKeyring`, `TestSealerGuardAllowsAnExplicitBackend`].
- While a session daemon serves, `pcb serve` exits 20 naming `kind mcp` with the hint `end the agent session (pcb mcp), then
  try again`, and `pcb-ui` shows `an agent session (pcb mcp, PID N, kind mcp) already serves this state directory` with the
  same hint; `pcb status` shows `kind mcp` and no listener (§4.1, §15) [verified: `TestBridgeNamesAnAgentSession`,
  `TestServeLockNamesTheKind`; `make rehearse` step 22].
- `pcb mcp token add NAME --allow CAPS`: `CAPS` among `freebusy`, `read`, `write`, `delete` (duplicates dropped); `login` or
  `manage` is exit 2 `login and manage are stdio only: grant them with pcb mcp --profile`, `delete` without `write` exit 2
  `delete needs write: a grant that may delete may also edit` (ruling R19), an unknown one exit 2 naming the valid ones.
  `--account` (repeatable) must name an account (the daemon's view when one answers; else `no account X`, exit 2) and is
  stored as typed; `--calendar` IDs are stored unchecked; `--expires` takes whole days (`30d`) or a Go duration. The token is
  printed once (text with the `Authorization: Bearer` line to use and the remove command; `--json` in `data.token`);
  `--plan` checks everything and prints no token. Names follow the CalDAV client rules, unique case-insensitively
  [verified: `TestMCPTokenAddPrintsOnce`, `TestMCPTokenRefusesLoginCap`, `TestMCPTokenRefusesDeleteWithoutWrite`,
  `TestMCPTokenUnknownAccount`, `TestMCPTokenExpires`, `TestMCPTokenPlanChangesNothing`, `TestTokenAddRefusals`].
- `pcb mcp token list` (NAME, ALLOWS, ACCOUNTS, CALENDARS, CREATED, LAST USED, EXPIRES; `(expired)` marked; `--json`
  `{"tokens": [...]}` with `expired`; with none, `No MCP token yet` and a hint) and `pcb mcp token remove NAME`
  (an unknown name is exit 2; type `remove` or `--yes`; no terminal and no `--yes`: exit 10) [verified: `TestMCPTokenListEmpty`,
  `TestMCPTokenRemoveNeedsYes`, `TestTokenRemove`]. A token's last use is recorded at most once a minute
  [verified: `TestTokenVerify`].

Until the repository is public [call]: install from source (`make install` in a clone, then `pcb install`), or build
the container image locally (`make image`; the GHCR package `ghcr.io/barben360/proton-calendar-bridge` is published by CI on main (`:main`
and `:sha-<12 hex>`, first success 2026-11-12) and stays private, confirmed Private by the author on 2026-11-12 (`HUMAN-CHECKS.md` 7.8): pulling it needs
`docker login ghcr.io` with a token that can read packages, and everyone else builds it with `make image`; the model is the author's headless Proton Mail Bridge container:
one volume, `docker exec -it`). Updates and signed releases are not planned yet.

Later, when public: `install.sh` downloads a signed release (minisign, as
backsight [call]), verifies it, places the binary, then runs `pcb install`.
Services: systemd `--user` unit (with lingering instructions) or system unit
with a dedicated user [call]; container image with the same binary and
`pcb serve`.

## 14. Configuration

One TOML file, strict decoding (unknown keys and empty lists refused —
lesson from the proton-cal fork audit). CalDAV client credentials live in
`<state>/clients.json` (salted hashes and sealed copies, not the C2 SQLite store) and are managed by `pcb client`, not hand-edited [call].
`PCB_CONFIG_DIR` and `PCB_STATE_DIR` move the directories (§7.1).

`[caldav]` (M3): `listen` (HOST:PORT, explicit host, default
`127.0.0.1:1080`; `pcb serve --listen` overrides it under the same rule),
`hostnames` (DNS names or IPs clients use: certificate SANs, the first one
in the printed URL), `tls_cert` and `tls_key` (absolute paths, both or
neither; when set, TLS on any bind).

`[log]` (M5): `level` (`debug`, `info`, `warn` or `error`, lower case; default `info`; anything else is refused without
echoing the value); `pcb serve --log-level` overrides it [verified: `TestLogLevelDefaultsToInfo`,
`TestUnknownLogLevelIsRefused`, `TestServeHonoursTheLogLevel`, `TestServeRefusesABadLogLevel`].
`[secrets] key_file` (M1) is the path the container's compose secret is mounted at (§7.3).
`[secrets] backend` (M7): `auto` (default), `keyfile` or `secret-service`: which sealer a state gets when it is created (§7.3). `auto` takes the desktop keyring when an unlocked
one answers and the key file otherwise; it is never read again once `layout.json` records the sealer. Any other value is refused at load, exit 10, hint `fix <config path>`, without echoing it
[verified: `TestBackendDefaultsToAuto`, `TestBadBackendIsRefused`]. `pcb-ui` re-reads `[log] level` at each start of its bridge and does not write it back: the level chosen in its Settings lasts until the bridge restarts.
`[mcp]` (M8, §12): `remote` (default `false`: serve `/mcp` on the `[caldav]` listener, under its TLS rule, for agents with a
token; a `pcb mcp` session daemon never does, having no listener) and `timezone` (an IANA zone for times given without one;
default `""`, the process's zone; `Local` and paths are refused at load without echoing the value), strictly decoded like
every section [verified: `TestMCPConfigStrict`, `TestServeServesRemoteMCP`, `TestRunWithoutListenerOpensNoPort`]. The
`config.toml` that `pcb install` writes names both, commented [verified: `TestDefaultConfigGolden`, `TestDefaultConfigLoadsAsDefaults`].

## 15. Observability

Structured text logs on stderr (`log/slog` key=value lines, M3 `pcb serve`;
no event content, no secrets; IDs only). `pcb serve` logs one `render counters`
line per sync pass while a counter is non-zero, and once more when they change:
`undecryptable` (placeholders served), `unverified` (rows marked
`X-PCB-INTEGRITY:unverified`, §5.3), `duplicate_masters` (left out), `dropped_alarms` (malformed reminders left out) and
`keys_error_ids` (calendars whose keys failed). Writes (M4) log warnings with
IDs, fields and Proton codes only, never content: `refusing a write: a row's
key packet changed since pcb first stored it` (calendar, row, field),
`Proton refused a write` (calendar, entry or status, code), `a write met a
concurrent change of its event`, `a write is not confirmed by the rows read
back`, `a write is not confirmed: a row came back under other key packets`,
`a write could not be read back`, and the adapter's `caldav: the service
could not answer` for each 503. Every refused write (each 403, 409 and 412) logs one
`refused a CalDAV write` warning (the same code and href at most once per 10 minutes, then with `repeated=N`) with `method`, `status`, `code` (`pcb:<code>`, `need-privileges`,
`no-uid-conflict`, `precondition-failed`), `calendar`, `href` and a fixed `reason`, plus `conflict` (the other href) on a 409: never a body, summary, attendee or
password; the adapter's early 412 (an `If-None-Match` naming no ETag, a blank `If-Match`) is decided before any service call, so its `calendar` is empty. The
diagnostic warnings above still fire beside it for the same refusals (`HUMAN-CHECKS.md` greps them as stop conditions) [verified:
`TestEachRefusedWriteLogsOneWarning`, `TestRefusedWritesAreLogged`]. CLI commands that open the service print its warnings on stderr (§13). A
`clients.json` that cannot be loaded logs an error once per distinct failure (§9), and a failed
`state.json` mirror write logs a warning with a pcb-worded `cause`, never the Go chain [verified: `TestFailedMirrorWriteIsLogged`]. What a write normalised is logged at debug
(`write normalised`); `[log] level` or `pcb serve --log-level debug` (§14) prints it. Only the serving process holds
the render counters, so since M5 `pcb status` and `pcb doctor` read them from the daemon's control socket (M3-R34, below)
[verified: `TestServeLogsTheRenderCounters`, `TestServePassesCarryTheRenderCounters`].

Render counters and the read path (initial-version audit; `internal/service/render.go`, `internal/ical`):
- The status JSON's `render` object has five keys: `undecryptable`, `unverified`, `duplicate_masters`, `dropped_alarms`, `keys_errors`. The marks behind the first
  four are per row, pruned to the rows of the listing (a deleted row stops counting at once, also in an incremental `Changes`; another calendar's rows are never pruned)
  [verified: `TestRenderCountsFollowDeletedRows`, `TestRenderCountsFollowDeletedRowsInChanges`].
- Two exception rows with one RECURRENCE-ID: the most recently edited is served, the others count in `duplicate_masters` [verified: `TestDuplicateExceptionMostRecentWins`].
- C0 controls except HTAB (and DEL) are dropped from every served value, parameter and nested component; a property whose name holds one is dropped whole (TEXT `\n` escapes are untouched)
  [verified: `TestMergeEventDropsControls`; `FuzzMergeEvent` asserts no control but CRLF line ends and HTAB].
- A placeholder for a recurring timed row (RRULE or RECURRENCE-ID) is `DTSTART;TZID=<zone>:<local>` in the row's own zone when it is a tzdata zone, else UTC as before
  [verified: `TestPlaceholderRecurringUsesTheRowZone`].
- One malformed reminder (no Type, a Trigger that is not a duration) is dropped and counted in `dropped_alarms`; the event is still served, whereas the write path's validation stays strict
  [verified: `TestApplyNotificationsDropsMalformedReminders`].

Control socket (M5, `internal/ctl`; M5-Rpre1, R2, P2 to P5):
- `pcb serve` listens on `<state>/pcb.sock` (§7.1). The API is HTTP/1.1 under `/v2/` since M6 (`/v1/` is gone and answers 404, as does any unclean path), internal and versioned:
  `GET /v2/accounts`, `GET /v2/status` (every account; one account that cannot be read still gets its entry, with an `account-unreadable` alert, instead of failing the others),
  `POST /v2/login` (the username decides, §4.1), and per account, `{id}` being its local ID: `POST /v2/accounts/{id}/login/totp`, `/logout` (an optional body `{"discard": true}` is the confirmed discard of a store pcb cannot read, §7.3; an empty body is the zero request), `/remove`, `/caldav` and `/mcp` (M8; body `{"on": bool}`, required) and `/restart`. Each of them goes through the Hub (ruling R14), so its rules apply. `Daemon` carries `kind`, `serve` or `ui` (M7), and a `pcb-ui` daemon adds `POST /v2/ui/show` (below). Each answer is one `output.Envelope`:
  200 ok, 422 a command error (its exit code, message and hint), 400, 403, 404, 405 or 413 a transport error. Request bodies
  are at most 64 KiB and never logged (the log line holds method, path, status and exit). Client timeouts: 2 s to
  dial, 10 s for status, 180 s for login. Secrets travel only in request bodies.
- Peer check: on Linux the peer UID (`SO_PEERCRED`) must equal the daemon's effective UID, root included (root uses its
  own state directory or `sudo -u`); other Unix rely on the 0600 mode and the 0700 directory; off Unix there is no CTL
  and the CLI is always offline. Before dialling, the client checks that the socket file is a socket (not a symlink)
  owned by the caller's UID (M5-R2); on Linux it also checks the connected server's UID before sending a login, on
  every redial. A refusal exits 20 [verified: `TestV1RoutesAreGone`, `TestUncleanPathsAre404`, `TestAccountsRoute`, `TestAccountRoutesReachTheHub`, `TestUnknownAccountRoute`, `TestCalDAVNeedsItsSwitch`,
  `TestTOTPGoesToItsAccount`, `TestLoginResultNamesItsAccount`, `TestDaemonKind`, `TestOtherUIDIsRefused`, `TestDefaultPeerUID`,
  `TestDialChecksTheSocketBeforeDialling`, `TestSymlinkAtTheSocketPathIsRefused`, `TestClientRefusesAServerOfAnotherUID`,
  `TestOversizedBodyIs413`, `TestLoginBodyIsBounded`, `TestErrorsCrossTheSocket`, `TestStatusRefusesAForeignSocketPath`].
- Login through the daemon (M5-Rpre4, R12, P11, P12): with a daemon running, `pcb login`, `pcb login resume` and `pcb logout`
  go through it; without one the command says so on stderr (text mode only) and runs in process. A login waiting for
  its 2FA code is held in the daemon for 5 minutes, then revoked and its password zeroed. `pcb login resume --totp` sends
  the code (from `PCB_TOTP` or the prompt) and takes no credentials; with no daemon it exits 10 (`no pcb daemon holds a login
  waiting for its code`); it must be the only mode (exit 2 with `--captcha-token`, `--username` and the like). A second
  login while one runs is refused (exit 10, `a login is already in progress`); a new login replaces a held 2FA login and
  revokes its half session. `login resume --captcha-token` gathers the credentials again; only the post-SRP 2FA login is
  held [verified: `TestPendingLoginExpiresAfterFiveMinutes`, `TestPendingPasswordIsZeroedOnExpiry`,
  `TestReplacedLoginsTimerNeverRevokesTheNewOne`, `TestLoginOverTheSocketNeverLogsSecrets`,
  `TestSecondLoginWhileOneRunsIsRefused`, `TestLoginGoesThroughTheDaemon`, `TestLogoutGoesThroughTheDaemon`,
  `TestNonInteractiveTOTPIsHeldByTheDaemon`, `TestInteractiveTOTPThroughTheDaemon`,
  `TestResumeTOTPWithoutADaemonIsExit10`, `TestResumeNeedsExactlyOneMode`, `TestNoDaemonLoginSaysSo`].
  Ctrl-C at the 2FA prompt leaves the held login until it expires or is replaced (at most 5 minutes) [source: ruling
  M5-R12 (4); no test].
- `pcb status` since M6 (schema 2): `daemon` (null offline), `listener` (the base URL, TLS, fingerprint, certificate end), `alerts` (the process's) and `accounts[]`, each with its
  `AccountInfo` (`id`, `address`, `phase`, `pending`, `caldav`, `stopped`) flat, `login` (`service.Status`'s fields, which M5-P4 kept flat at the top level), `health` (serve state, cadence `interval_seconds`, `idle`, `last_pass`, per-calendar sync and write gate
  with its reason, render counters, human verification), `url` (its CalDAV URL while a listener is bound) and its own `alerts` [verified: `TestStatusListsEveryAccount`, `TestOneFailingAccountDoesNotFailTheStatus`, `TestVanishedAccountIsSkipped`].
  Text lines, once for the process then per account (the block starts at `account: <address> (<local-id>)`, marks `pending`, `CalDAV off`, `stopped`): first the login's own `key: value` lines (`phase:`, `secrets:`, `key_file:`, `unattended:`, `login_expires:`), then `daemon: serving since <t> (pid, version)` or `waiting for a login since <t>`
  (offline: `daemon: not running (offline view)`), `listener:`, `url:`, one `calendar:` line
  each, `render:`, `alert:`. Without a daemon it is the offline view (no network call, `daemon` null). It exits 0 whatever
  the alerts; `--daemon` exits 20 when none answers (the container's HEALTHCHECK uses it, M5-P5), with a hint that fits the
  host: `pcb service start` where a unit is installed, the container's restart where `/.dockerenv` or `/run/.containerenv`
  exists, else `pcb serve` (final review M2) [verified: `TestStatusDaemonHintFitsTheHost`]. `pcb doctor` adds a `service`
  result (the installed unit's state) and a `daemon` result and lists the alerts when a daemon answers; offline it shows
  none (a fresh, logged-out install must not exit 20); with a daemon waiting for a login it still exits 0, `needs-login` being a warning then (exit rule: §13) [verified: `TestDoctorNeedsLoginWhileTheDaemonWaitsIsAWarning`]. With a daemon answering, its `caldav listener` and `tls certificate`
  report the daemon's listener (address, TLS, fingerprint, certificate end) rather than what `config.toml` predicts, and warn
  when the daemon serves another certificate than the one on disk (M5-R15 C4) [verified: `TestDoctorUsesTheDaemonsListener`,
  `TestCalDAVChecksPreferTheDaemonsListener`]. Its `key file` check opens and reads the key as pcb does: a key this user cannot
  read fails with the cause (`not readable by this user (uid N): permission denied`), and so does a wrong size (M5-R15 C3)
  [verified: `TestKeyFileCheckReadsTheKey`]. Key errors of `pcb login`, `client add` and `client show` carry the OS cause or
  the mode refusal, never a secret (M5-R15 C2) [verified: `TestAddClientNamesWhyTheKeyCannotBeUsed`,
  `TestLoginNamesWhyTheKeyCannotBeRead`] [verified: `TestStatusShowsTheDaemon`,
  `TestStatusShowsADaemonWaitingForALogin`, `TestStatusFallsBackOffline`, `TestStatusDaemonFlagNeedsADaemon`,
  `TestOnlyNoDaemonFallsBackOffline`, `TestDoctorListsAlerts`, `TestDoctorReportsTheService`,
  `TestServeServesTheControlSocket`, `TestHealthReportsGatesWithReasons`, `TestHumanVerificationIsHeldUntilAWriteLands`].
- Alerts: `{account?, code, severity: error|warning, message, fix, calendar?}` (`account` the local ID; none on a process alert), errors first, then account, code, then calendar ID. Codes
  (stable): `account-stopped` (error, M6: the account's serve loop is parked, §4.1; message the stop error's, fix its hint, `pcb account restart` or a login clears it) [verified: `TestAccountStoppedAlert`],
  `account-unreadable` (error, M6: the account's login state or store cannot be read, with that error's message and fix; the other accounts still answer) [verified: `TestOneFailingAccountDoesNotFailTheStatus`, `TestStatusShowsEveryAccount`],
  `needs-login` (error; also a process alert, message `not logged in to Proton`, when no account exists, ruling R44: an existing account carries its own) [verified: `TestProcessNeedsLoginWithNoAccount`, `TestStatusOfNoAccountNeedsALogin`], `needs-captcha` (error, login), `human-verification` (error, API 9001, separate
  from the CAPTCHA because the fixes differ, M5-P7), `sync-stalled` (warning, per calendar: now minus the later of the last success and
  Serve's start is more than 5 intervals), `calendar-keys-failed` (error), `write-gate-undecided` (warning, names the calendar
  and why: closes the M4-R39 note), `clients-file-unreadable` (error: `clients.json` exists but cannot be loaded, so every CalDAV request answers 503; also in `pcb status` without a daemon and as the failed `caldav clients` check of `pcb doctor`, fix `fix or restore <path> (pcb client list shows the error)`), `tls-certificate-expiring` (warning, 30 days) and `tls-certificate-expired` (error; these two are the process's alone, `ProcessAlerts`)
  [verified: `TestAlerts`, `TestAlertsAreOrdered`, `TestProcessAlerts`, `TestCertificateAlertIsTheProcesss`, `TestStatusNamesAClientsFileThatCannotBeLoaded`].
- `keyring-locked` (M7; error, per account): the desktop keyring that seals the account is locked; message `the desktop keyring is locked`, fix `unlock it (log in to the desktop session), pcb retries every 30 s`; it clears by itself once the
  keyring unlocks (§7.3) [verified: `TestKeyringLockedAlert`].
- `keyring-unavailable` (M7; error, per account): no Secret Service answers for a keyring state (ruling R15); message `the desktop keyring is not available`, fix `run pcb inside the desktop session that holds the keyring (pcb doctor checks it); [secrets] backend only chooses for a new state`;
  the phase is kept, so no `needs-login` comes with it; it notifies in `pcb-ui` like `keyring-locked` (§7.3) [verified: `TestKeyringUnavailableIsNotANeedsLogin`, `TestNotifyCodes`].
- M7 additions (`internal/ctl`, `internal/daemon`, `internal/ui`, `internal/logfile`, `internal/doctor`):
  - `POST /v2/ui/show`, body `{"account": "<local-id>"}` (optional), answers `{"shown": true}`: the window raises itself, on that account when named. It is registered only when the daemon has a UI to show (`pcb-ui`; the callback runs on its own goroutine and never blocks the daemon).
    On `pcb serve` there is no such route, so the answer is the existing 404 envelope for any unknown path, `no such control endpoint: /v2/ui/show` (exit 20, hint `use the pcb of the same version as the daemon`; the plan said `unknown endpoint`, ruling R5: one wording for every unknown path).
    The peer-UID rule of this section applies like for every route [verified: `TestShowUIRoute`, `TestShowUIWithoutCallbackIsAnUnknownEndpoint`, `TestDaemonPassesShowUI`, `TestDaemonWithoutShowUIHasNoRoute`, `TestSecondInstanceShowsTheFirst`].
  - `ctl.BuildStatus` is the one implementation of the status the socket answers; `pcb-ui` calls it in process, so the window and `pcb status` cannot differ [verified: `TestStatusIsBuildStatus`].
  - `Status.Sealer` (`sealer` in the login block of `pcb status --json`): `keyfile` or `secret-service`; `secrets` may be `locked` or `keyring-unavailable` (R15); `key_file` is empty on a keyring state (§7.3).
  - `pcb-ui` logs to `<state>/logs/pcb-ui.log` (no terminal): files 0600 in a 0700 directory, rotated to `pcb-ui.log.1` once the file would pass 10 MiB, one rotation kept (10 MiB x 2), the same content and levels as `pcb serve`'s stderr (the sync pass lines, the
    certificate warnings, the render counters). A write that would pass the cap rotates first; one write larger than the cap is kept whole; a failed rename of the rotated file is retried only after another cap's worth of writes, so no line is lost
    [verified: `TestLogfileRotatesAtMax`, `TestLogfileKeepsOneRotation`, `TestLogfileOversizeWriteIsKeptWhole`, `TestLogfileReopenAppendsAndCountsExistingBytes`, `TestLogfileTightensAnExistingFile`, `TestLogfileConcurrentWritesAreWhole`,
    `TestLogfileWriteAfterCloseFails`, `TestLogfileOpenRefusesBadArguments`, `TestLogfileFailedRenameLosesNothing`, `TestLogfileImportsOnlyTheStandardLibrary`, `TestBridgeLogsPassesLikeServe`, `TestBridgeLogsTheCertificateWarning`].
  - `pcb doctor` gains a `sealer:` result (`sealer: keyfile` or `sealer: secret-service`, from `layout.json`). On a state not created yet it says `sealer: not chosen yet: the first login picks it ([secrets] backend = auto)` (just `keyfile` when `backend` is `keyfile`),
    and never probes the keyring then. For a keyring state it adds a `keyring` check (`the desktop keyring is unlocked`; locked is a warning with the alert's fix; no Secret Service on the bus is an error), the per-account secrets checks open the keyring item instead of a key file,
    and the `pcb-ui` check above. The state's sealer is built by the one `service.NewSealer`, with the absolute state path
    [verified: `TestSealerCheckOnAFreshStateDoesNotClaimTheKeyFile`, `TestAccountsReportsTheRecordedSealer`, `TestKeyringCheck`, `TestSecretsCheckWithTheKeyringNeedsNoKeyFile`, `TestSecretsCheckWithAKeyringMissingItsKeyErrors`,
    `TestSecretsCheckWithALockedKeyringLeavesItToTheKeyringCheck`, `TestSealedClientsCheckWithTheKeyring`, `TestDoctorSealerForARelativeStateDirUsesTheAbsoluteKeyringItem`, `TestDoctorChecksSecretsWithTheKeyring`, `TestDoctorWarnsLockedKeyring`,
    `TestDoctorReportsAKeyFileStateAsKeyFile`, `TestDoctorTextOK`].
  - The unattended-relogin warning is built by `doctor.UnattendedKeyringWarning(sealedPath)` (ruling R10; the plan wanted one shared constant, but `pcb doctor` names the full `secrets.sealed` path and the wizard, which does not know it, names `secrets.sealed`)
    [verified: `TestWizardUnattendedWarningIsDoctors`, `TestSecretsCheckWithTheKeyringNeedsNoKeyFile`].
- M8 additions (`internal/ctl`, `internal/daemon`, `internal/doctor`, `internal/mcp`; §12):
  - The control socket serves `/mcp` (every method, the exact path) to `pcb mcp` relays, after the peer-UID check and the
    unclean-path 404; its answers are MCP's own (no envelope), streamed, and its log line keeps method, path and status only.
    A daemon with no MCP server answers it with the usual 404 envelope [verified: `TestControlSocketServesMCP`,
    `TestControlSocketMCPRefusesOtherUID`].
  - `Status.MCP` (`mcp` in `pcb status --json`, with a daemon only): `remote` (whether `/mcp` is on the listener) and
    `sessions` (live MCP sessions per entry, `remote`, `relay`, `stdio`); text: `mcp: remote on|off; live sessions: remote N,
    relay N, stdio N`. `Daemon.kind` may be `mcp` (§4.1), with no `listener` [verified: `TestStatusShowsRemoteMCP`,
    `TestRemoteMCPServesTools`, `TestRunWithoutListenerOpensNoPort`; `make rehearse` steps 19 and 22].
  - `pcb doctor`: `mcp tokens` (ok with the count; an unreadable `mcp-tokens.json` is an error with the fix `fix or restore
    <path>`), one warning `mcp token NAME` per expired token (fix `pcb mcp token remove NAME`), and a warning `mcp remote`
    when `[mcp] remote` is on with a non-loopback bind and no usable token; nothing at all when no token exists and remote is
    off [verified: `TestDoctorWarnsRemoteWithoutToken`, `TestDoctorWarnsRemoteMCPWithoutToken`].
  - The audit line `mcp call` per tool call (§12) and, per session, the `pcb mcp` mode line on its stderr (`relaying to the
    daemon on the control socket` or `serving this agent session: kind mcp, no network listener`) [verified: `TestMCPStdioWritesNothingButProtocolOnStdout`]. No metrics for MCP:
    there is no Prometheus endpoint for CalDAV either.
- Prometheus endpoint: deferred (M5-Rpre11); status over the socket covers sync lag, Proton error classes and login state.

## 16. How it is tested

- Unit tests per block; every test must fail when its behaviour breaks
  (checked by sabotage in review, not mutation testing by default).
- **Fake Proton**: an in-repo fake of the calendar API driven by recorded,
  redacted fixtures from the test account; the same contract tests run
  against the fake (every CI) and the live test account (nightly canary +
  on demand) so a drift in Proton shows as a failing canary.
- **CalDAV**: contract tests from E08's requirements list (live Thunderbird traces to be added after the human check), a scripted client suite
  (python-caldav or vdirsyncer in CI [call]), and a manual Thunderbird
  checklist per release.
- **Codecs**: round-trip property tests, fuzzing, golden files.
- **Live**: only against the dedicated test account [decided]; never the
  author's real account in automated runs.
- CI: GitHub Actions, lint (golangci-lint), `go test -race`, fuzz smoke.
- **M7 (the keyring and `pcb-ui`).** No test touches the developer's keyring, bus or display:
  - *Secret Service*: a fake service (`internal/store/seal/sstest`) answers on a private `dbus-daemon` started by `internal/dbustest` with its own minimal configuration and no service directory, so activation can never start a real keyring daemon
    (the test skips when `dbus-daemon` is missing and fails when `CI` is set). It records the calls, so a test can assert that no `Unlock` or prompt was made and that the session algorithm was `plain`. The service, cli, daemon and caldav tests unset
    `DBUS_SESSION_BUS_ADDRESS` and `scripts/live/rehearse.sh` pins `[secrets] backend = "keyfile"`, so a developer's own keyring is never read.
  - *UI logic*: `internal/ui` is tested against the fake Proton through `service.Hub`, like the rest, with `-race`.
  - *Fyne layer*: `internal/ui/desktop` runs on Fyne's headless test driver (a widget changes on the main goroutine only; tests replace the window's dispatcher by a queue they drain, because the driver runs `fyne.Do` inline). The notification and autostart code runs on a private bus against a fake notification server.
  - *Why the GUI itself is a human step* (ruling R11): the agent that builds this has no display, and a real run would open a window on the author's desktop and could reach the author's real keyring. So it checks only `bin/pcb-ui --version` and a start with no display (exit 2, nothing created);
    CI runs the same under `xvfb-run`. What no test can show (the tray, a real notification and its click, the keyring prompt, closing the startup error dialog, autostart, a lock in Seahorse) is `spec/HUMAN-CHECKS.md` step 9 and carries `[assumed → step 9]` where it is claimed.
  - *No GUI dependency in `pcb`*: `cmd/pcb/arch_test.go` builds `./cmd/pcb` with `CGO_ENABLED=0` and fails when its dependencies include `fyne.io/` [verified: `TestPcbHasNoGUIDependency`]; `internal/ui` and `internal/ui/desktop` follow the `cli` import rule, `internal/logfile` imports only the standard library
    [verified: `TestUIArch`, `TestDesktopArch`, `TestLogfileImportsOnlyTheStandardLibrary`].

- **M8 (MCP).** Every tool is driven through the SDK's own client against `service.Hub` on the fake Proton or a stub
  hub, over the in-memory transport, the listener's `/mcp`, the control socket and stdin/stdout pipes, on the protocol
  versions 2025-11-25 and 2026-07-28 [verified: `TestRelayServesBothProtocolVersions`, `TestRunStdioServesBothProtocolVersions`].
  `TestNoSecretInAnyMCPOutput` scans every output of a whole session for the secrets of §12. `make rehearse` drives the
  real `pcb mcp` binary as an agent would against `pcb-fake`: a session relayed to `pcb serve` (step 19: tools of the full
  profile, `status`, `calendars_list`, the seeded series expanded by `events_list`, then `event_create`, `event_get`,
  `events_search`, `event_update` keeping location and alarm, a stale ETag refused as `conflict`, `event_delete`, each
  write counted at the fake, the audit line and no password in any output) and a session `pcb mcp` serves itself (step 22:
  kind `mcp`, no listener, `pcb serve` refused naming it) [verified: `make rehearse`, PASS on 2026-11-13]. A real agent
  client is `spec/HUMAN-CHECKS.md` step 10 `[assumed → step 10]`.

## 17. Experiments (assumptions to settle before the code that needs them)

| # | Question | Needed by | Method |
|---|---|---|---|
| E01 | Access/refresh token lifetimes; refresh rotation; what ends a session | C1 auth | test account: log in, refresh on a schedule, record |
| E02 | Is a solved CAPTCHA token valid in another process, and for how long? | C1 auth, CLI resume | trigger HV on the test account, solve, retry from a new process after delays |
| E03 | Accepted `x-pm-appversion` values and resulting scopes | C1 transport | try candidates, record scopes and calendar access |
| E04 | Calendar modelevents semantics (cursor, payload, gaps); push stream status | C4 sync | test account: edit events, read the feed (`pcb-lab probe feed`; source-based write-up `E04-calendar-feed.md`, live run pending) |
| E05 | Core event loop for calendar list / membership changes | C4 sync | same (`E05-core-events.md`; not used in M2, M2-R2) |
| E06 | Rate limits and 429 behaviour | C1, C4 | controlled bursts on the test account |
| E07 | What changes in a row on edit (modify time, sequence, parts) | ETags | edit fields one at a time (`E07-row-changes.md`, source-based, live run pending) |
| E08 | Thunderbird's CalDAV requests (discovery, sync, writes, errors) | A1 | read Thunderbird's source (done, `E08-thunderbird-caldav.md`); live capture against a reference server deferred to the human check |
| E09 | Decrypt cost per 1 000 events; cold-start time | C2 design (moved from M2 to M3, M2-R1) | offline: 0.2 to 0.4 ms per event, signatures checked [verified: `BenchmarkOpen`], `E09-decrypt-cost.md`; live half (test-account rows, cold start) pending |
| E10 | Which keys sign each part; does the member passphrase signature verify with our address key (R17) | C1 keys, M4 write gate | `E10-signatures.md`: `pcb calendar list`, the parts pcb writes, the render counters, on the test account (HUMAN-CHECKS §6); a shared calendar needs a second account |
| E11 | Does Thunderbird need VTIMEZONE? | C0 | answered from source inside E08 (§6.3, "Answers to SPEC questions"; no E11 file): **no** [source: E08]; the bridge emits none; live check pending with the Thunderbird session |
| E12 | Recurrence forms Proton refuses or alters | C0/C1 fidelity (M4) | `E12-recurrence-forms.md`: write each RRULE form and series operation through pcb, read back, compare in the web app and Thunderbird (HUMAN-CHECKS §6) |
| E13 | Unknown iCal properties: kept, dropped or refused by Proton? | C1 codec (M4) | `E13-unknown-properties.md`: write X- and standard extras, read back, edit in the web app, read again (HUMAN-CHECKS §6) |
| E14 | Member `Permissions`: the Write bit (owned calendars); view-only vs edit shares not planned | M4 write gate | `E14-member-permissions.md`: `pcb calendar list` on the test account, the lab probe if needed (HUMAN-CHECKS §6) |
| E15 | Secret sealing on a headless Linux host (systemd-creds, Secret Service) | C2 | try on a headless Debian 13 server |
| E16 | RRULE library audit vs our own | C0 | done (M8): rrule-go v1.8.2 not adopted (DST gap and overlap instants differ from RFC 5545 §3.3.5); own expander over Proton's set, `E16-rrule.md` |
| E17 | go-webdav audit: fit, bugs, client compatibility | A1 | done (M3): not adopted as server, test client only, `E17-go-webdav-audit.md` |
| E18 | (dropped: no REST API planned) | – | – |
| E19 | MCP Go SDK: version, names, CGO-freeness, Host/Origin checks, which agent clients declare elicitation | A4 (M8) | done (M8): `modelcontextprotocol/go-sdk` v1.8.0 pinned, pure Go (Go ≥ 1.25), stateful streamable HTTP, elicitation through `InputRequests`, the SDK checks loopback `Host` but never `Origin`; client declarations from Claude Code's docs, live check in HUMAN-CHECKS 10; `E19-mcp-go-sdk.md` |

Status at the close of M0 (2026-11-11):
- E03: gate passed on the fork's evidence (R13); scope list and other candidates pending the live run.
- E15: done (user-scope systemd-creds, Secret Service presence, key file verified; TPM2 and desktop system scope untested).
- E08, E11: answered from Thunderbird's source; live Thunderbird session pending.
- E01, E02: probes built and tested offline, no live measurement yet (the test account's CAPTCHA is unsolved); their markers in §5.2 stay `[assumed]`.
- Live runs, in order: `spec/HUMAN-CHECKS.md`.

Status at the close of M2 (2026-11-11):
- E04, E05, E07: source-based write-ups (`E04-calendar-feed.md`, `E05-core-events.md`, `E07-row-changes.md`; evidence `raw/m2-feed-research.md`), every marker stays `[assumed]`; the live run is deferred ("M2 live session" in `spec/HUMAN-CHECKS.md`, recorded with `pcb-lab probe feed`).
- E09: moved to M3 with event decryption (M2-R1); the M2 cache stores ciphertext only.
- E06 (rate limits): not started; M2 relies on the client's limiter and on honouring `Retry-After`.

Status at the close of M3 (2026-11-11):
- E08: implemented and contract-tested ("What the bridge does (M3)" in `E08-thunderbird-caldav.md`); live Thunderbird session after M4 (`spec/HUMAN-CHECKS.md` step 6).
- E09: offline measured, live half pending. E11: answered from source in E08, live check pending. E17: done.

Status at the close of M4 (2026-11-12):
- E10, E12, E13, E14: source-based write-ups, each with what the code assumes and the exact live steps; the code gatekeeps so that a wrong assumption fails closed (read only, 403 or 503), never writes wrong data silently. The live run is `spec/HUMAN-CHECKS.md` step 6 (the M4 live session, with the Thunderbird session of M3).
- E08: writes implemented and contract-tested (MUST 10 and 11, "What the bridge does"); live with step 6.
- E06 (rate limits): not started; writes are never retried and a 429 is a 503 with Proton's `Retry-After`.

Status at the close of M5 (2026-11-12):
- E15: the key file is the only sealer built; the smoke tests ran pcb as a systemd user and system service and in a container (logged out: no sealed session yet), see `E15-secret-sealing.md`; the sealed session in a service and a container, TPM2 and the other backends stay untested (step 7).
- E08 §3.7 (Thunderbird's 30-minute refresh) fixes the idle threshold at 35 min (§8.1); whether Thunderbird behaves so is step 7.4.
- E06 (rate limits): not started; the idle cadence and the back-off are unchanged by it.
- Live run: `spec/HUMAN-CHECKS.md` step 7.

Each experiment gets `spec/experiments/E##-<slug>.md`: question, method,
raw results (redacted), conclusion, and the decision it feeds.

## 18. Roadmap (blocks, each reviewed and merged on its own)

| Milestone | Blocks | Done when |
|---|---|---|
| M0 Foundations | **E03 first** (client identity with calendar scope), then repo layout, CI, conventions, E01, E02, E15; synthetic data set on the test account (years of events, recurrences, exceptions, all-day, time zones) | experiments written up; `pcb version`, `pcb doctor` skeleton |
| M1 Proton core, read | C0 (read side), C1 auth + keys + read endpoints, C2 secrets, `pcb login` (password, TOTP, CAPTCHA resume, unattended relogin) | `pcb calendar list` against the test account |
| M2 Store and sync | C2 cache, C4 initial + incremental (E04, E05, E07; E09 moved to M3) | cache follows edits made in the Proton web app |
| M3 CalDAV read (built, live session after M4) | A1 read, client credentials, TLS for non-loopback binds, E08, E11, E17 | Thunderbird (local and over the network) shows all calendars and stays in sync: contract-tested against a fake Proton; the live check is `spec/HUMAN-CHECKS.md` step 6 |
| M4 CalDAV writes (built, live session pending) | C1 codec write path, A1 writes, fidelity table (E12, E13), write gate (E10, E14) | Thunderbird two-way edits, incl. single occurrences: contract-tested against a fake Proton; the live check is `spec/HUMAN-CHECKS.md` step 6 |
| M5 Daemon and ops (built, live session pending) | C6, `pcb install/uninstall/service/doctor`, container image | fresh Debian host and a container both set up from the terminal only: smoke-tested in CI (a Debian container with systemd as PID 1, the image); the live run is `spec/HUMAN-CHECKS.md` step 7 |

| M6 Several accounts per process (built 2026-11-12, live session pending) | `service.Hub`, layout v2 and the M5 migration, per-account CalDAV URLs, selector and `pcb account`, control socket v2, `internal/daemon`, `output` schema 2 | two accounts served side by side on the fake Proton, an M5 install upgraded in place keeps its session, cache and CalDAV passwords: contract-tested against the fake only; the live run is `spec/HUMAN-CHECKS.md` step 8, on the one test account |
| M7 `pcb-ui` desktop app (built 2026-11-13, live session pending) | a separate cgo binary running `internal/daemon` in process: tray, account window, add-account wizard, notifications, autostart; the Secret Service sealer (§7.3); `pcb-ui` log file; `make ui` and `make install-ui` | the UI logic and the keyring sealer are tested against the fake Proton, a fake Secret Service and Fyne's headless driver; the GUI itself is `spec/HUMAN-CHECKS.md` step 9, in the step 8 throwaway VM with a GNOME session, the test account only (decision 0015) |
| M8 MCP adapter (built 2026-11-13, live session pending) | E16, E19; the structured event layer (`ical.Expand`, `DecodeEvent`, `ApplyEdit`, `service.Agenda`); the per-account MCP switch; `internal/mcp` (tokens, grants, the eleven tools, untrusted marking, audit); remote `/mcp`; `/mcp` on the control socket; `pcb mcp` (relay or session daemon); `login` and `logout` (`pcb-ui` wizard or loopback page) | every tool tested through the SDK's client against the fake Proton, a secrets scan over a whole session, and `make rehearse` driving `pcb mcp` against `pcb-fake`; Claude Code over stdio and remote, the loopback login and one create/update/delete seen in the Proton web app are `spec/HUMAN-CHECKS.md` step 10, the test account only (decision 0016) |

Nothing else is planned [decided 0007; MCP by 0016].

Initial-version audit (2026-11-12, main at 557fe84): six read-only auditors, 0 Critical, 17 Important, about 48 Minor findings; the Important ones and the Minors worth their cost are fixed on branch `audit-fixes`, the rest parked in `OPEN.md` ("Parked by the initial-version audit") or accepted, each with its reason in `spec/audits/2026-11-12-initial-version.md`.

M2 delivered (2026-11-11): the SQLite cache (`internal/store/cache`, ciphertext rows only), the syncer (initial, feed-driven incremental, fallbacks), `pcb sync [--full] [--watch]`, `pcb event list`, the doctor cache check and `pcb-lab probe feed`. Partial: partial sync = exit 40 (M2-R11). Deferred: the live session against Proton (settles E04, E05, E07; `spec/HUMAN-CHECKS.md`); the core event loop (M2-R2, until E05); event decryption, the JSCalendar model (dropped, 0012), the iCalendar codec and E09 (M3, M2-R1); the daemon-side watch with idle slow-down (M5). Rulings recorded: M2-R1 decrypt/E09 to M3; M2-R2 no core loop; M2-R3 Action ignored; M2-R4 ETag over the canonical row; M2-R8 a refused row read makes the calendar resync in full; M2-R10 fallback gap 5 min; M2-R11 deferred counts as partial; M2-R12 Ctrl-C of watch exits 0; M2-R13 cache error classes.

M3 delivered (2026-11-11, branch m3-caldav-read): event decryption and the iCalendar merge (`internal/proton/cards`, `internal/ical`), the render and sync-token layer in `service.Local.Serve`, `internal/caldav` (read-only CalDAV), `pcb client`, `pcb serve` with generated or user TLS, the doctor checks for listener, certificate and clients, E08 contract tests, E09 (offline), E17. Deferred: the live Thunderbird session and the live halves of E08/E09/E11 (after M4); writes (M4); the daemon behaviours (M5). Rulings recorded: M3-R1 privileges read+write on owned calendars, live session after M4; R2 iCalendar-native (decision 0012); R3 duplicate masters; R4 placeholders; R5 own server; R6 serve reads on the writer's handle; R9 to R12, R16 tokens and ETags; R15 writability from member permissions parked to M4; R17 to R21 client store and limiter; R22 to R26, R30 503 rather than 4xx; R28 to R32 `pcb serve` and TLS.

M4 delivered (2026-11-12, branch m4-caldav-write): the fake Proton's sync endpoint and `?UID=` listing (`internal/proton/protontest`), the write gate with the member passphrase signature (`internal/proton/calendar`), the write side of the iCalendar codec (`ical.ParseWrite`, `Route`, `Patch`, VALARM → `Notifications`), sealing (`cards.Seal`, the session-key proof), `events.ListByUID` and `events.Sync`, the cache's `aliases` and `pins` (schema 2), the planner and executor (`service.PutObject`, `DeleteObject`), CalDAV PUT and DELETE with served names, `spec/mapping.md`, E10, E12, E13, E14 write-ups. Deferred: the live session (`spec/HUMAN-CHECKS.md` step 6) and every live marker above; fresh session keys on update (after L.4 #3); the daemon behaviours (M5). Rulings recorded (`.superpowers/sdd/2026-11-11-m4-caldav-write/progress.md`): Rpre1–3 package seams; R1 the write gate; R2, R3 the fake as an oracle, stricter than Proton where marked; R4 SEQUENCE (and fail closed on two primary keys); R5 MONTHLY ordinal → BYSETPOS; R6 aliases, X-PM-CONFERENCE-ID shared signed; R7 strip pcb lines, keep stored COLOR and components; R8 DTSTAMP and DTEND defaults; R9 attendee parameter edits ignored; R10 the RRULE allow-list; R11 Thunderbird's faked master dropped; R12 DAILY BYDAY → WEEKLY; R13, R15 no new session key on update; R14, R16, R19, R20 the session-key proof; R17, R30, R35, R36, R37 key-packet pinning; R18 only our own verified parts are rewritten; R21, R22 ApplyWrite and alias lifetime; R23, R24 one resend after a 401, 2001 not split; R25 Task 7 in two; R26 controller re-review; R27 deletes skip the trust checks; R28 new exceptions take the master's attendees; R29 colours and mirrored exception rows; R31 an ambiguous call confirmed by the read-back is a success; R32 confirmation by plaintext; R33 names from the service; R34 412 for a concurrent change, 403 for a refusal; R38 alternative names; R39, R40 undecided gate is 503 with no privileges advertised; R41 displaced hrefs kept.

M5 delivered (2026-11-12, branch m5-daemon-ops; the live checks of `spec/HUMAN-CHECKS.md` step 7 are pending): the paths overrides and `[log]` (`internal/paths`, `internal/config`), the idle cadence and `Health` (`internal/syncer`, `internal/service`), the control socket, alerts and live `pcb status`/`pcb doctor` (`internal/ctl`), `pcb serve --wait-for-login` and login through the daemon, the sealed-secrets generation and the `state.lock` (`internal/store`), `pcb install`, `uninstall` and `service` (`internal/install`), viewable CalDAV credentials (decision 0013: `pcb client show`, `rotate`, the `default` client), the container image, `make container-smoke` and `make systemd-smoke`, and `.github/workflows/image.yml` (build and smoke on pull requests, push to private GHCR on main). Deferred: the live session (step 7); the Prometheus endpoint (Rpre11); `pcb config`; plain HTTP behind a reverse proxy; the items under "Later" in `OPEN.md`. The two workflows have not run in GitHub Actions: they need the push over SSH. Rulings recorded (`.superpowers/sdd/2026-11-12-m5-daemon-ops/progress.md`; the plan `spec/plans/2026-11-12-m5-daemon-ops.md`):

| Ruling | Decision |
|---|---|
| Rpre1, R2, P2, P3 | CTL: HTTP/1.1 on `<state>/pcb.sock`, 0600 in a 0700 directory, SO_PEERCRED on Linux, `/v1/`, internal; the client checks the socket's owner and type before dialling; stale sockets replaced without a probe |
| Rpre2, P4, P5, P7 | live `pcb status` and `pcb doctor` over CTL; flat status JSON plus `daemon`, `health`, `listener`, `alerts`; `status --daemon` exits 20 without a daemon; separate `needs-captcha` and `human-verification`; `sync-stalled` per calendar |
| Rpre3, R8 to R10, P8, P9, P10, P22 | `--wait-for-login`: wait only for a stop a login fixes; 503 for an unknown calendar until the first pass; authentication before the 503; login seen by a 5 s poll or an in-process wake; logout halts Serve first; a second signal exits 20 |
| Rpre4, R12, P11, P12 | login through the daemon: 5 min TTL; `login resume --totp` is a boolean, the code from `PCB_TOTP` or the prompt; a new login revokes the held half session; flags that do not apply to `--totp` exit 2 |
| Rpre5, Rpre6, R4 to R7, P16 to P19, P23 | `install`/`uninstall`/`service`: `--user` default, `--system` copies the binary to `/usr/local/bin/pcb`; `--plan` never needs root; `--delete-data` removes only directories named `pcb`, reads the unit's own environment, keeps a key file outside the config directory; start/stop/restart need root on the system scope; units set the directories explicitly; `make install` |
| Rpre7, P20, P21 | image: distroless static nonroot (65532), `golang:1.27-trixie` builder, both pinned by digest, `CGO_ENABLED=0`, one volume `/data`, TLS on 1080, exec-form HEALTHCHECK; both smoke tests run in CI (`--privileged --cgroupns=host`) |
| Rpre8 | `[log] level` and `--log-level` |
| Rpre9, P6, R1 | cadence 30 s, 5 min after 35 min idle; a partial pass keeps the cadence |
| Rpre10, P14, P15, R3 | R3m: sealed generation in v2 `secrets.sealed`, mirrored in `state.json`, a restore of the whole directory undetected; R10m: `state.lock` through `filelock`; the in-memory session is revoked on a rollback |
| Rpre11 | Prometheus deferred |
| Rpre12 | done when: both smoke tests in CI; the live run is step 7 on the test account only |
| P1, P13 | `PCB_CONFIG_DIR`/`PCB_STATE_DIR` verbatim and absolute; the write gate's last decision stays per process (M4-R40 kept) |
| 0013, R13, 7b-R1, R14 | CalDAV passwords sealed beside their hash and shown again; no CTL endpoint for `show`/`rotate`; logout keeps the key file; a pcb from before M5 refuses a v2 `clients.json` (no downgrade); `client add` and `rotate` refuse while the key is missing but `secrets.sealed` exists (`pcb logout`, then `pcb login`); offline `show` ignores a `--listen` override |

M5 carry-overs from M1 to M4, as closed:
- R3m (rollback of `secrets.sealed`): done, a monotonic generation sealed and mirrored (§7.3) [verified: `TestRestoredOlderSecretsAreRefused`].
- R8m (a pending login with no timeout): done, 5 min TTL in the daemon [verified: `TestPendingLoginExpiresAfterFiveMinutes`].
- R10m (a leaf lock on every `state.json` writer): done [verified: `TestUpdateStateWaitsForTheLock`, `TestHumanNeededDoesNotOverwriteAFinishedLogin`].
- R17: done in M4 (§5.3); live confirmation is E10, step 6.
- M4-R39 (a calendar whose write gate stays undecided is named by `pcb status` and `pcb doctor`) and M4-R44 (human verification in `pcb status`): done, alerts `write-gate-undecided` and `human-verification` (§15).
- R16 (`pcb login resume --totp`): done, as a boolean (M5-P11).
- Open and parked, not carried: see "Later" in `OPEN.md`.

M6 delivered (2026-11-12, branch m6-multi-account; the live checks of `spec/HUMAN-CHECKS.md` step 8 are pending, test account only): `store` layout v2 and the crash-safe M5 migration (`internal/store/layout.go`), the fake Proton's per-user failure injection, `Local` on an account directory with a shared guard and limiter, `service.Hub` (accounts, login rules, pending adds, sign-out, removal, the CalDAV switch, one serve loop per account, the exit rule, the router), the CalDAV router with per-account URLs, control socket v2, `internal/daemon` (the wiring out of `internal/cli/serve.go`), the selector, `pcb account` and schema 2 in the CLI, the doctor's layout and per-account checks, and the two-accounts and upgrade contract tests (`internal/daemon`). Author's choice, recorded: no second Proton test account, so **multi-account is proven on the fake only** (two or more users); the live run uses one account. Deferred: the live session (step 8) and `pcb-ui` (M7). Rulings recorded (`.superpowers/sdd/2026-11-12-m6-multi-account/progress.md`; plan `spec/plans/2026-11-12-m6-multi-account.md`; design `spec/designs/2026-11-12-multi-account-and-ui-design.md`):

| Ruling | Decision |
|---|---|
| R1 | `internal/store` tests write an M5 fixture with a minimal `clients.json` literal (no `internal/access` import); the real-format migration test is `TestOpenHubMigratesM5State` |
| R2 | `OpenHub` takes the root `sync.lock` (and `layout.lock`) only while it migrates an M5 layout; with layout v2 it never touches them, so CLI commands work while a daemon serves |
| R3, R18, R19, R22 | the Hub runs every `Local.Serve` with `WaitForLogin` and applies the process exit rule itself; a removed account's loop stops first; a login re-reads CalDAV, address and creation time under `state.lock` (a CalDAV switch is not lost to a relogin); `ServeStopped` is reused for parked, paused and ended loops |
| R4, R5, R26 | `service.AccountPath` is the one URL helper; `daemon.Hub` is the interface `daemon`, `ctl` and `cli` share (`*service.Hub` satisfies it); `Router.Route` returns `(api, localID, retryAfter, err)` so the handler knows which account `/` authenticated |
| R6, R7, R9 | legacy detection counts data files only, never locks; `Migrate` leaves the root `sync.lock` in place; `ErrNewerLayout` for a `layout.json` above version 2 (exit 10 since R36, `upgrade pcb`) |
| R8 | the `state.json` key is `address` (the design wrote `username`) |
| R10, R12, R14, R15, R17 | `Hub.SubmitTOTP` applies the same post-login rules as `Hub.Login`; a duplicate-user refusal revokes the session it opened first; account-changing commands go through Hub methods (CTL when a daemon answers, since a running Hub does not rescan `accounts/`); `SignOut` fires `OnChange` |
| R11, R13 | an abandoned add (no Proton user, never logged in, no data, no CAPTCHA, no held 2FA login) is removed lazily and by `OpenHub` once older than the 2FA TTL plus a minute |
| R16, R37 | `OpenHub` backfills a migrated account's address from its sealed username; a migrated signed-out account with no address (and a readable `state.json`, R37) is adopted by the first login that matches no account; cost: that login inherits its `clients.json`, so the old Thunderbird password reads the new user's calendars (`pcb client rotate`, §4.1) |
| R20 | `serve.lock` holds the PID as decimal text and a newline |
| R21, R23, R25 | a damaged cache parks the account with exit 10 (not 20), a sync lock held elsewhere is retried every 30 s and never parked, and a Restart racing the park is a no-op that a later Restart repairs |
| R24 | the serve half of `hub.go` is `hub_serve.go`, the account API `hub_api.go` (a pure move) |
| R27 | an unknown but well-shaped local ID is 401 realm `pcb <id>` with a limiter charge (no probe oracle); a first segment that is not an ID is a compatibility path |
| R28, R29 | a client of an account whose CalDAV is off is 401 with no charge; while any `clients.json` is broken an unknown password on a compatibility URL is not charged |
| R30 | the adapter's log carries `account=<local-id>`; an unreadable `state.json` is 503 with no charge, a Router error 503 |
| R31 | the control error envelope carries `data.accounts` of a several-accounts refusal and the client rebuilds an error exposing it |
| R32 | an unclean control path (the ServeMux 301) is a 404 envelope |
| R33 | `pcb serve` opens the Hub itself (`service.OpenHub`) and runs `daemon.Run` with kind `serve` |
| R34 | upgrade-path fixes: `pcb doctor` warns on a `.migrating` directory and names the layout fix; `pcb logout` with only a pending account acts on it; offline status drops an account only on the Hub's "no account" refusal |
| R36 | layout refusals (files of an older pcb beside the layout, a newer, damaged or unreadable `layout.json`) are exit 10, as state faults; another pcb holding `layout.lock` or the root `sync.lock` stays exit 20 |
| R38 | with no selector, no account (or only pending adds) is exit 10; a selector naming none and several accounts stay exit 2; `ErrNoAccount` stays the cause, which offline status and the control socket's status match |
| R39 | a CalDAV request skips an account being removed instead of waiting for its revoke; the adoption cost of R16 is documented (§4.1) |

Where the built behaviour differs from the design (§4 of the design document, each marked there): the `state.json` key `address`, not `username` (R8); one `caldav` boolean, not an `adapters` object (§7.1); the root `sync.lock` stays at the root and is held by the serving process and the migration (R7); a held per-account sync lock is retried, not parked (R23); a damaged cache parks with exit 10 (R21); root `serve.lock` and `layout.lock` are new; stale root `state.lock`, `secrets.lock` and `clients.lock` an old binary recreates are never cleaned (§7.1).

M7 delivered (2026-11-13, branch m7-pcb-ui; the live checks of `spec/HUMAN-CHECKS.md` step 9 are pending, test account only, in the throwaway VM (a GNOME session)): the Secret Service sealer (`internal/store/seal`, `internal/dbustest`), the sealer recorded in `layout.json` and chosen once when a state is created (`store.CreateLayout`, `service.OpenHub`, `[secrets] backend`), the locked-keyring wait and alert, the doctor's `sealer`, `keyring` and `pcb-ui` results, `/v2/ui/show` and `ctl.BuildStatus`, `internal/logfile`, `ui.Bridge` (the daemon in process), the view model, actions, wizard and notification rules (`internal/ui`), the Fyne window, tray, notifications and autostart (`internal/ui/desktop`), `cmd/pcb-ui`, `make ui` and `make install-ui`, and the CI job that builds and tests it. Deferred: the GUI session (step 9); a Windows build; the minors in `OPEN.md` ("Parked by M7"). Rulings recorded (`.superpowers/sdd/2026-11-13-m7-pcb-ui/progress.md`, kept out of the repository; the plan `spec/plans/2026-11-13-m7-pcb-ui.md` is left as written):

| Ruling | Decision |
|---|---|
| P1 | `TestSecretServiceBindsItsName` lives in an external test package (`package seal_test`): `internal/store` imports `seal`, so a store test inside `seal` would be an import cycle |
| P2 | `Migrate` writes `sealer: keyfile` whenever it writes `layout.json`; `OpenHub` on a new state chooses the sealer and calls `CreateLayout` (which also creates `accounts/`) under the layout lock before `Migrate`, which then sees version 2 |
| P3 | the doctor looks for `pcb-ui.desktop` in `$XDG_DATA_HOME/applications` (default `~/.local/share/applications`), `/usr/local/share/applications` and `/usr/share/applications`; a custom `PREFIX` install is missed (a warning only) |
| P4 | `Wizard.CaptchaSolved(ctx)` logs in again with the `LoginResult`'s CAPTCHA token, as `pcb login` does; the plan's `CaptchaSolved(ctx, token)` misread the CLI, which reads no token from the user |
| P5, P6 | the desktop package defines `WizardModel` (the wizard's methods; `*ui.Wizard` satisfies it, so tests fake it) and the tray takes a two-method interface (`SetSystemTrayMenu`, `SetSystemTrayIcon`) |
| P7 | no packaging copy of the icon: `make install-ui` installs `internal/ui/desktop/icons/app.svg` as `pcb-ui.svg`; `packaging/` holds only the desktop file |
| P8 | the GL and X headers are installed in the CI jobs that need them: `check` (the linter type-checks `cmd/pcb-ui` with cgo) and the new `ui` job (builds `pcb-ui`, runs `--version` under `xvfb-run`, and its tests under `dbus-run-session`); the `rest` test shard leaves `cmd/pcb-ui` out but still compiles `internal/ui/desktop` without the headers installed, and the first CI run decides whether it needs them (not run yet) |
| R1 | the keyring's `CreateItem` replaces, so two first sealers could overwrite each other's key: the first seal runs under `<state>/sealer.lock` and re-reads the stored item |
| R2 | an invalid `[secrets] backend` is exit 10 (a person must fix the file), like `[log] level` and every config fault, not 2 as the plan wrote |
| R3 | a mid-serve locked keyring also waits, retries every 30 s and shows `keyring-locked` (the design says accounts wait), and the errors name the keyring on a keyring state |
| R4 | `scripts/live/rehearse.sh` pins `[secrets] backend = "keyfile"` and unsets the bus address, so a rehearsal never touches the developer's keyring |
| R5 | a daemon without `ui/show` answers the existing `no such control endpoint: <path>` 404, not a new `unknown endpoint` |
| R6 | a daemon that ends by itself after Ready pokes `Changes`, and the next Restart starts a new one (the tray must not show a dead bridge as alive); the pass lines and warnings `pcb serve` logs also go to `pcb-ui.log` (`daemon.LogPass`, `RenderLog`) |
| R7 | the view carries `Bridge` (`bridge stopped: <err>`, tray `attention`) and the actions refuse while the bridge is not running |
| R8 | the one-line state precedence is: stopped, keyring locked, keyring unavailable (R15), login phase, CalDAV off, syncing, synced; a loop that reports no interval is shown against the 5 min idle cadence |
| R9 | the notifier notifies the listed codes (six since R15: `needs-login`, `needs-captcha`, `human-verification`, `sync-stalled`, `keyring-locked`, `keyring-unavailable`) whatever their severity, so the `sync-stalled` warning notifies (the plan's "error-severity alerts" would have skipped it) |
| R10 | the unattended-relogin warning is a format function, `doctor.UnattendedKeyringWarning(sealedPath)`, not one shared constant, so `pcb doctor` keeps the full path |
| R11 | the plan's manual run of `pcb-ui` against `pcb-fake` is not done by the agent (no display; a real run would open a window on the author's desktop and could probe the author's real keyring): `--version`, a no-display start and CI under `xvfb-run` instead, and the GUI session is step 9 |
| R12 | `spec/HUMAN-CHECKS.md` step 9 runs in the throwaway VM with a GNOME session, not on the author's workstation |
| R13 | `pcb uninstall --delete-data` deletes the state's `pcb sealing key` item when `layout.json` records `secret-service`; a locked or absent keyring is a note with the Seahorse fix, not a failure; `pcb account remove` keeps it |
| R14 | with no tray host, closing the window quits through the tray Quit's drain instead of hiding (a hidden bridge with no icon and no Quit would hold `serve.lock` unseen) |
| R15 | `seal.ErrNoService` is Blocked `the desktop keyring is not available` with a desktop-session hint; `pcb status` shows secrets `keyring-unavailable` with its own alert and keeps the phase, never `needs-login` |
| R16 | a login on a keyring state checks the keyring (locked, service present) before SRP, so a doomed seal spends no Proton login and no login-guard slot |

Where the built behaviour differs from the design (§5 of the design document, each marked there): the sealer is chosen when the state is created, by `[secrets] backend`, and the key file stays for every existing state; the 404 wording (R5); `sync-stalled` notifies although it is a warning (R9); the unattended-relogin warning is a function (R10); `CaptchaSolved` takes no token (P4); the log level chosen in Settings is not written to `config.toml`; no Proton login is made while the keyring is locked; the GUI is a human step (R11).

M8 delivered (2026-11-13, branch m8-mcp; the live checks of `spec/HUMAN-CHECKS.md` step 10 are pending, test account only): E16 (own recurrence expander, rrule-go not adopted) and E19 (the MCP Go SDK v1.8.0), the event view, recurrence expansion and edits as patches (`internal/ical`), `service.Agenda` and `Hub.Agenda`, the per-account MCP switch (`pcb account mcp`, the loop rule "either adapter on"), MCP tokens (`access.TokenStore`, `pcb mcp token`), `internal/mcp` (grants, ids, times, the read and write tools, untrusted marking, audit, the remote handler with its Host and Origin guard and limiter, the relay handler), `/mcp` on the listener and on the control socket, the listener-less daemon kind `mcp` and the kind in `serve.lock`, `pcb mcp` (relay, session daemon, the lock race, the sealer guard), `internal/loginflow`, the `login` and `logout` tools with `pcb-ui`'s wizard and the loopback page (`internal/mcp/loginpage`), the `[mcp]` section, the doctor's `mcp tokens` check, and the rehearsal's MCP steps. Deferred: the live session (step 10); a hand-over from a session daemon to `pcb-ui` or the service; the minors in `OPEN.md` ("Parked by M8"). Rulings that change what a user or an agent sees (`.superpowers/sdd/2026-11-13-m8-mcp/progress.md`, kept out of the repository; the plan `spec/plans/2026-11-13-m8-mcp.md` is left as written, except its import line, amended for R18):

| Ruling | Decision |
|---|---|
| R4 | with `[mcp] remote = false`, `/mcp` on the listener is just another CalDAV path: it never answers MCP (no `Mcp-Session-Id`, never 2xx) |
| R8 | the exit rule of `pcb serve` without `--wait-for-login`: an account with either adapter on counts as a served account does (one with CalDAV off and MCP on whose login ends has stopped); one with both off, its loop paused, counts as running |
| R9, R12 | an event pcb cannot read (an unknown TZID, a shape outside Proton's set, unparsable data) is skipped, counted (`skipped`) and logged with its calendar and UID only, never failing a list; `event_get` on it is `must-act`, naming why |
| R10 | an `event_id` holds the occurrence in the series' own form (date, floating wall clock, zoned instant), so the same occurrence has the same id whatever zone a call reads in |
| R11 | a series change that moves the date and would leave an exception, or the start, off the rule kept is refused (`pcb:series-has-exceptions`, `pcb:bad-data`); pcb never rewrites a BYDAY by itself: change the rule and the start together |
| R13 | `free_busy` reports `skipped` with a note that those events' times are unknown, so unreadable events do not read as free; a create or update whose ETag cannot be read back is a success without `etag`, with a note to call `event_get` and not to write again |
| R14 | under a grant that names calendars, an account whose calendars are not known yet (before its first sync) counts as holding none: it is hidden until it is known to hold one (fail closed) |
| R15 | every tool taking an `event_id` hides an invisible account or calendar as missing (`no event X`, the same text as a malformed id), whatever the agenda would answer; one audit line per logical call, a run that only asks the person writes none |
| R16 | the delete confirmation for a whole recurring series names it: `Delete the whole series (every occurrence, first on D): «S»?` (final review: the scope first, the title last and sanitised) |
| R17 | remote sessions are forgotten after 1 h idle (the client initialises again; `TestRemoteMCPSessionEndsWhenIdle`); `Host` may also be the IP the request arrived on; a present `Origin` must equal `Host` exactly, `null` refused; `pcb status` shows the `mcp:` line only when a daemon answers |
| R19 | `pcb mcp token add --allow delete` without `write` is refused at creation (exit 2), as the grant itself would refuse it |
| R20 | `pcb mcp`'s sealer guard runs when the session daemon starts (exit 10 on a new state with `backend = auto` and no Secret Service), before it opens the state, not only in the `login` tool; the relay's own connections are not UID-checked beyond the probe (same-UID trust, a 0700 directory) |
| T9 | an account signed out (`logged-out`) is invisible to agents, as if outside the grant; one that needs a login stays visible with its step; a tool error is a JSON text `{"error": {...}}` |
| T14 | the loopback page binds `127.0.0.1:0` (an ephemeral port); one `login` per 60 s per MCP session; URL elicitation declined or cancelled closes the page and answers `refused`, accepted it answers without the URL; `pcb-ui` raises its wizard with the username filled in and keeps a login already in progress |

Where the built behaviour differs from the design (each marked "Built:" in `spec/designs/2026-11-13-mcp-design.md`): RDATE is not expanded (Proton never stores it); the sealer guard runs at the session daemon's start (R20); signed-out accounts are invisible rather than listed `logged-out` (T9); tool errors are a JSON text (T9); a client with URL-only elicitation deletes on the `delete` grant alone, as one with none; the login steps live in `internal/loginflow`, shared by `pcb-ui`'s wizard and the loopback page, and `internal/mcp` never imports `internal/ui` (R18, R21); remote sessions end after 1 h idle (R17); there are no MCP metrics, as there are none for CalDAV.

## 19. Security model (summary)

Assets: Proton session and key passphrase (account takeover), decrypted
events (privacy), the ability to write (integrity). Adversaries: other local
users, network peers, malicious calendar content, a leaked CalDAV app
password. Controls: sealed secrets (§7.3), no plaintext cache (§7.2),
loopback by default, TLS and authentication on any other bind (§9), per-
client app passwords stored hashed and, since decision 0013, sealed beside the hash, signature verification (§5.3: an
event that fails it is served marked, not dropped), writes only to calendars
whose member passphrase signature verifies with our key, and only over
content of ours that verifies, with pinned key packets (§5.3, its residual
risks listed there), no
secrets in argv or logs, state directory 0700. On a server, the daemon runs
as a dedicated `pcb` user (system service) or in its container [call].
M5 additions:
- The control socket (§15) is 0600 in a 0700 directory and accepts the daemon's UID only (SO_PEERCRED, Linux; the
  client checks the socket and the server too), because logins and the 2FA code cross it. Another local user, and root
  under another UID, cannot use it [verified: `TestOtherUIDIsRefused`, `TestClientRefusesAServerOfAnotherUID`].
- Units: the user unit carries the hardening that needs no user namespace, the system unit the full set (`ProtectSystem=strict`,
  `ProtectHome`, `PrivateTmp`, `NoNewPrivileges`, an empty capability bounding set, a system call filter) and runs as
  `pcb` [verified: `TestUserUnitGolden`, `TestSystemUnitGolden`; `systemd-analyze verify` in `make systemd-smoke`]; the exposure
  score printed by `systemd-analyze security` was 1.4 locally (not asserted).
- Image: distroless static, UID 65532, no shell, one volume `/data` with 0700 directories [verified: `make container-smoke`
  steps 7 and 8]; the key file belongs on another mount (README, "Run in a container") [call].
- Audit hardening (`spec/audits/2026-11-12-initial-version.md`): a corrupt `clients.json` fails closed (503, logged), request bodies and property names are bounded (§9), an IPv4 bind stays IPv4 only, a
  replaced certificate is a warning, login calls have deadlines (§5.2), `synchronous=FULL` and a single-transaction migration keep the cache durable (§7.2), and a pcb run as root refuses another user's state (§7.1).
- Decision 0013: whoever can read the state directory and the key file can recover the CalDAV passwords; the same
  access already exposes the Proton session, which is more sensitive.
- Residual (documented): restoring the whole state directory, `state.json` included, defeats the rollback check (§7.3);
  the daemon's memory holds the decrypted calendar and, for at most 5 minutes, a pending login.
M8 additions (MCP, §12):
- Adversaries added: an agent acting beyond what its person meant (prompt injection through calendar text included), a
  leaked MCP token, a web page in the person's browser aiming at loopback (DNS rebinding).
- **Remote is a boundary**: a bearer token per agent, `pcbm_` and 256 bits, kept as a salted SHA-256 only and shown once;
  its grant (capabilities, accounts, calendars) is enforced by the daemon, scope included (what is outside is answered as
  missing); `login` and `manage` can never be granted to a token; 401 failures are limited per IP; `/mcp` inherits the
  listener's TLS rule, is off by default, and checks `Host` and `Origin` (403) on top of the token [verified:
  `TestTokenAddStoresOnlyAHash`, `TestRemoteMCPNeedsAToken`, `TestRemoteMCPLimiter503AfterFiveFailures`,
  `TestRemoteMCPRefusesForeignHost`, `TestRemoteMCPRefusesForeignOrigin`, `TestRemoteMCPSessionBoundToItsToken`,
  `TestScopedGrantCannotSeeOtherAccounts`, `TestMCPTokenRefusesLoginCap`].
- **stdio is same-UID trust**: any process of the person's UID can run `pcb` or dial the control socket, so a `pcb mcp`
  profile or `--allow` is a guard rail against a careless agent, not a boundary against a hostile process; the socket's
  peer-UID check (§15) stays the boundary, and its `/mcp` route takes the grant the relaying process sends. Agents reach
  no `/v2` route: `pcb mcp` uses only the status probe [verified: `TestControlSocketMCPRefusesOtherUID`, `TestRelayAuthNeedsTheGrantHeader`].
- **The person's switch**: `pcb account mcp off` hides an account from every agent and every token at once [verified:
  `TestMCPOffAccountIsInvisible`].
- **Secrets never pass through the agent**: `login` takes a username only; the password, 2FA code and CAPTCHA are typed by
  the person in `pcb-ui`'s window or on the loopback page (127.0.0.1 only, an ephemeral port, a 128-bit single-use path
  bound to that username, claimed by the first browser that posts, 5 minutes, `no-store` and `Referrer-Policy:
  same-origin`, no field ever rendered with a value, no log). The nonce is not a secret beyond that: it lets one browser type
  credentials for that one username, once. Neither the nonce nor any secret reaches a log or audit line [verified:
  `TestLoginPageSingleUse`, `TestLoginPageRefusesForeignHostAndOrigin`, `TestLoginPageNeverEchoesThePassword`,
  `TestLoginPageNoStore`, `TestNoSecretInAnyMCPOutput`]. The agent alone cannot attach an account (a person must type the
  secrets), so `login` sits in the stdio `read-only` profile; its cost is spam, bounded by one call per 60 s per session.
- **Writes**: an agent edit is an ETag-guarded patch (no lost update, nothing it did not touch dropped), the M4 refusals and
  the write gate apply unchanged, and a delete asks the person when the client can ask (§12). Calendar text reaches the
  agent marked `untrusted`; that is a mitigation, not a control.
- Residual: over stdio the grant does not hold against same-UID code; a client without form elicitation deletes on the
  grant alone; a URL-elicitation client shows the page link in the agent's interface (the nonce is then in the client's
  transcript, still single-use and bound to one username).
Residual risk, stated in the docs: the bridge holds the decrypted calendar
in memory, like Mail Bridge does for mail; whoever can unseal the store
owns the Proton session (and the account, if unattended relogin is on).

## 20. Prior art and credits

To audit and learn from (MIT unless noted), credited in `README.md` when a
piece of logic is reused:
- cheeseandcereal/proton-cal (Unlicense): reverse-engineered API and crypto
  docs, write path, recurrence, login; our fork Barben360/proton-cal adds
  env/TOTP login, two-step CAPTCHA, allowlists, export, and an audit list of
  pitfalls (stale caches, fail-open config, shutdown).
- develonrails/carbonate: sync-collection on modelevents, go-webdav
  compatibility fixes, invitations through the Mail API. M2 takes from it,
  as design notes, the baseline-cursor pitfall and the ID-driven re-read that
  ignores `Action`.
- jmdlab/cal-gateway: Apple client compatibility, scheduling advertising,
  VTIMEZONE generation, fidelity matrix, RSVP endpoint. M2 takes from it the
  baseline-before-listing order and the row-404-means-delete rule.
- WentTheFox/caloxide (from protoxide/ferroxide/hydroxide): unattended
  relogin, CAPTCHA page, shared rate limiter, encrypted cache. M2 takes from
  it the feed's endpoint shapes and the 30 s / backoff cadence; M5 takes its
  standby idea (slow the poll when nobody uses the bridge), rewritten as the
  idle cadence of §8.1 with our own definition of idle.
- The Proton Mail Bridge container model (the author's headless Bridge
  container): M5 takes the usage model only, one volume, login with `docker exec -it`,
  a credential the bridge shows on request (decision 0013); no code.
- emersion/go-webdav (MIT): audited in E17; used as a test client only (M3-R5).
- Thunderbird's CalDAV provider (mozilla/releases-comm-central, MPL-2.0): read for
  understanding only (E08), no code copied.
- Fyne v2.8.2 (fyne.io/fyne/v2, BSD-3-Clause) and godbus/dbus v5.2.4 (BSD-2-Clause): used as dependencies in M7 (the toolkit of `pcb-ui`; the D-Bus
  client of the Secret Service sealer and of the notifications), credited in `README.md`; nothing copied. Our own Secret Service client was written from the
  freedesktop Secret Service specification, with no other implementation consulted.
- ProtonMail/WebClients (GPL-3.0 [verified: GitHub licence 2026-11-10]:
  read for understanding only, never copy code), go-proton-api, gopenpgp,
  go-srp (MIT, used as dependencies); rrule-go (MIT) audited in E16 and not
  adopted (M8 expands recurrences with its own code);
  modelcontextprotocol/go-sdk v1.8.0 (MIT moving to Apache-2.0, both fine for
  an MIT project as a dependency) is the MCP server of M8, audited in E19,
  credited in `README.md`.

## 21. Open questions for the author

See `OPEN.md`.
