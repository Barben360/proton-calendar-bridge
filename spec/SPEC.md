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

**Scope of this spec: a connectable CalDAV interface, nothing more**
[decided 0007]. Other adapters (REST, MCP, `.ics` feeds) are not planned;
the architecture only keeps the door open for them (§4).

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
- **Scriptable CLI** (backsight conventions) [call]: `--json` envelope,
  `--plan`, `--non-interactive`, fixed exit codes (§13).

## 3. Goals, non-goals, anti-goals

Goals: CalDAV read/write for owned calendars; read-only for subscribed and
holidays calendars; login with password, or password + TOTP [decided
0010]; a CLI for everything about the bridge itself (install, service,
login, status, doctor, CalDAV client credentials, uninstall); a container
image; Linux amd64 and arm64. Those two are the supported platforms; macOS and
Windows only build, and the CLI there runs offline (§13, "Platforms").

Not planned (not designed here): REST API, MCP, `.ics` feeds, event
commands in the CLI (Thunderbird is the client), GUIs, updates/signed
releases (while private), shared calendars, invitations, two-password mode,
FIDO2, CardDAV, macOS/Windows service management.

Anti-goals (failure even if it works):
- Plaintext calendar data or reusable credentials on disk unsealed.
- A listener reachable beyond loopback without TLS and authentication.
- Behaviour that gets the Proton account flagged (login storms, CAPTCHA
  loops, unbounded polling).
- Silent lossy round-trips (an edit in Thunderbird that drops alarms or
  exceptions in Proton).

## 4. Architecture

