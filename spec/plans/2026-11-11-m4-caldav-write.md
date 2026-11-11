# M4 CalDAV Write Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Thunderbird edits flow back to Proton. A PUT creates or updates an event, including a single occurrence of a series. A DELETE removes an event or one occurrence. Each write is encrypted and signed the way Proton's own clients do it, goes to Proton synchronously, and is read back into the cache before the answer, so the next sync shows exactly what Proton stored. Refusals are honest 403s that touch nothing; a stale `If-Match` is a 412. Everything is proven against the fake Proton, which now implements the write endpoint with Proton's rules. The live checks (E10, E12, E13, E14, the Thunderbird session) are deferred human checks.

**Architecture:**
- **C0 `internal/ical`** gains the write side. Input normalisation and validation (typed refusals). Property routing to card parts (research B.1). A line-level patch of the current parts, so untouched lines keep their part and bytes. VALARM → Proton notifications.
- **C1 `internal/proton/cards`** gains `Seal`, the inverse of `Open`.
- **C1 `internal/proton/calendar`** verifies the member-passphrase signature (R17). It exposes the primary calendar key, the member's address signer ring and the member permissions.
- **C1 `internal/proton/events`** gains `ListByUID` and `Sync` (`PUT /calendar/v1/{cal}/events/sync`).
- **C2 cache** gains `ApplyWrite` and a persistent href-alias table.
- **C3 service** gains a pure write planner (current group + incoming VCALENDAR → row operations or a refusal), plus an executor: refresh, precondition, seal, sync, read back, apply. The executor is serialised with Serve's sync writer.
- **A1 `internal/caldav`** answers PUT and DELETE.

**Tech Stack:** as M3; `time/tzdata` embedded (TZID validation in a container without zoneinfo).

**Spec:** `spec/SPEC.md` §5.3, §5.4, §8.1, §8.2, §8.4, §9; `spec/experiments/E08-thunderbird-caldav.md` §4, §6 (writes, ETags, recurrence); research `spec/experiments/raw/m4-research.md` (sections A–L, cited below as "research X"); decision 0012; `spec/OPEN.md` "For M4" and "Parked by M3".

**Out of scope (YAGNI, say so in review):**
- Invitations and attendee changes (refused, §8.4).
- Writes to calendars that are shared with us, or whose passphrase signature does not verify with one of our address keys (read-only until a public-key lookup exists).
- "This and following" (RANGE=THISANDFUTURE, refused).
- An offline write queue (§8.1: none in v1).
- MKCALENDAR, PROPPATCH (405) and VTODO.
- Personal parts on write: alarms go to the row `Notifications`, as in every reference writer (research D).
- Retries of a write.
- The daemon behaviours (M5).

## Global Constraints

- Everything in the M1, M2 and M3 Global Constraints still holds: the envelope; exit codes; secrets never in argv, logs, errors or responses; adapters call only `service.API`; nothing decrypted on disk; tests bite; live checks only on the test account and only as deferred steps; commit style; SSH push for workflows.
- Never copy code from the reference bridges; never read ProtonMail/WebClients (GPL). Research citations are file:line in `m4-research.md`.
- **One write endpoint:** `PUT /calendar/v1/{calID}/events/sync`, body `{MemberID, Events: [{Overwrite?, Event?, ID?}]}`. The shapes and per-entry codes are in research A (1001 envelope; 2001 signature or sequence; 2000 status; 2011 RRULE with RECURRENCE-ID, or colour; 2501 gone; 2061 not found). Deletes answer `Responses: []`.
- **Routing table (research B.1):**
  - shared signed: UID, DTSTAMP, DTSTART, DTEND, RECURRENCE-ID, RRULE, EXDATE, SEQUENCE, ORGANIZER;
  - shared encrypted: UID, DTSTAMP, CREATED, SUMMARY, DESCRIPTION, LOCATION and **every other VEVENT property**;
  - calendar signed: UID, DTSTAMP, EXDATE, STATUS, TRANSP;
  - calendar encrypted: UID, DTSTAMP, COMMENT;
  - attendees: UID, DTSTAMP, ATTENDEE.
  - EXDATE goes in **both** signed parts. DURATION is converted to DTEND before routing.
  - Never routed into a part: VALARM (→ `Notifications`), COLOR (→ the row `Color`, kept as stored), VTIMEZONE, and VCALENDAR-level properties. Also stripped: `X-PCB-INTEGRITY`, `X-PCB-PLACEHOLDER`, `X-PM-SESSION-KEY`.
