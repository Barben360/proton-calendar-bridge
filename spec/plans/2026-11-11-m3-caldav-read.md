# M3 CalDAV Read Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `pcb serve` runs the sync loop and a CalDAV server; Thunderbird (local over HTTP, or over the network with TLS) discovers every calendar, shows its events decrypted, and stays in sync through `sync-collection`; CalDAV clients authenticate with per-client app passwords from `pcb client add`. Proven offline (fake Proton with real encrypted cards, a CalDAV contract suite from E08, go-webdav's client as a second independent client); the live Thunderbird session is a deferred human check.

**Architecture:** C1 gains `internal/proton/cards` (decrypt and verify a row's card parts with the calendar and address keyrings). C0 is `internal/ical` (RFC 5545 content lines: unfold, parse, fold, serialize; the Proton part merge; the per-UID VCALENDAR assembly) — an iCalendar-native model (decision 0012, below). C2's cache gains per-calendar change reads. C5 is `internal/access` (hashed app passwords). C3 gains a read API for adapters (`service.Calendars` resources, per-UID objects, changes since a token) with an in-memory LRU of rendered objects; keys stay unlocked in memory for the process lifetime. A1 is `internal/caldav`: our own `net/http` handler and `encoding/xml` types (no go-webdav at runtime, E17). `pcb serve` (foreground; the service install is M5) holds the sync lock, runs the watch loop and the listener.

**Tech Stack:** Go 1.27, gopenpgp v2 (as M1), `net/http`, `encoding/xml`, `crypto/tls` + `crypto/x509` (self-signed ECDSA P-256), the M2 cache; tests only: `github.com/emersion/go-webdav v0.7.0` (MIT) client.

**Spec:** `spec/SPEC.md` §6, §7.2, §9, §13, §16, §19; `spec/experiments/E08-thunderbird-caldav.md` (MUST/SHOULD/AVOID list = the contract); research `spec/experiments/raw/m3-research.md` (Task 1 adds it); decisions 0006 (superseded for v1 by 0012), 0007.

**Out of scope (YAGNI, say so in review):** writes (PUT/DELETE answer 403 with a precondition until M4); `calendar-query` and `free-busy-query` (Thunderbird uses neither, E08; they need recurrence expansion, E16, not planned while no client needs it — not advertised in `supported-report-set`); VTIMEZONE (Thunderbird uses its own tzdata, E08/E11); scheduling (never advertised); JSCalendar (0012); MKCALENDAR/PROPPATCH (405); signature verification of parts authored by other users (counted as unverified; shared calendars are not planned); the daemon/unit install (M5); a decrypted index on disk (never, §7.2).

## Global Constraints

- Everything in the M1 and M2 plans' Global Constraints still holds (envelope, exit codes, secrets never in argv/logs/errors/responses, `internal/cli` and adapters call only `service.API`, tests bite, live only on the test account and only as deferred steps, commit style, SSH push for workflows).
- Adapters never import C1, C2 or C4 (§4): `internal/caldav` imports `service` (and its own packages) only — enforced by an architecture test like `internal/cli/arch_test.go`.
- Nothing decrypted is written to disk (§7.2); rendered objects live in memory only (LRU, default 2 000 objects).
- Decrypt with `GetBinary`, never `GetString` (CRLF and UTF-8 survive) [source: gopenpgp crypto/message.go:204-206].
- Merge rules (proton-cal, live-verified [claim]): structural single-valued properties (UID, DTSTAMP, DTSTART, DTEND, DURATION, RRULE, RECURRENCE-ID, SEQUENCE) from the shared signed part win; other single-valued: first seen in part order shared-signed, shared-encrypted, calendar-signed, calendar-encrypted; multi-valued (EXDATE, RDATE, ATTENDEE, CATEGORIES, COMMENT, …): union without exact-duplicate lines; nested components kept verbatim; `X-PM-SESSION-KEY` dropped; any other property, `X-PM-*` and unknown ones included, kept verbatim.
- Row-level data rendered into the VEVENT: `Color` → `COLOR`; `Notifications` → one VALARM each (`null` = the calendar's default part-day or full-day notifications, `[]` = none; Type 0 → `ACTION:EMAIL`, else `DISPLAY` with a `DESCRIPTION`); `Attendees[].Status` joined to attendee lines by `X-PM-TOKEN` → `PARTSTAT` (0 NEEDS-ACTION, 1 TENTATIVE, 2 DECLINED, 3 ACCEPTED).
- One CalDAV object per UID: master first, then exceptions by RecurrenceID, then row ID; an exception never carries RRULE/EXDATE; orphan exceptions served as they are; duplicate masters: the most recently edited one is the master, the others are left out and counted (`pcb doctor`) [call, ruling M3-R3].
- VCALENDAR: `VERSION:2.0`, `PRODID:-//pcb//proton-calendar-bridge//EN`, `CALSCALE:GREGORIAN`, no VTIMEZONE, CRLF line ends including the last line, lines folded at 75 octets.
- Object ETag = `"v1-" + hex32(sha256(render version ‖ sorted (row ID, row ETag) ‖ the calendar settings the render used))`; CTag = `<epoch>:<calendar max seq>:<settings hash>`; sync-token = `urn:pcb:sync:1:<epoch>:<seq>`; a token of another epoch, a seq above the current one or below the oldest retained tombstone → 403 with `<D:error xmlns:D="DAV:"><D:valid-sync-token/></D:error>` (never an empty `<D:error/>` with 403, E08).
- Deltas and the new token are read in one SQLite read transaction; a UID with no live row is a response-level 404; deleting an exception row is a change of its UID's object, not a 404.
- CalDAV surface: OPTIONS on any path 200 with `DAV: 1, 3, calendar-access` (never `calendar-schedule`/`calendar-auto-schedule`); `/.well-known/caldav` → 308 to the principal (after auth: never 403 there, E08); PROPFIND Depth 0/1 (infinity → 403 `propfind-finite-depth`); REPORT `sync-collection` and `calendar-multiget`; GET/HEAD objects; PUT/DELETE → 403 `<C:no-write>`-style precondition (`DAV:need-privileges`) until M4; PROPPATCH/MKCALENDAR → 405. `supported-report-set` lists exactly sync-collection and calendar-multiget.
- Privileges (ruling M3-R1): owned normal calendars advertise read + write already in M3 (writes refused until M4); subscribed, holidays and other read-only calendars advertise read only. The live Thunderbird session runs after M4, so no subscriber sees an M3-only server.
- Auth: HTTP Basic; any username (Thunderbird may send the account email); password = an app password from `pcb client add <name>` (24 random bytes, base64url, printed once); stored as `sha256(salt ‖ password)` per client in `<state>/clients.json` (0600, atomic writes), compared in constant time; 401 with `WWW-Authenticate: Basic realm="pcb", charset="UTF-8"`, never 403 for missing or wrong credentials; failed attempts limited per remote IP (5 per minute, then 429 with Retry-After).
- Listener (`[caldav]` in config.toml, strict): `listen` default `127.0.0.1:1080`; loopback → HTTP allowed; any other address (checked on the resolved bind address) → TLS required: `tls_cert`/`tls_key` (user files) or, when both are empty, a persistent generated self-signed ECDSA P-256 certificate in `<state>/tls/` (key 0600, SANs = `hostnames` + bind IPs, 825 days), its SHA-256 fingerprint printed by `pcb serve` and `pcb doctor`; TLS ≥ 1.2; request bodies ≤ 8 MiB; `ReadHeaderTimeout` 10 s; no error text, Proton text or IDs of other users in response bodies.
- One undecryptable or unparsable object never fails a multiget or a sync-collection (ruling M3-R4): it is served as a minimal placeholder VEVENT (UID; DTSTART/DTEND from the plaintext row; `SUMMARY:(pcb could not decrypt this event)`; `X-PCB-PLACEHOLDER:1`) with its normal ETag, and counted in `pcb status`/`pcb doctor`. M4 must refuse any write to a placeholder object (412 or 403), so an edit in Thunderbird can never overwrite the real event.

## Review Focus

1. **A Thunderbird with a stored sync-token after the cache was recreated (logout/login, epoch change)** → 403 `valid-sync-token`, then a full sync with the empty token lists every object; nothing duplicated, nothing stale; pinned by `TestSyncTokenOfAnotherEpochIsRefused` (Task 6).
2. **An event deleted in Proton** → the next sync-collection reports its UID's href as a response-level 404 exactly once; an exception deleted → its UID's object is a 200 change; pinned by `TestSyncCollectionReportsDeletionsPerUID` (Task 6).
3. **One event Proton serves with a part that does not decrypt** → every other event of the calendar still syncs; that event shows as the placeholder and is counted; pinned by `TestUndecryptableEventDoesNotFailTheReport` (Task 6).
4. **`pcb serve --listen 0.0.0.0:1080` without TLS config** → a generated certificate is used (never plain HTTP on a non-loopback address), its fingerprint printed; `listen = "[::]:1080"` likewise; pinned by `TestNonLoopbackBindNeverServesPlainHTTP` (Task 7).
5. **A wrong or missing app password, or a revoked client** → 401 with the Basic challenge, no body detail; the 6th failure within a minute from one IP → 429; a correct password after revocation → 401; pinned by `TestAuthFailuresAreChallengedAndLimited` (Task 5).

---

### Task 1: Fake Proton with real encrypted cards; C1 `internal/proton/cards`

**Files:**
- Create: `internal/proton/cards/cards.go`, `internal/proton/cards/verify.go`, `spec/experiments/raw/m3-research.md` (the controller's research, paths replaced by project names as in m2-feed-research.md), `spec/decisions/0012-icalendar-native-model.md`
- Modify: `internal/proton/protontest/events.go` (replace placeholder parts by real ones), `spec/decisions/INDEX.md`, `spec/decisions/0006-jscalendar-internal-model.md` (status line: superseded for v1 by 0012)
- Test: `internal/proton/cards/cards_test.go`, `internal/proton/protontest/events_test.go`

**Interfaces:**
- Consumes: M1 `calendar.Access{KR *crypto.KeyRing, …}`, `keys.Unlocked` (address keyrings).
- Produces:
  - `cards.Part{Kind cards.Kind; Text []byte; Signed, Verified bool; Author string}`; `cards.Kind` = `SharedSigned`, `SharedEncrypted`, `CalendarSigned`, `CalendarEncrypted`, `Attendees`, `Personal`.
  - `cards.Open(raw json.RawMessage, cal *crypto.KeyRing, addrs map[string]*crypto.KeyRing, own func(email string) *crypto.KeyRing) (cards.Event, error)`; `cards.Event{Parts []cards.Part; Notifications json.RawMessage; Color string; Attendees []cards.AttendeeStatus; RecurrenceID *int64}`: decrypts with the calendar key's session keys (`SharedKeyPacket`, `CalendarKeyPacket`), the address keys when `AddressKeyPacket` is present, `GetBinary` only; verifies a part's detached signature with `own(Author)` when Author is one of our addresses (`Verified`), else `Verified=false`; a part that fails to decrypt → error naming the part kind (no content).
  - Fake: `protontest.Event` gains `Card string` (the VEVENT body lines to split into parts per the merge table) and the fake builds real parts: shared signed (cleartext + signature by the owner's address key), shared encrypted (session key wrapped to the calendar key), calendar signed/encrypted, attendees; optional `AddressKeyPacket` mode; `BrokenPart` to serve one undecryptable part.
- [ ] **Step 1: Failing tests**: `TestOpenDecryptsEveryPart` (fake event with SUMMARY/DESCRIPTION/LOCATION/EXDATE/ATTENDEE → each part's Text contains its lines byte-exact, CRLF kept), `TestOpenVerifiesOwnSignatures` (tampered cleartext → `Verified=false`, no error), `TestOpenAddressKeyPacket`, `TestOpenBrokenPartNamesThePart` (error text contains the kind, not the ciphertext), `TestOpenKeepsUTF8AndCRLF` (é, emoji, `\r\n`).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `make lint test` → PASS.
- [ ] **Step 5: Benchmark** `BenchmarkOpen` (1 000 synthetic rows; record ns/op and allocs in E09 notes for Task 8) — this is E09's offline half.
- [ ] **Step 6: Decision 0012** (iCalendar-native model for v1: CalDAV is the only adapter (0007); a JSCalendar round trip would be lossy against E08's "store iCalendar verbatim, X-MOZ-* included"; JSCalendar returns with the first non-iCalendar adapter) and the research copy.
- [ ] **Step 7: Commit** `✨ Decrypt Proton event cards`.

### Task 2: C0 `internal/ical` — content lines, Proton merge, per-UID object

**Files:**
- Create: `internal/ical/lines.go`, `internal/ical/merge.go`, `internal/ical/object.go`
- Test: `internal/ical/lines_test.go`, `internal/ical/merge_test.go`, `internal/ical/object_test.go`, `internal/ical/fuzz_test.go`

**Interfaces:**
- Produces:
  - `ical.Parse(b []byte) ([]ical.Component, error)` / `(ical.Component).Bytes() []byte`: RFC 5545 unfolding/folding (75 octets, never splitting a UTF-8 sequence), properties kept as raw lines (name, params, value) so unknown content round-trips byte-exact after unfold/fold.
  - `ical.MergeEvent(parts [][]byte) (ical.Component, error)`: the merge rules of the Global Constraints, input in part order.
  - `ical.Alarms(notifications json.RawMessage, defaults json.RawMessage, allDay bool) ([]ical.Component, error)`; `ical.ApplyRow(ev *ical.Component, color string, statuses map[string]int)`.
  - `ical.Object(master *ical.Component, exceptions []ical.Component) []byte`: the VCALENDAR of the Global Constraints (exceptions sorted by RECURRENCE-ID; RRULE/EXDATE stripped from exceptions).
- [ ] **Step 1: Failing tests**: `TestFoldUnfoldRoundTrip` (long UTF-8 lines, folded at ≤ 75 octets, never inside a rune), `TestUnknownPropertiesSurvive` (X-MOZ-*, X-PM-* except X-PM-SESSION-KEY, params with quotes and commas), `TestMergeStructuralFromSharedSigned`, `TestMergeMultiValuedUnion` (EXDATE in both signed parts → one set), `TestMergeDropsSessionKey`, `TestAlarmsTriState` (null → defaults, [] → none, array → custom; EMAIL vs DISPLAY+DESCRIPTION; all-day defaults), `TestPartstatJoin`, `TestObjectOrderAndStripping`, `TestObjectEndsWithCRLF`; `FuzzParse` (no panic; Parse(Bytes(Parse(x))) stable).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `make lint test` (+ `go test -fuzz=FuzzParse -fuzztime=30s ./internal/ical/` once, recorded) → PASS.
- [ ] **Step 5: Commit** `✨ Merge Proton parts into iCalendar objects`.

### Task 3: C2 change reads for CalDAV

**Files:**
- Modify: `internal/store/cache/cache.go` (M2 shipped `tombstones.uid`, the `(calendar_id, seq)` indexes, `meta.epoch`, `Epoch(ctx)` and `Changes(ctx, calID, since)`: build on them; schema changes only if a query below needs one, with a migration of existing files like M2's in-place column adds)
- Test: `internal/store/cache/changes_test.go`

**Interfaces:**
- Produces (read on the writer's handle: `pcb serve` is the process that syncs, so it opens the cache with `Open` and reads in WAL read transactions; never `OpenReadOnly`'s immutable snapshot, which may be inconsistent during a checkpoint — M2 note in SPEC §7.2):
  - `(*Cache).Snapshot(ctx, calID string, fn func(cache.View) error) error`: runs `fn` inside one read transaction; `cache.View` offers `Epoch() string`, `MaxSeq() int64` (calendar's max over rows and tombstones), `MinTombstoneSeq() int64`, `Groups() ([]cache.Group, error)` (all UIDs with their rows: `Group{UID string; Rows []cache.StoredRow}`, `StoredRow` = Event fields + Raw), `Group(uid string) (cache.Group, bool, error)`, `ChangedSince(seq int64) (changed []cache.Group, deletedUIDs []string, err error)` (UIDs with a row seq > s, and UIDs with a tombstone seq > s and no live row).
- [ ] **Step 1: Failing tests**: `TestSnapshotIsConsistent` (a writer commits during fn: fn sees one state), `TestChangedSinceGroupsByUID` (exception change → its UID in changed), `TestDeletedUIDOnlyWhenNoLiveRow`, `TestMaxSeqPerCalendar`, `TestMinTombstoneSeq`.
- [ ] **Step 2–4**: FAIL → implement → `make lint test` PASS. **Step 5: Commit** `✨ Read per-UID changes from the cache`.

### Task 4: C3 read API for adapters, render pipeline and LRU

**Files:**
- Create: `internal/service/objects.go`, `internal/service/render.go`, `internal/service/lru.go`
- Modify: `internal/service/api.go`
- Test: `internal/service/objects_test.go`, `internal/service/render_test.go`

**Interfaces:**
- Produces (`service.API` additions, socket-ready JSON structs):
  - `Collections(ctx) ([]Collection, error)`: `Collection{ID, Name, Color, Description string; ReadOnly bool; CTag, SyncToken string}` from the cache (no network; never synced → MustAct).
  - `Objects(ctx, calID string) ([]ObjectInfo, string, error)`: every object `{UID, Href string; ETag string}` and the current sync token, from one snapshot.
  - `ObjectsByUID(ctx, calID string, uids []string) ([]Object, error)`: `Object{UID, ETag string; Data []byte; Missing bool}`.
  - `Changes(ctx, calID, token string) (ChangeSet, error)`: `ChangeSet{Changed []ObjectInfo; Deleted []string (UIDs); Token string}`; an invalid token → `*exitcode.Error` with a typed cause `service.ErrInvalidSyncToken` (errors.Is).
  - Serve mode: `(*Local).Serve(ctx, o ServeOptions) error` (used by Task 7) holds the sync lock, opens the cache once with `Open`, runs the M2 watch loop on it and serves the read methods above from the same handle; outside serve mode the read methods open the cache read-only per call (CLI use) and say so in their doc.
  - Keys: address keys and each calendar's `calendar.Access` are unlocked at the first read and kept for the process (bootstrap is a Proton call, the only network use of a read); a relogin or logout drops them; a calendar whose keys fail renders every object as the placeholder (ruling M3-R4) and `pcb status` says so.
  - Render: decrypt (Task 1) → merge (Task 2) → object; keys unlocked once per `Local` (address keys + per-calendar `calendar.Access`), dropped on logout/relogin/Close; LRU of rendered objects keyed by object ETag (default 2 000); an undecryptable row → the placeholder VEVENT (ruling M3-R4) and a counter `service.Status.Sync.Undecryptable`.
- [ ] **Step 1: Failing tests**: `TestObjectsRenderDecryptedEvents` (fake with real cards → Data contains SUMMARY in clear and every unknown property), `TestObjectETagChangesWithAnyRow` (exception edit → master object ETag changes), `TestObjectETagChangesWithDefaultNotifications`, `TestChangesSinceToken`, `TestInvalidTokenIsTyped`, `TestUndecryptableRowIsAPlaceholder`, `TestLRUServesWithoutDecrypting` (second read: zero decrypt calls), `TestKeysDroppedOnLogout`, `TestDuplicateMastersMostRecentWins`.
- [ ] **Step 2–4**: FAIL → implement → `make lint test` PASS. **Step 5: Commit** `✨ Serve decrypted calendar objects to adapters`.

### Task 5: C5 `internal/access` and `pcb client add|list|remove`

**Files:**
- Create: `internal/access/clients.go`, `internal/cli/client.go`, `internal/service/clients.go`
- Modify: `internal/service/api.go`, `internal/cli/root.go`, clitest contract list, `README.md`
- Test: `internal/access/clients_test.go`, `internal/cli/client_test.go`, `internal/service/clients_test.go`

**Interfaces:**
- Produces: `access.Store` over `<state>/clients.json` (`Add(name) (id, password string, err)`, `List() []access.Client{ID, Name string; Created, LastUsed time.Time}`, `Remove(idOrName)`, `Check(password) (access.Client, bool)` constant-time over all clients, `Touch(id)` throttled to once a minute); `access.Limiter` (5 failures per IP per minute → blocked until the minute ends). Service: `AddClient(ctx, name) (ClientSecret, error)` (password returned once), `Clients`, `RemoveClient`, `CheckClient(ctx, password, remote string) (ok bool, retryAfter time.Duration)`. CLI: `pcb client add NAME` (prints the password once, with how to enter it in Thunderbird), `pcb client list`, `pcb client remove NAME|ID`.
- [ ] **Step 1: Failing tests**: `TestAddedPasswordChecksAndIsNeverStored` (file holds no password, mode 0600), `TestRemovedClientIsRefused`, `TestAuthFailuresAreChallengedAndLimited` (service level; the HTTP part is pinned again in Task 6), `TestClientNamesUnique`, CLI envelope and usage-contract tests.
- [ ] **Step 2–4**: FAIL → implement → `make lint test` PASS. **Step 5: Commit** `✨ Add CalDAV client app passwords`.

### Task 6: A1 `internal/caldav` server

**Files:**
- Create: `internal/caldav/server.go`, `internal/caldav/xml.go`, `internal/caldav/propfind.go`, `internal/caldav/report.go`, `internal/caldav/auth.go`, `internal/caldav/arch_test.go`
- Modify: `go.mod` (test-only use of `github.com/emersion/go-webdav v0.7.0`)
- Test: `internal/caldav/contract_test.go` (one test per E08 MUST/AVOID item, named after it), `internal/caldav/report_test.go`, `internal/caldav/webdav_client_test.go`, `internal/caldav/fuzz_test.go`

**Interfaces:**
- Consumes: Task 4 and Task 5 service methods only.
- Produces: `caldav.Handler(api service.API, o caldav.Options) http.Handler`; routes `/`, `/.well-known/caldav`, `/dav/principals/me/`, `/dav/calendars/me/`, `/dav/calendars/me/<calseg>/`, `…/<name>.ics` (calseg: first 16 hex of sha256(calendar ID); name: the UID when it matches `[A-Za-z0-9._@-]{1,200}`, else `~` + base64url(UID)); the surface, auth and error rules of the Global Constraints.
- [ ] **Step 1: Failing tests**: the E08 contract table (discovery chain from `/` and from `/.well-known/caldav`, OPTIONS 2xx with exactly the DAV classes, 401 challenge never 403, PROPFIND props for principal/home/collection/object, supported-report-set exact, privileges per R1, multiget with encoded hrefs and a missing href → response-level 404, sync-collection empty token = full listing, delta, deletion 404 per UID, 403 valid-sync-token on a bad token/epoch, PUT/DELETE 403, PROPPATCH/MKCALENDAR 405, Depth infinity 403, body > 8 MiB → 413, no error text in bodies); `TestSyncTokenOfAnotherEpochIsRefused`, `TestSyncCollectionReportsDeletionsPerUID`, `TestUndecryptableEventDoesNotFailTheReport`; `TestGoWebdavClientDiscoversAndReads` (go-webdav caldav.Client: FindCurrentUserPrincipal, FindCalendarHomeSet, FindCalendars, MultiGetCalendar); `FuzzReportBody`.
- [ ] **Step 2–4**: FAIL → implement → `make lint test` PASS. **Step 5: Commit** `✨ Serve calendars over CalDAV`.

### Task 7: `pcb serve`, listener, TLS, config, doctor

**Files:**
- Create: `internal/cli/serve.go`, `internal/service/serve.go` (or a small `internal/server` composition package that only the CLI's serve command imports, calling service + caldav), `internal/tlsutil/selfsigned.go`
- Modify: `internal/config/config.go` (`[caldav] listen, hostnames, tls_cert, tls_key`, strict), `internal/doctor` (listener/TLS/clients checks: certificate expiry < 30 days warns, fingerprint shown, no client registered warns), `README.md`, `spec/SPEC.md` §9/§13/§14
- Test: `internal/tlsutil/selfsigned_test.go`, `internal/cli/serve_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `pcb serve [--listen ADDR]`: takes the sync lock (busy → exit 20 "a sync is already running"), runs the M2 watch loop and the CalDAV listener in one process, logs one line per pass and per listener event (structured, IDs only), stops cleanly on SIGINT/SIGTERM (exit 0; listener drained ≤ 5 s); first start prints the URL to enter in Thunderbird, the TLS fingerprint when TLS is used, and the `pcb client add` hint when no client exists.
- [ ] **Step 1: Failing tests**: `TestNonLoopbackBindNeverServesPlainHTTP` (0.0.0.0 and [::] → TLS with a generated cert; loopback → HTTP), `TestSelfSignedCertIsPersistentAndPrivate` (second start reuses it; key 0600; SANs = hostnames + bind IPs), `TestUserCertIsUsed`, `TestServeRefusesWhileSyncRuns`, `TestServeStopsOnSignal`, config strictness tests for `[caldav]`.
- [ ] **Step 2–4**: FAIL → implement → `make lint test` PASS. **Step 5: Commit** `✨ Add pcb serve with the CalDAV listener`.

### Task 8: E08/E11/E17 write-ups, Thunderbird checklist, docs

**Files:**
- Create: `spec/experiments/E17-go-webdav-audit.md`, `spec/experiments/E09-decrypt-cost.md`
- Modify: `spec/experiments/E08-thunderbird-caldav.md` (what the server now does per MUST item, link to the contract tests), `spec/experiments/E11-*` notes inside E08 if no file exists, `spec/HUMAN-CHECKS.md` ("6. Thunderbird against the bridge": the M3 steps — local HTTP, network TLS with the fingerprint check, client add, discovery from the bare URL and from `/.well-known/caldav`, offline/cached mode, an edit in the web app appearing in Thunderbird within one interval, a deletion, a revoked client; the 10 live checks of the research list), `spec/SPEC.md` (§6 per 0012, §9 as built, §13 `pcb serve`/`pcb client`, §17 status, §18 M3), `spec/OPEN.md`, `README.md` (Thunderbird setup in a few lines; credits: go-webdav as a test client, the reference bridges' CalDAV design notes)
- [ ] **Step 1**: write-ups with the evidence and markers; **Step 2**: checklist with exact commands and expected outputs; **Step 3**: docs; **Step 4: Commit** `📝 Write up the CalDAV experiments and the Thunderbird session`.