```
                 ┌──────────────── pcb daemon (one per account) ─────────────────┐
 Thunderbird ──▶ │ A1 CalDAV ──▶ C3 service (use cases, access checks)            │
                 │                    │                                           │
 pcb CLI ──────▶ │ control API ──────▶│   C4 sync engine                          │
 (unix socket)   │ (internal)         │        │                                  │
                 │               C2 store (cache, state, sealed secrets)          │
                 │                    │                                           │
                 │               C1 proton (transport, auth, keys, calendar API,  │
                 │                    change feed, codecs: model ⇄ Proton)        │
                 └───────────────────────────────────────────────────────────────┘
 Later adapters (REST, MCP, .ics) would plug into C3 like A1; not planned.
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
| C5 access | CalDAV client credentials (app passwords), read-only enforcement for subscribed/holidays calendars; per-client grants designed but not built | `access.Check(client, op, calendar)` | C2 |
| C6 daemon | Process lifecycle, listeners, config, logs, metrics, health | `pcb serve` | all |
| A1 | CalDAV adapter (§9) | CalDAV | C3 only |
| CTL | Control API for the CLI: JSON over HTTP on a unix socket, internal, versioned, not a public API | `pcb` subcommands | C3, C6 |

Rule: adapters never import C1, C2 or C4; they only call `service.API`. A
second account is a second `service` instance in the same daemon [call;
multi-account in v1 data model, one account tested in v1].

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
- Refresh request: `POST /auth/v4/refresh` with `UID`, `RefreshToken`,
  `ResponseType: token`, `GrantType: refresh_token`, `RedirectURI`, a random
  `State` and the `AccessToken` of the pair being refreshed. Proton refuses a
  refresh token issued by a refresh without that access token (400, code
  10013); only the first refresh after a login works without it
  [verified: live 2026-11-12, E01]. pcb (`auth.Refresh`) and pcb-lab always
  send it, and the shared fake enforces the rule.
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
dropped [verified: `TestObjectReads/TestUnverifiedSignatureIsMarked`,
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
calendar part, 403 `pcb:unsupported-change`, M4-R13), a row whose UID
column holds an escaped form (`a\,b`, another client's data: Proton itself
stores the unescaped value, below) of the UID its body carries: such an
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

UID escaping: Proton stores a UID by its TEXT value, unescaped (RFC 5545
§3.3.11: `\\`, `\;`, `\,`, `\n`): a create whose parts carry
`UID:pcb-m4-a\,b` is the row `pcb-m4-a,b` [verified: live 2026-11-13,
`HUMAN-CHECKS.md` W10]. pcb identifies an event by that value everywhere:
`ical.ParseWrite` unescapes the body's UID (its VEVENTs keep the line as
written, so the parts and what pcb serves carry `UID:pcb-m4-a\,b`), and the
`?UID=` lookup, the read-back, the planner's row check, the conflict checks
and the cache use the value; `ical.ProvesKey` compares the parts' UID by its
value with the column [verified: `TestAnEscapedUIDIsCreatedConfirmedAndEdited`,
`TestAnEscapedUIDIsCreatedAndEditedAtItsName`]. Such an event is served at the
client's name when it was created there, else at the canonical name of the
value (`~` + base64url of `pcb-m4-a,b`, §9): the name an event synced from
Proton gets, so an href does not depend on which client wrote the event. A
placeholder (an event that does not decrypt) serves the column escaped
again (`ical.EscapeText`), as the parts would. Only `\,` was measured live;
`\;`, `\\` and `\n` are taken to follow the same RFC rule. A body's UID
holding an escape RFC 5545 does not define (`\x`, a trailing backslash) or a
control character in its value (a newline included) is refused, 403
`bad-data` with the reason: what Proton stores for it is not known, and a
guess would leave the write unconfirmed; no client pcb targets writes one
[verified: `TestParseWriteRefusesAUIDItCannotName`]. A row whose column
holds an escaped form (`pcb-c\,d`, which Proton never writes; another
client's data could) is never rewritten through pcb: any edit at its name,
even one whose body repeats the column's exact text, is 503
`ErrUIDStoredOtherwise`, because pcb names the event by the body's value
(`pcb-c,d`), which is not the column, so neither the `?UID=` read-back nor
the key proof would find it; a DELETE is unaffected.

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
  (STATUS), 2001 (signature or exception SEQUENCE, not told apart, M4-R24;
  also an exception of a master without SEQUENCE, so every master pcb writes
  carries one, `spec/mapping.md` §6 [verified: live 2026-11-13,
  `HUMAN-CHECKS.md` 6.3 D7]),
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
  list means the calendar's default notifications; email notifications are
  Proton-side, not served and kept by an update: `spec/mapping.md` §5
  [verified: live 2026-11-13, `HUMAN-CHECKS.md` 6.3 D4]), attendee `Status` to `PARTSTAT`
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
- Recurrence: no expansion in M3 (no `calendar-query`, no free-busy); library choice
  after the audit E16 when time-range queries or free-busy are needed [call].
- Fuzz tests on every parser (`internal/ical`, `internal/caldav` request bodies)
  [verified: `FuzzReportBody`, `FuzzPropfindBody`]; golden files from real Proton,
  Thunderbird and Apple exports are not yet collected.

## 7. C2 — Store block

### 7.1 Layout
`$XDG_STATE_HOME/pcb/` (`/var/lib/pcb/` for a system service, `/data/state` in
the container): `pcb.db` (SQLite, WAL), `secrets.sealed`, `state.json`,
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
  login); 2. user-scope `systemd-creds`; 3. key file 0600.
- Server system service: 1. system-scope `systemd-creds` (host key); 2. key
  file 0600 on a separate path, `doctor` warns.
- Container: 1. compose secret file at `/run/secrets/<name>`; 2. key file 0600
  on a volume separate from the data.
Built in M5: the key file only (`[secrets] key_file`, default `<config>/secrets.key`, exactly 32 random bytes,
refused when group or other can read it); Secret Service and `systemd-creds` are not built, and in a container the
compose secret is just the path `key_file` points at (README, "Run in a container"). `doctor` reports the backend chosen. Host-key `systemd-creds` is no stronger than
a key file against root or a same-user process; only TPM2 binding, or a keyring encrypted
under a strong login password (untested), helps against offline theft. [verified: E15 for user-scope systemd-creds, Secret Service presence
(interactive session), key file; TPM2 and desktop system scope untested]
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
`POST /v1/logout` takes an optional body `{"discard": bool}`; an empty body is the zero request, so an older pcb
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
  limiter. A request that gets no answer (its timeout, a reset connection) closes the client's idle
  connections, then and before its next request, so a retry or the next pass dials anew: an HTTP/2
  connection that stopped answering otherwise stays in the pool and takes every later request, as live
  after a login (`[verified: live 2026-11-13]`, HUMAN-CHECKS 7.1.5). The refresh does the same on the
  login manager's connections. Both transports also run the HTTP/2 health check (a PING after 15 s
  without a frame, the connection closed when it is not answered within 15 s). Tuning after E06 stays open; the daemon's idle slow-down is below.
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
     another name, or the name serving another live UID → 409 (UIDs by their
     unescaped value, §5.3; a row whose column holds an escaped form of the
     body's UID is never a 409 against itself at its own name, it is
     503 `ErrUIDStoredOtherwise`); `If-Match` (`*`, or a comma-separated
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
unsupported VALARMs dropped (`ACTION:EMAIL` among them: email
notifications are Proton-side, `spec/mapping.md` §5), the client's `COLOR` ignored, `DURATION` made
`DTEND`, Mozilla TZIDs mapped, two RRULE forms rewritten (M4-R5, M4-R12),
missing `DTSTAMP`/`DTEND` defaulted (M4-R8), attendee parameter edits
ignored (M4-R9). Refused (each with its code in `spec/mapping.md`):
- "this and following" splits (`RANGE=THISANDFUTURE`), RDATE, EXRULE;
- an RRULE outside Proton's supported set (`spec/mapping.md` §4; whether
  Proton alters the accepted ones [assumed → E12,
  `spec/experiments/E12-recurrence-forms.md`]);
- a write that introduces a master DTSTART not an occurrence of its RRULE
  (by the rule's BY parts, on DTSTART's day: its TZID's zone, a UTC time's
  UTC day): a create, or an update changing the stored DTSTART or RRULE;
  Proton drops that start while Thunderbird shows it [verified: live
  2026-11-13, `HUMAN-CHECKS.md` W8], so `pcb:unsupported-recurrence`,
  `DTSTART is not an occurrence of the RRULE`, before any write; an update
  keeping both passes, and reads are not checked (`spec/mapping.md` §4)
  [verified: `TestRRuleStartMustBeAnOccurrence`,
  `TestADTSTARTOffItsRuleIsRefusedBeforeProton`,
  `TestAStoredDTSTARTOffItsRuleIsEditedUnlessTheWriteMovesIt`];
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
- Auth (M3-R17 to R21, amended by decision 0013): HTTP Basic, any username; the password is an
  app password (24 random bytes, base64url) from `pcb client add NAME`. It is kept in
  `<state>/clients.json` (0600, format version 2) twice: as a salted SHA-256, compared in
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
  `WWW-Authenticate: Basic realm="pcb", charset="UTF-8"`; 5 failures per minute per IP
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
(`/ctl/v1/…`), used only by `pcb`. It is documented in code, not in
OpenAPI, and may change between versions.

## 12. A4 — MCP adapter

Not planned [decided 0007]. Notes kept for whoever designs it later:
calendar content is a prompt-injection channel (mark external text as
untrusted, flag outside invitations), grants per agent with none by
default, agents never on the control socket, audit log of every call.

## 13. A5 — CLI and ops

Conventions (as backsight) [call]: every command has `--json` except `serve` and `service logs`, which refuse it (exit 2) (one
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
pcb login [--username U] [--password-fd N] [--unattended-relogin]   # M5: through the running daemon when there is one
pcb login resume (--captcha-token T | --totp)   # --totp takes no value: the 2FA code of the login the daemon holds comes from PCB_TOTP or a prompt, never argv (M5-P11)
pcb logout [--yes]                          # revokes the session; --yes discards a store pcb cannot read, without the confirmation (§7.3)
pcb status [--daemon]                       # login state; with a daemon: its live state, listener, per-calendar sync and write gate, render counters, alerts (§15); --daemon exits 20 when none answers
pcb calendar list                           # what Thunderbird will see; read_only is the write gate (M4-R1: false only for a normal calendar with the Write bit and a verified passphrase signature)
pcb sync [--full] [--watch [--interval 30s]] # M2: fill/refresh the event cache (--watch not with --json)
pcb event list --calendar ID|NAME [--from D] [--to D]   # M2: cached events, no network call
pcb client add NAME|list|remove NAME|ID      # CalDAV credentials (M3: app passwords; add works logged out; `clients.json` survives `pcb logout`, M3-R17)
pcb client show [NAME|ID] [--reveal]         # M5, decision 0013: URL, username (the client's name), password and, over TLS, the certificate fingerprint; without NAME: `default`, else the only client
pcb client rotate NAME|ID                    # M5, decision 0013: a new password, the old one stops working at once
pcb config get|set|validate                  # not built (M5 out of scope: edit config.toml, run pcb doctor)
pcb version
```
One account per daemon in v1 [call]; the data model keeps an account ID so a
second one is an addition, not a migration.
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
- `pcb serve` listens on `<state>/pcb.sock` (§7.1). The API is HTTP/1.1 under `/v1/`, internal and versioned:
  `GET /v1/status`, `POST /v1/login`, `POST /v1/login/totp`, `POST /v1/logout` (an optional body `{"discard": true}` is the confirmed discard of a store pcb cannot read, §7.3; an empty body is the zero request). Each answer is one `output.Envelope`:
  200 ok, 422 a command error (its exit code, message and hint), 400, 403, 404, 405 or 413 a transport error. Request bodies
  are at most 64 KiB and never logged (the log line holds method, path, status and exit). Client timeouts: 2 s to
  dial, 10 s for status, 180 s for login. Secrets travel only in request bodies.