- **Fragments:** `BEGIN:VCALENDAR`/`BEGIN:VEVENT` … `END:VEVENT`/`END:VCALENDAR`, CRLF, folded at 75 octets, no VERSION/PRODID, no trailing CRLF.
  - Signed parts: `Data` is the exact signed bytes plus a detached armored **binary-mode** signature.
  - Encrypted parts: SEIPD under the session key, `Data` = base64 of the data packet only, `Signature` = detached over the plaintext.
  - Signer: the **primary key of our member's address** (`Access.AddressID`), alone in a one-key ring.
  - Key packets: to the **primary calendar key only** (bootstrap `Keys[].Flags & 2`), on create only. Updates reuse the row's session keys and send no key packets.
- **Write gate (ruling M4-R1):** writable ⇔ calendar `Type` normal ∧ our member's `Permissions & 8` (Write; bit values [claim: caloxide], E14 live) ∧ the member-passphrase signature verifies with one of our address keys (R17). Otherwise the collection advertises read only and every write is 403 `DAV:need-privileges`. This replaces M3-R1/M3-R15's "owned normal ⇒ write".
- **Refusals (403, a pcb precondition element in the `urn:pcb` namespace, a short reason, no Proton text):**
  - a placeholder, undecryptable or unverified group (service state, never the `X-PCB-PLACEHOLDER` property; OPEN.md);
  - a row with an `AddressKeyPacket` (someone else's invitation);
  - attendee add or remove, or ORGANIZER+ATTENDEE on create;
  - RANGE, RDATE, EXRULE;
  - an RRULE outside Proton's supported set (research E.1: FREQ DAILY|WEEKLY|MONTHLY|YEARLY; COUNT ≤ 49; UNTIL ≤ 2037-12-31; not both COUNT and UNTIL; MONTHLY with one BYDAY+BYSETPOS (setpos ∈ {-1,1,2,3,4}) or one BYMONTHDAY; YEARLY BYMONTHDAY needs BYMONTH; INTERVAL ≤ 999 daily / 4999 weekly / 99 yearly);
  - floating date-times; a TZID Go's tz database cannot resolve (after mapping `/mozilla.org/<date>/<Area/City>` → `Area/City`);
  - a body that is not one VCALENDAR with VEVENTs of a single UID;
  - more than 100 VEVENTs.
- **Accepted and normalised (no refusal, logged at debug):**
  - `STATUS:TENTATIVE` dropped (Proton refuses it, 2000);
  - unsupported VALARMs dropped (absolute or `RELATED=END` triggers, after start (except all-day < 24 h), out of bounds; at most 10 kept);
  - client COLOR ignored;
  - VALARM-level `X-MOZ-*` lost, documented (research F).
- **Notifications (research D):** EMAIL → Type 0; DISPLAY/AUDIO → Type 1; Trigger as a negative duration. Tri-state on update:
  - the incoming set equals, by meaning, the **effective** set we render (the calendar defaults when the row has `null`) → send the row's value unchanged (`null` stays `null`);
  - no VALARM in the client → `[]`;
  - otherwise the new array.
  On create: no VALARM → `[]`, unless the set equals the defaults → `null`.
- **SEQUENCE (ruling M4-R4, research L.2):** keep `max(incoming, stored)`. Bump to stored+1 when the shared signed content changes (times, RRULE, EXDATE, RECURRENCE-ID) and the client did not bump it. Write exceptions with SEQUENCE ≥ master's. No bump on text-only or alarm-only edits; the live check decides ([conflict], HUMAN-CHECKS).
- **Write flow (ruling M4-R5):**
  1. Resolve href → UID (alias or canonical).
  2. Gate.
  3. **Refresh the UID's rows from Proton** (`ListByUID`) and apply them to the cache.
  4. Evaluate the precondition on the fresh object ETag: `If-Match` exact string → 412 on mismatch; `If-None-Match: *` → 412 if the href exists; a UID living at another href → 409 `<C:no-uid-conflict><D:href>…</D:href></C:no-uid-conflict>`.
  5. Plan.
  6. Seal.
  7. **One** `Sync` call holding every row operation, master first.
  8. Read back with `ListByUID`, then `ApplyWrite` (seq bump, feed cursor untouched), then answer **201** (create) or **204**, with no `ETag` header.
  - Proton 429, 5xx or network failure → 503 + Retry-After; never retried. An ambiguous failure (the request left) → read back and apply, then 503.
  - A per-entry Proton code → read back and apply, then 403 with `pcb:proton-refused`.
  - Writes and the syncer's applies for one calendar are serialised.
- **Deleting one occurrence** = master update with a new EXDATE + deletion of that occurrence's exception row, in one call. Deleting the resource = every row of the UID in one call. A PUT whose master changes times or RRULE keeps the exception rows that are still in the body, and deletes the ones that are absent (mirror the body, research E.2).
- **Aliases (ruling M4-R6):** a PUT create at a name that is not the canonical name of its UID records `(calendar, name) → UID` in the cache. Listings, multiget and sync-collection then serve that UID at the alias href (one href per UID). A cache re-creation drops aliases (new epoch: Thunderbird resyncs).
- No new runtime dependency. tzdata is embedded in `internal/ical` only.

## Review Focus

1. **Editing only the title in Thunderbird** of an event that has custom notifications, a colour, attendees statuses, `X-MOZ-*` lines, a COMMENT and an ORGANIZER → Proton receives only the changed SUMMARY line, in the shared encrypted part. Every other part's bytes are unchanged; Notifications, Color and Attendees are re-sent as stored; SEQUENCE is not bumped. Pinned by `TestTitleEditChangesOnlyThatLine` (Task 7).
2. **A PUT with a stale `If-Match`**, after the event was edited in the web app (the fake changed it after the last sync) → 412, and no write reaches the fake. Pinned by `TestStaleIfMatchIs412AndWritesNothing` (Task 8).
3. **Deleting one occurrence of a weekly series that has an edited exception at that date** → one sync call carrying the master update with the EXDATE and the deletion of the exception row. The next sync-collection shows the UID changed, not deleted. Pinned by `TestDeleteOneOccurrenceOfASeries` (Task 7).
4. **An event whose row Notifications is `null`**, edited in Thunderbird without touching its alarms (it sends back the defaults it saw as VALARMs) → `Notifications` stays `null`. Pinned by `TestUnchangedDefaultAlarmsKeepNull` (Task 3/Task 7).
5. **A write to a placeholder, to an event with an unverified own part, to an invitation row, to a read-only calendar, or to a calendar whose passphrase signature fails** → 403, and the fake records no sync call. Pinned by `TestRefusedWritesNeverReachProton` (Task 8).

---

### Task 1: The fake's write endpoint, with Proton's rules

**Files:**
- Modify: `internal/proton/protontest/{calendars.go,events.go,cards.go,fake.go}`
- Create: `internal/proton/protontest/sync.go`
- Test: `internal/proton/protontest/sync_test.go`

**Interfaces:**
- Produces:
  - `PUT /calendar/v1/{cal}/events/sync`. It parses the shared signed part into the row columns (StartTime, EndTime, StartTimezone, FullDay, RRule, RecurrenceID, Exdates). It verifies every part's signature with the member's address key (2001 otherwise). It requires key packets on create and decrypts them with the calendar key (2001 otherwise). It enforces SEQUENCE ≥ master for exceptions (2001), RRULE with RECURRENCE-ID → 2011, STATUS TENTATIVE → 2000, and a non-palette Color → 2011. Notifications are tri-state: absent on update = keep. Everything else is fully replaced. Answers use a 1001 envelope with per-entry codes; deletes answer `Responses: []`. Every write emits feed entries.
  - `GET /calendar/v1/{cal}/events?UID=`.
  - Switches in `protontest.Options.Write`: `DropUnbumpedSequence`, `RefuseNewKeyPacketsOnUpdate`, `RejectDuplicateUID`, `AllOrNothing`. Each [unknown] in research L.4 can then be tested both ways.
  - `Fake.SyncCalls()` (count and bodies), for "no write reached Proton" assertions.
  - Bootstrap fields: `Keys[].Flags` (1 active, 2 primary; serve one primary and one retired key), `Passphrase.MemberPassphrases[].Signature` (a real signature by the member's address key; `Options.BadPassphraseSignature` serves a wrong one), and `Members[].Permissions`.
  - Routing fixes: EXDATE in both signed parts and `Exdates` served; DURATION → DTEND; no VALARM in personal parts by default.
- [ ] **Step 1: failing tests:**
  - `TestSyncCreateParsesTheSignedPart`
  - `TestSyncRefusesABadSignature`
  - `TestSyncRefusesKeyPacketsToARetiredKey`
  - `TestSyncUpdateReplacesAndKeepsAbsentNotifications`
  - `TestSyncExceptionNeedsSequenceAtLeastMaster`
  - `TestSyncDeleteAnswersNoResponses`
  - `TestSyncWritesReachTheFeed`
  - `TestListByUID`
  - `TestBootstrapServesFlagsAndASignedPassphrase`
  - one test per switch.
- [ ] **Step 2–4:** FAIL → implement → `make lint test` PASS. Existing M3 tests stay green, since the routing fixes change served bytes. Fix any test that pinned the old routing.
- [ ] **Step 5: Commit** `✨ Teach the fake Proton the event write endpoint`.

### Task 2: Calendar write keys and the write gate (R17, M3-R15)

**Files:**
- Modify: `internal/proton/calendar/{unlock.go,list.go}`
- Modify: `internal/service/{objects.go,api.go,render.go}` (`Collection.ReadOnly` from the gate)
- Test: `internal/proton/calendar/unlock_test.go`, `internal/service/objects_test.go`

**Interfaces:**
- Produces:
  - `calendar.Access` gains:
    - `Primary *crypto.KeyRing` (the primary calendar key alone);
    - `Signer *crypto.KeyRing` (the primary key of our member's address alone);
    - `Permissions int`;
    - `PassphraseVerified bool` (the member-passphrase signature verifies with our address keyring, research G);
    - `Writable() bool`, per M4-R1.
  - A failed verification is not an unlock error: reads still work, and the calendar is read-only.
  - The service: `Collection.ReadOnly = !Writable()`. The `KeyState` for ETags already covers key changes.
- [ ] **Step 1: failing tests:**
  - `TestPrimaryKeyIsTheFlaggedOne`
  - `TestPassphraseSignatureVerifiedWithOurAddressKey`
  - `TestBadPassphraseSignatureLeavesTheCalendarReadOnly`
  - `TestWritableNeedsTheWriteBit`
  - `TestCollectionReadOnlyFollowsTheGate`
- [ ] **Step 2–4** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `🔒 Verify the member passphrase and gate writes per calendar`.

### Task 3: The iCalendar write side (pure)

**Files:**
- Create: `internal/ical/write.go`, `internal/ical/validate.go`, `internal/ical/alarms_write.go`
- Test: `internal/ical/write_test.go`, `internal/ical/validate_test.go`, `internal/ical/alarms_write_test.go`, `FuzzParseWrite`

**Interfaces:**
- Produces:
  - `ParseWrite(body []byte) (Incoming, error)`: one VCALENDAR; VEVENTs of one UID; master and exceptions keyed by RECURRENCE-ID (Unix); VTIMEZONE accepted and dropped; normalisation per the Global Constraints. Errors are `*Refusal{Reason string, Code RefusalCode}`, with codes `NotOneUID`, `TooMany`, `Floating`, `UnknownTZID`, `UnsupportedRecurrence`, `Range`, `RDate`, `ExRule`, `BadData`.
  - `Route(ev Component) map[PartKind][]Property`, per the routing table.
  - `Patch(current map[PartKind][]byte, incoming Component) (map[PartKind][]byte, changed map[PartKind]bool, error)`:
    - an unchanged property line keeps its bytes and its part;
    - a changed one is replaced in place;
    - a new one goes where `Route` says;
    - a removed one is removed;
    - property order inside a part is preserved, and new lines are appended before `END:VEVENT`.
  - `NotificationsFromAlarms(alarms []Component, allDay bool) ([]Notification, dropped int)`, and `SameNotifications(a, b []Notification) bool` (by meaning: type plus signed offset, in any order).
  - `IsSharedSignedChange(old, new Component) bool` for the SEQUENCE rule.
  - tzdata: `import _ "time/tzdata"` in validate.go only.
- [ ] **Step 1: failing tests** (each refusal code has its own case):
  - `TestRefusals`
  - `TestNormalisationStripsOurAndRenderedLines`
  - `TestRoutingTable`
  - `TestPatchKeepsUntouchedBytes`
  - `TestPatchMovesNothingAcrossParts`
  - `TestTentativeIsDropped`
  - `TestNotificationsFromAlarms` (bounds and dropped forms)
  - `TestUnchangedDefaultAlarmsKeepNull` (the comparison half)
  - `TestMozillaTZIDIsMapped`
  - fuzz: `ParseWrite` never panics, and `Patch(Route(x))` round-trips.
- [ ] **Step 2–4** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Parse, validate and patch events for writing`.

### Task 4: `cards.Seal`

**Files:**
- Create: `internal/proton/cards/seal.go`
- Modify: `cards.go` (export the opened session keys)
- Test: `internal/proton/cards/seal_test.go`

**Interfaces:**
- Produces:
  - `Seal(parts map[PartKind][]byte, k SealKeys) (WireEvent, error)`.
    - `SealKeys{Signer, CalendarPrimary *crypto.KeyRing; Shared, Calendar *crypto.SessionKey}`: nil session keys mean a create, which mints fresh AES-256 keys and returns key packets.
    - `WireEvent` is the JSON-ready `SharedEventContent`, `CalendarEventContent`, `AttendeesEventContent`, `SharedKeyPacket` and `CalendarKeyPacket`.
  - `Event.SessionKeys() (shared, calendar *crypto.SessionKey)`. It returns them only when every encrypted part that used them verified as ours, and `ErrUntrustedSessionKey` otherwise (research G).
- [ ] **Step 1: failing tests:**
  - `TestOpenSealRoundTrip` (property over random fragments);
  - `TestSealSignsBinaryExactBytes`;
  - `TestSealKeyPacketOpensWithThePrimaryOnly`;
  - `TestUpdateReusesSessionKeysAndSendsNoPacket`;
  - `TestUntrustedSessionKeyIsRefused`;
  - the fake's sync accepts what Seal produces (create and update).
- [ ] **Step 2–4** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Seal event parts the way Proton reads them`.

### Task 5: `events.ListByUID` and `events.Sync`

**Files:**
- Create: `internal/proton/events/write.go`
- Test: `internal/proton/events/write_test.go`

**Interfaces:**
- Produces:
  - `ListByUID(ctx, c, calID, uid) ([]Row, error)`, paged.
  - `Sync(ctx, c, calID, memberID string, ops []Op) ([]Result, error)`:
    - `Op{Create *WireEvent | Update{ID, *WireEvent, Notifications, Color, Attendees} | Delete{ID}}`;
    - `Result{Index, Code int, Row *Row}`;
    - typed per-entry errors: `ErrSequence`, `ErrSignature`, `ErrStatus`, `ErrInvalid`, `ErrGone`, `ErrNotFound`, keeping the raw code;
    - session-wide failures are classified as in M2 (429/5xx/network);
    - `ErrAmbiguous` when the request may have been applied.
  - Never retried.
- [ ] **Step 1: failing tests:**
  - `TestSyncMapsEntryCodes`
  - `TestSyncDeleteHasNoResponses`
  - `TestSyncIsNeverRetried`
  - `TestTimeoutAfterSendIsAmbiguous`
  - `TestListByUIDPages`
- [ ] **Step 2–4** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Write events to Proton in one sync call`.

### Task 6: Cache `ApplyWrite` and href aliases

**Files:**
- Modify: `internal/store/cache/{cache.go,schema.go,snapshot.go}`
- Test: `internal/store/cache/write_test.go`

**Interfaces:**
- Produces:
  - `ApplyWrite(ctx, calID string, uid string, rows []events.Row) error`: replaces the UID's rows with exactly `rows`. Each changed row gets a seq bump; removed rows become tombstones with the UID. The feed cursor is untouched, and a later feed re-read of an equal row changes nothing.
  - `PutAlias(calID, name, uid)`, `AliasOf(calID, name) (uid, ok)`, `AliasesFor(calID) map[uid]name`, all exposed on `View`.
  - Schema version bump with a migration from M3 files (empty alias table).
- [ ] **Step 1: failing tests:**
  - `TestApplyWriteReplacesTheGroup`
  - `TestApplyWriteKeepsTheCursor`
  - `TestFeedRereadOfAWrittenRowIsANoop`
  - `TestAliasesSurviveReopenAndDieWithTheEpoch`
  - `TestMigratesAnM3File`
- [ ] **Step 2–4** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Apply written rows and remember client hrefs`.

### Task 7: The service write planner and executor

**Files:**
- Create: `internal/service/{writeplan.go,write.go}`
- Modify: `api.go` and `serve.go` (serialise with the syncer)
- Test: `internal/service/{writeplan_test.go,write_test.go}`

**Interfaces:**
- Consumes: Tasks 2–6.
- Produces:
  - `Plan(current Group, in ical.Incoming, defaults Defaults) (WritePlan, error)`. It is pure: `WritePlan{Ops []events.Op, Created bool}`, and errors are `*WriteRefusal{Code, Reason}`. Rules: Global Constraints (refusals, normalisation, notifications tri-state, SEQUENCE, exceptions mirrored, delete one occurrence, attendees passthrough, Color re-sent).
  - Service API:
    - `PutObject(ctx, calID, name string, body []byte, cond Precondition) (PutResult{Created bool}, error)`
    - `DeleteObject(ctx, calID, name string, cond Precondition) error`
    - `Precondition{IfMatch string; IfNoneMatchAny bool}`
  - Errors: `ErrPreconditionFailed`, `*UIDConflictError{Href}`, `ErrNotWritable`, `*WriteRefusal`, plus the existing unavailable errors (503).
  - Only during Serve (writer handle); otherwise `ErrNotServing`.
- [ ] **Step 1: failing tests, planner:**
  - `TestTitleEditChangesOnlyThatLine`
  - `TestUnchangedDefaultAlarmsKeepNull`
  - `TestTimeEditBumpsSequence`
  - `TestExceptionCreateHasSequenceAtLeastMaster`
  - `TestDeleteOneOccurrenceOfASeries`
  - `TestMasterEditMirrorsExceptions`
  - `TestAttendeeChangeIsRefused`
  - `TestInvitationRowIsRefused`
  - `TestPlaceholderGroupIsRefused` (service state, not the property)
- [ ] **Step 1: failing tests, executor (with the fake):**
  - `TestPutCreatesAndReadsBack`
  - `TestPutUpdateIsVisibleInTheNextChanges`
  - `TestRefreshBeforePrecondition`
  - `TestProton5xxIs503AndNeverRetried`
  - `TestAmbiguousFailureReadsBack`
  - `TestEntryRefusalIs403AndCacheRefreshed`
  - `TestWritesSerialiseWithTheSyncer` (-race)
  - Every fake switch from Task 1 keeps the outcome truthful: a silently dropped edit is detected by the read-back and answered 503, never a false 204.
- [ ] **Step 2–4** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Plan and execute CalDAV writes against Proton`.

### Task 8: CalDAV PUT and DELETE

**Files:**
- Modify: `internal/caldav/{server.go,propfind.go,report.go}`
- Create: `internal/caldav/write.go`
- Test: `internal/caldav/write_test.go`, `contract_test.go` (E08 MUST 10, 11, 12 and the §4 items)

**Interfaces:**
- Consumes: the Task 7 API.
- Produces:
  - PUT: `Content-Type` text/calendar (else 415); body ≤ 8 MiB; `If-Match` / `If-None-Match: *`; answers 201/204 with no ETag header.
  - Status mapping:
    - `ErrPreconditionFailed` → 412
    - `UIDConflictError` → 409 `no-uid-conflict` with the href
    - `ErrNotWritable` → 403 `need-privileges`
    - `WriteRefusal` → 403 `<pcb:… xmlns:pcb="urn:pcb">`
    - unavailable → 503 + Retry-After
  - DELETE: 204; 404 when missing; 412 on mismatch.
  - Aliases served in every listing and resolved on every method.
  - `current-user-privilege-set` follows the gate.
- [ ] **Step 1: failing tests:**
  - `TestStaleIfMatchIs412AndWritesNothing`
  - `TestRefusedWritesNeverReachProton`
  - `TestCreateAtASanitisedNameIsServedThere`
  - `TestUIDConflictNamesTheOtherHref`
  - `TestDeleteMissingIs404`
  - `TestWriteThenMultigetSeesProtonsBytes`
  - an end-to-end test over the real listener with go-webdav's client (`PutCalendarObject`, then `MultiGetCalendar`)
  - contract tests for E08 MUST 10–12.
- [ ] **Step 2–4** FAIL → implement → PASS.
- [ ] **Step 5: Commit** `✨ Accept CalDAV writes`.

### Task 9: Mapping table, write-ups, live checklist, docs

**Files:**
- Create: `spec/mapping.md` (§8.4), `spec/experiments/E12-recurrence-forms.md`, `E13-unknown-properties.md`, `E14-member-permissions.md` and `E10-signatures.md`. Each has a status, what the code assumes, and the live steps.
- Modify:
  - `spec/HUMAN-CHECKS.md`: a "M4 live session" with the 12 checks of research L.4 merged into §6 (Thunderbird), on the test account only, in an order that needs the fewest author actions;
  - `spec/SPEC.md` §5.3, §5.4, §8.1, §8.2, §8.4, §9, §13, §17, §18;
  - `spec/OPEN.md`;
  - `README.md` (writes, what is refused).
- [ ] **Step 1:** write-ups and the mapping table (every property: routed part, transformed, stripped with a note, or refused).
- [ ] **Step 2:** the checklist, with exact commands and expected outputs.
- [ ] **Step 3:** docs.
- [ ] **Step 4: Commit** `📝 Write up M4: mapping, experiments, live checklist`.