- Peer check: on Linux the peer UID (`SO_PEERCRED`) must equal the daemon's effective UID, root included (root uses its
  own state directory or `sudo -u`); other Unix rely on the 0600 mode and the 0700 directory; off Unix there is no CTL
  and the CLI is always offline. Before dialling, the client checks that the socket file is a socket (not a symlink)
  owned by the caller's UID (M5-R2); on Linux it also checks the connected server's UID before sending a login, on
  every redial. A refusal exits 20 [verified: `TestOtherUIDIsRefused`, `TestDefaultPeerUID`,
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
- `pcb status` keeps `service.Status`'s fields flat at the top level of its JSON (M5-P4) and adds `daemon` (null
  offline), `health` (serve state, cadence `interval_seconds`, `idle`, `last_pass`, per-calendar sync and write gate
  with its reason, render counters, human verification), `listener` (URL, TLS, fingerprint, certificate end) and `alerts`.
  Text lines: first the login's own `key: value` lines (`phase:`, `secrets:`, `key_file:`, `unattended:`, `login_expires:`), then `daemon: serving since <t> (pid, version)` or `waiting for a login since <t>`
  (offline: `daemon: not running (offline view)`), `listener:`, one `calendar:` line
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
- Alerts: `{code, severity: error|warning, message, fix, calendar?}`, errors first, then code, then calendar ID. Codes
  (stable): `needs-login` (error), `needs-captcha` (error, login), `human-verification` (error, API 9001, separate
  from the CAPTCHA because the fixes differ, M5-P7), `sync-stalled` (warning, per calendar: now minus the later of the last success and
  Serve's start is more than 5 intervals), `calendar-keys-failed` (error), `write-gate-undecided` (warning, names the calendar
  and why: closes the M4-R39 note), `clients-file-unreadable` (error: `clients.json` exists but cannot be loaded, so every CalDAV request answers 503; also in `pcb status` without a daemon and as the failed `caldav clients` check of `pcb doctor`, fix `fix or restore <path> (pcb client list shows the error)`), `tls-certificate-expiring` (warning, 30 days) and `tls-certificate-expired` (error)
  [verified: `TestAlerts`, `TestAlertsAreOrdered`, `TestStatusNamesAClientsFileThatCannotBeLoaded`].
- Prometheus endpoint: deferred (M5-Rpre11); status over the socket covers sync lag, Proton error classes and login state.

## 16. Testing

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
| E16 | RRULE library audit vs our own | C0 | read, test against RFC 5545 examples |
| E17 | go-webdav audit: fit, bugs, client compatibility | A1 | done (M3): not adopted as server, test client only, `E17-go-webdav-audit.md` |
| E18 | (dropped: no REST API planned) | – | – |

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

Nothing after M5 is planned [decided 0007].

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
- ProtonMail/WebClients (GPL-3.0 [verified: GitHub licence 2026-11-10]:
  read for understanding only, never copy code), go-proton-api, gopenpgp,
  go-srp, rrule-go (MIT, candidate dependencies),
  modelcontextprotocol/go-sdk (MIT moving to Apache-2.0, both fine for an
  MIT project as a dependency).

## 21. Open questions for the author

See `OPEN.md`.
