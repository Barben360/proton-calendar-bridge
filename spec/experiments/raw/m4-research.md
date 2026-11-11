# M4 research — CalDAV writes (Proton sync endpoint, card sealing, Thunderbird write flow)

Date: 2026-11-11. Read-only study; no code copied, behaviour described and cited as file:line relative to each project's own tree. No call to Proton was made; ProtonMail/WebClients (GPL) was **not** read: where a bridge says it derived a rule from the web client, that is reported as the bridge's claim.

Sources (versions as read):
- caloxide = WentTheFox/caloxide (https://github.com/WentTheFox/caloxide), commit 77fad978, 2026-11-05. MIT.
- carbonate = develonrails/carbonate (https://github.com/develonrails/carbonate), commit 87d387cd, 2026-11-02. MIT. Its write path says it is "read from protoxide" (https://github.com/mathewcsims/protoxide, MIT; not read here) (`carbonate/docs/ARCHITECTURE.md:395-400`).
- cal-gateway = jmdlab/cal-gateway (https://github.com/jmdlab/cal-gateway), commit 1bf8e4e7, 2026-10-14. MIT. Client is Apple Calendar (`dataaccessd`), not Thunderbird.
- proton-cal = cheeseandcereal/proton-cal (https://github.com/cheeseandcereal/proton-cal), commit c3e382b3, 2026-10-28. **Unlicense**. `docs/*.md` are live-verified by its author [claim].
- SevenOfNine-labs = SevenOfNine-labs/proton-calendar-bridge (https://github.com/SevenOfNine-labs/proton-calendar-bridge), commit 10dfc8af. REST only, and its Proton writes are stubs returning NotSupported (`internal/provider/proton.go:267-277`): ignored below.
- go-proton-api = ProtonMail/go-proton-api `v0.0.0-20261022084531-3f8fbebac58d` (https://github.com/ProtonMail/go-proton-api), MIT, module cache.
- gopenpgp = ProtonMail/gopenpgp `v2.11.1-proton` (https://github.com/ProtonMail/gopenpgp), MIT, module cache.
- Ours: `internal/proton/{calendar,cards,events,api,protontest}`, `internal/ical`, `internal/service`, `internal/caldav`, `internal/store/cache` at main 7e689ad; `spec/SPEC.md`, `spec/OPEN.md`, `spec/decisions/0012-icalendar-native-model.md`, `spec/experiments/E08-thunderbird-caldav.md` ("E08 §x", authoritative for Thunderbird, "TB"), `spec/experiments/raw/m3-research.md` ("M3-RES x").

Confidence legend: [source] = read in the cited code; [claim: X live] = project X says it observed it against a real account; [claim: X] = X states it, no live evidence (often "from the web client"); [inferred] = my reading, not tested; [conflict] = sources disagree, live check needed.

---

## A. Write endpoints (Q1)

### A.1 One endpoint: `PUT /calendar/v1/{calID}/events/sync`
All four bridges write only through it; no POST/DELETE per event exists [claim: proton-cal live] (`proton-cal/docs/api.md:157-167`; `carbonate/docs/ARCHITECTURE.md:416-421`; `cal-gateway/internal/proton/write.go:650-699`; `caloxide/protonmail/calendar.go:1208,1259`). go-proton-api has **no** calendar write and its fake server has **no** calendar route [source] (`go-proton-api/calendar.go:9-65`, `calendar_event.go:11-54`: GETs only; `server/*.go` contains no calendar handler). So our `protontest` fake must implement it (§L.3).

Request envelope [claim: proton-cal live] (`proton-cal/docs/api.md:171-187`; `proton-cal/pkg/event/wire.go:44-58`):

| Field | Where | Value / rule | Evidence |
|---|---|---|---|
| `MemberID` | top level | our member ID in that calendar | all four bridges; caloxide's delete omits it and still works [claim: caloxide] (`caloxide/protonmail/calendar.go:1249-1257`) |
| `IsImport` | top level | `0`, sent on creates only | `proton-cal/pkg/event/write.go:110-116`; `cal-gateway/internal/proton/write.go:206-211`; carbonate/caloxide never send it |
| `Events[]` | top level | one entry per operation | |
| create entry | `{"Overwrite":0,"Event":{…}}` | key packets included | `proton-cal/docs/api.md:183`; `carbonate/internal/calendar/write.go:112-115` |
| update entry | `{"ID":"<row ID>","Event":{…}}` | **no key packets** (server keeps the originals) | [claim: proton-cal live] `api.md:184,257-267`; [claim: carbonate live] `ARCHITECTURE.md:408-411` |
| delete entry | `{"ID":"<row ID>"}` (+ `DeletionReason:0` in carbonate/caloxide) | | `api.md:185`; `carbonate/internal/calendar/write.go:122-126`; `caloxide/protonmail/calendar.go:1244-1257` |

`Overwrite: 1` semantics: **unknown** (no source uses it) [inferred: maybe "replace the event with the same UID"; do not use].

### A.2 `Event` body fields

| Field | Create | Update | Notes / evidence |
|---|---|---|---|
| `Permissions` | `1` | `1` | every bridge sends 1 for own events (`proton-cal/pkg/event/write.go:45,229`; `cal-gateway/internal/proton/write.go:396`, `update.go:431`; `carbonate/.../write.go:413`; `caloxide/.../calendar.go:1021`). Meaning of the event-level bit unknown |
| `IsOrganizer` | optional | optional | proton-cal omits it [claim: proton-cal live] (`api.md:222-223`); carbonate always 1 (`write.go:413,423`); caloxide 1 on create, old value on update (`calendar.go:1048-1053`); cal-gateway only with attendees (`write.go:196-198`) |
| `SharedKeyPacket` | b64 PKESK of the shared session key → calendar key | omitted | `api.md:194,259`; carbonate/caloxide send one on update only when the row had none (`carbonate/.../write.go:837-863`; `caloxide/.../calendar.go:1088-1095`) |
| `CalendarKeyPacket` | b64 PKESK of the calendar session key | omitted | same; web-app events may have **no** calendar encrypted part and no `CalendarKeyPacket` [claim: proton-cal live] (`api.md:263-267`) |
| `SharedEventContent` | `[{Type:2,Data,Signature},{Type:3,Data,Signature}]` | same, resealed | arrays must be `[]`, never `null` [claim: proton-cal live] (`api.md:211-213`) |
| `CalendarEventContent` | same shape | same | a part with only UID+DTSTAMP is omitted by carbonate (`ARCHITECTURE.md:475-477`) and caloxide; proton-cal always sends both (with an empty `COMMENT:`) (`proton-cal/pkg/ical/ical.go:231-237`) — both accepted [claim: both live] |
| `AttendeesEventContent` | `[]` or `[{Type:3,…}]` (shared session key) | resealed verbatim | `cal-gateway/internal/proton/write.go:413-422` |
| `Attendees` | `[]` | **re-send** `[{Token,Status,Comment}]` from the row | omitting it wipes RSVP rows [claim: proton-cal live] (`api.md:237-248`; `proton-cal/pkg/event/write.go:251-262`) |
| `Notifications` | `null` (calendar defaults) / `[]` / array | **always re-send** (tri-state) | omitted on update = unchanged [claim: carbonate live] (`ARCHITECTURE.md:487-489`); wrong value wipes reminders [claim: proton-cal live] (`api.md:237-248`). §D |
| `Color` | `null` or palette hex | re-send the row's | off-palette: code 2011; `null` on update is ignored (colour sticks) [claim: proton-cal live] (`api.md:217-221,246-248`) |
| `AddedProtonAttendees`, `RemovedAttendeeAddresses` | invitations only | | [claim: carbonate live] (`ARCHITECTURE.md:513-536`); out of scope |
| `PersonalEventContent` | — | — | **no bridge sends it**; field name unverified [inferred] |
| Plaintext times (`StartTime`…) | **not sent** | not sent | §C |

### A.3 Response
- HTTP 200 even when an entry fails; top-level `Code` **1001** (batch) or 1000; per entry `Responses[{Index, Response:{Code, Error, Event}}]` [claim: proton-cal live] (`api.md:269-284`); [claim: carbonate live] (`ARCHITECTURE.md:423-428`).
- Create echoes the full row; update "may omit it"; a **delete answers `{"Code":1001,"Responses":[]}`** [claim: carbonate live] (`ARCHITECTURE.md:423-427`; `carbonate/.../write.go:277-334`).
- Our `api.Client.parse` accepts 1000/1001 at top level (`internal/proton/api/client.go:270-286`) [source]: the per-entry codes must be checked by the caller.

### A.4 Error codes collected

| Code / status | Meaning | Evidence |
|---|---|---|
| 2001 | exception row SEQUENCE < master's: "Single edits should have a Sequence greater or equal to main event" | [claim: proton-cal live] `api.md:376-378`, `overview.md` error table |
| 2001 | "Provide data signed using the address key" (signed with another address's key) | [claim: carbonate live] `ARCHITECTURE.md:615-617`; [claim: caloxide live] `calendar.go:1060-1063` |
| 2000 | `STATUS:TENTATIVE` refused (CONFIRMED, CANCELLED accepted) | [claim: cal-gateway live] `docs/FEATURE-MATRIX.md:160` |
| 2011 | "These properties are not supported": RECURRENCE-ID together with RRULE | [claim: carbonate live] `ARCHITECTURE.md:455-459` |
| 2011 | off-palette `Color` | [claim: proton-cal live] `api.md:217-221` |
| 2501 | event/calendar does not exist (delete of a deleted ID) | [claim: proton-cal live] `overview.md`; `cal-gateway/internal/proton/account.go:628`, `write.go:283-290` |
| 2061 | "not a valid ID" (GET with a malformed/unknown ID) | [claim: caloxide live] `calendar.go:1174-1180`; carbonate `ARCHITECTURE.md:430-431` |
| HTTP 500 | `SEQUENCE;VALUE=TEXT:0` (must be bare `SEQUENCE:0`) | [claim: carbonate live] `ARCHITECTURE.md:613-614`; caloxide `calendar.go:973-1001` |
| (refusal text) | "Shared event has attendees but does not have an organizer" | [claim: carbonate live] `ARCHITECTURE.md:618-622` |
| silent no-op | update whose SEQUENCE does not increase is "silently dropped" | [conflict] carbonate/caloxide say so live (`ARCHITECTURE.md:608-612`; `caloxide/.../calendar.go:976-1001`); proton-cal's live suite edits text **without** bumping and bumps only on time/recurrence changes (`api.md:376-380`; `proton-cal/pkg/event/event.go:183-185`) |
| 9101 | locked scope | not needed for event writes [claim: proton-cal live] |
| 429 | Retry-After | `overview.md`; our client retries 429 for any method (`client.go:119-129`) |

Not found anywhere: a "UID already exists" error, a size limit on parts, a limit on `Events[]` per batch. Duplicate same-UID masters exist in real data (M3-R3; `cal-gateway/docs/FEATURE-MATRIX.md:136`, "recurring-master corruption case"), so the server most likely **does not** enforce UID uniqueness [inferred].

### A.5 Idempotency and atomicity
- No idempotency key; a create retried after a lost answer can create a second master with the same UID [inferred]. Our client does not retry non-GET on 5xx/network (`client.go:119-129,150-158`) [source]: keep it so.
- A batch can partially fail (per-entry codes) [claim: carbonate] (`ARCHITECTURE.md:420-421`). Whether mixed create/update/delete entries in one batch are allowed is **unknown**; cal-gateway batches only deletes (`write.go:262-291`), its series PUT issues one call per row (`internal/caldav/backend.go:638-735`).
- Proton records the change in the calendar event loop "a second or two" after the write [claim: carbonate live] (`ARCHITECTURE.md:203-206`): the feed cannot give read-your-writes.

### A.6 Other write endpoints (not needed for M4)
- `PUT /calendar/v1/{calID}/events/{eventID}/personal` — personal part / notifications only [claim: proton-cal, table only, unused] (`api.md:165`).
- `PUT /calendar/v1/{calID}/events/{eventID}/attendees/{attendeeID}` `{Status, UpdateTime, Comment}` — own RSVP [claim: cal-gateway live] (`FEATURE-MATRIX.md:158`, M6b paragraph).

---

## B. Card construction — the inverse of `internal/proton/cards` (Q2)

### B.1 Property routing on write
Same table in four independent places (proton-cal and carbonate/caloxide trace it to the web client / protoxide) [claim: proton-cal live; carbonate live]:

| Part | Type | Properties | Evidence |
|---|---|---|---|
| shared signed | 2 (clear + detached sig) | UID, DTSTAMP, DTSTART, DTEND, [RECURRENCE-ID], [RRULE], EXDATE*, SEQUENCE, ORGANIZER, [X-PM-CONFERENCE-ID] | `proton-cal/docs/crypto.md:173-176`; `carbonate/.../write.go:35`; `caloxide/.../calendar.go:637-647`; `cal-gateway/docs/FEATURE-MATRIX.md:143-151` |
| shared encrypted | 3 | UID, DTSTAMP, CREATED, SUMMARY, DESCRIPTION, LOCATION, [X-PM-CONFERENCE-URL] **+ every other property ("rest")** | `crypto.md:176,188-197`; `carbonate/.../write.go:36,416-421`; `caloxide/.../calendar.go:740-743` |
| calendar signed | 2 | UID, DTSTAMP, EXDATE*, STATUS, TRANSP | `crypto.md:177`; `carbonate/.../write.go:38` |
| calendar encrypted | 3 | UID, DTSTAMP, COMMENT | `crypto.md:178` |
| attendees | 3, **shared** session key | UID, DTSTAMP, ATTENDEE* (with `X-PM-TOKEN`) | `crypto.md:179`; `cal-gateway/internal/proton/write.go:413-422` |
| personal | — | VALARM | **not written by any bridge**; alarms go to the row `Notifications` (§D) |
| row columns | — | `Color` (not COLOR in a card: the web app never reads it [claim: carbonate] `write.go:71-77`), `Notifications`, `Attendees` | |

- EXDATE goes in **both** signed parts in every writer (`proton-cal/pkg/ical/ical.go:154-186,210-228`; carbonate/caloxide tables). Our fake puts EXDATE only in the calendar signed part (`internal/proton/protontest/cards.go:133`) and serves `Exdates: []` always (`protontest/events.go:391`): fix the fake for M4.
- Our fake routes DURATION to shared signed and VALARM to the personal part (`protontest/cards.go:131,139-142`); neither matches the write table above [source vs claim].

### B.2 Crypto per part [claim: proton-cal live] (`crypto.md:316-335`)
1. Two fresh AES-256 session keys on create: shared (shared encrypted + attendees) and calendar (calendar encrypted). gopenpgp `GenerateSessionKey` is AES-256 [source] (`gopenpgp/crypto/sessionkey.go:106-108`).
2. Signed parts: detached, armored signature over the **exact fragment bytes**, binary literal (sig type 0x00), by the member's **address** key.
3. Encrypted parts: SEIPD of the fragment under the session key, `Data` = base64 of the data packet only; `Signature` = detached sig over the **plaintext**, not embedded.
4. Key packets: PKESK of each session key to the **calendar** key, base64, in `SharedKeyPacket` / `CalendarKeyPacket`.
- Binary vs text mode: `crypto.NewPlainMessage` is binary, `NewPlainMessageFromString` canonicalises line ends and sets text mode, so `DetachSignText` (sig type 0x01) [source] (`gopenpgp/crypto/message.go:66-73,93-100`; `signature.go:326-330`). carbonate signs and encrypts in text mode (`write.go:820,865-876`) and works live [claim: carbonate live]; proton-cal (`pgp/pgp.go:26-27,86`) and cal-gateway (`write.go:429-437`) use binary. Use binary: it covers the bytes we send, and our reader verifies exact bytes (`internal/proton/cards/verify.go:1-19`).
- `SessionKey.Encrypt` does not compress [source] (`sessionkey.go:146-148`).

### B.3 Which key does what
- **Signer**: the address key of **our member's address** (`Access.AddressID`), not "the first address" — wrong address → 2001 [claim: carbonate, caloxide live]. gopenpgp signs with the first unlocked entity of the keyring [source] (`gopenpgp/crypto/keyring.go:95-112`), so build a one-key signer ring from that address's **primary** key [inferred].
- **Recipient of key packets**: `KeyRing.EncryptSessionKey` writes one PKESK **per entity** and fails if any entity has no valid encryption subkey [source] (`gopenpgp/crypto/keyring_session.go:78-103`). Our `calendar.Access.KR` holds every unlocked generation (`internal/proton/calendar/unlock.go:100-124`), so encrypting to it would emit several PKESKs or fail on a retired key. Encrypt to the **primary** calendar key only: bootstrap `Keys[].Flags`, `Active=1`, `Primary=2` [source] (`go-proton-api/calendar_types.go:74-79`; our fake serves `Flags:3`, `protontest/calendars.go:464`). Our `bootstrap` struct does not decode `Flags` yet (`unlock.go:58-69`).
- **AddressKeyPacket**: on read, the shared session key wrapped to one of our address keys (Proton-to-Proton invitation; preferred over `SharedKeyPacket`) (`internal/proton/cards/cards.go:217-251`; `cal-gateway/internal/proton/update.go:174-182`). On write it appears only as `AddedProtonAttendees[].AddressKeyPacket` when inviting Proton users [claim: carbonate live]. A row with `AddressKeyPacket` is an event someone else organised: M4 should refuse to rewrite it [call].

### B.4 Fragment bytes
- `BEGIN:VCALENDAR` / `BEGIN:VEVENT` … `END:VEVENT` / `END:VCALENDAR`, CRLF, lines folded at 75 octets, TEXT escaped, **no VERSION/PRODID, no trailing CRLF** in what the web client writes [claim: proton-cal live] (`crypto.md:263-271`; builder `proton-cal/pkg/ical/ical.go:239-249`). carbonate and caloxide add VERSION/PRODID via go-ical and end with CRLF (`carbonate/.../write.go:900-913`; `caloxide/.../calendar.go:705-720`) and still work live [claim]: the server does not care [inferred].
- `Data` of a signed part must be byte-identical to the signed bytes, else "impossible to verify" [claim: cal-gateway] (`write.go:31-36`).
- `SEQUENCE` bare integer (A.4).

### B.5 Updates: full replace, session keys reused, patch in place
- A sync update **replaces the whole event**: anything omitted or `null`/`[]` is reset (except `Color: null`, ignored, and an absent `Notifications`, kept) [claim: proton-cal live] (`api.md:225-248`); [claim: carbonate live] (`ARCHITECTURE.md:487-489`).
- Reuse the session keys of the row: decrypt `SharedKeyPacket` / `CalendarKeyPacket` with the calendar key, re-encrypt the new plaintext under the **same** keys, send no key packets [claim: proton-cal live] (`api.md:257-267`; `proton-cal/pkg/event/write.go:166-237`); minting fresh keys without key packets makes everything undecryptable [claim: cal-gateway] (`update.go:24-31`). Whether the server accepts **new** key packets on an update is unverified (carbonate/caloxide send one only when the row had none) [unknown].
- Patch in place, never rebuild from a model: proton-cal and cal-gateway decrypt the current parts and change only the edited properties, keeping every other line byte for byte (ORGANIZER, X-*, conferencing, COMMENT) (`proton-cal/docs/api.md:231-236`; `cal-gateway/internal/proton/update.go:33-45`; `FEATURE-MATRIX.md:186-189`). Both refuse to reseal an event any part of which did not decrypt (`proton-cal/pkg/event/write.go:138-140`; `cal-gateway/.../update.go:45-47,66-70` → 403).
- carbonate and caloxide rebuild all parts from the client's VEVENT through go-ical (re-encoded, not byte-exact) (`carbonate/.../write.go:406-487`; `caloxide/.../calendar.go:722-745,965-1160`).

### B.6 What our code already has to invert
- `cards.Open` (`internal/proton/cards/cards.go:121-194`) gives parts by kind with `Signed/Verified/Own/Author`, opens session keys per row (`opener.sharedKey/calendarKey`, `:217-267`) — the write side needs those session keys back (today private) and the primary calendar key.
- `ical.MergeEvent` (`internal/ical/merge.go:42-86`) is line-level with source order kept: each merged line can carry its source part for a per-line diff [inferred].
- Render adds things that must **not** be written back: `COLOR` (`ApplyRow`, `merge.go:192-217`), VALARMs regenerated from `Notifications` (`ApplyNotifications` replaces every VALARM, `merge.go:164-190`), PARTSTAT injected and `X-PM-TOKEN` dropped on ATTENDEE (`merge.go:192-275`), `X-PCB-INTEGRITY` (`merge.go:279-289`), `X-PCB-PLACEHOLDER` (`internal/service/render.go:457-494`), VCALENDAR wrapper and PRODID (`ical/object.go`).
- `api.Client.Put` exists (`internal/proton/api/client.go:109-112`).

---

## C. Plaintext row fields on write (Q3)
- No writer sends `StartTime`, `EndTime`, `StartTimezone`, `EndTimezone`, `FullDay`, `RRule`, `RecurrenceID` or `Exdates`; the rows read back correct (A.2 bodies). The server **parses the shared signed part**: times from DTSTART/DTEND, `StartTimezone` from the TZID parameter, `FullDay:1` and midnight-UTC instants for `VALUE=DATE`, `RRule` verbatim, `Exdates` and `RecurrenceID` as unix seconds [claim: proton-cal live] (`crypto.md:184-186,273-286`; `api.md:363-367`).
- `IsOrganizer` and `Permissions` are request fields (A.2). `Color`, `Notifications`, `Attendees` are request fields stored as given.
- Consequence: the signed part is the only place times live; a TZID the server cannot resolve has no fallback [inferred] (§I).

## D. Notifications (Q4)
- Source of truth on write and read: the row field `Notifications` `[{Type,Trigger}]`, `Type` 0 email, 1 device; `Trigger` a duration relative to start (`-PT15M`) [claim: proton-cal live] (`crypto.md:209-226`). Live events carry **no** `PersonalEvents` [claim: proton-cal live] (`api.md:250-255`). All four writers ignore the personal part.
- Tri-state: `null` = calendar defaults (`DefaultPartDayNotifications` / `DefaultFullDayNotifications`), `[]` = none, array = custom [claim: proton-cal live].
- VALARM → notification [claim: cal-gateway, rules "ported from the web client"] (`cal-gateway/internal/proton/notifications.go:9-31,159-220`): ACTION EMAIL → 0, DISPLAY/AUDIO → 1, others dropped; TRIGGER relative to START only (absolute `VALUE=DATE-TIME` and `RELATED=END` dropped); after-start triggers dropped (all-day: positive < 24 h accepted); per-unit bounds weeks ≤ 999, days ≤ 6999, hours ≤ 999, minutes ≤ 9999, seconds forced 0; at most 10 (`:45`). carbonate maps every non-EMAIL to device and keeps all (`carbonate/internal/calendar/alarm.go:26-63`).
- Tri-state on update (cal-gateway's rule, `notifications.go:95-112`): client sent none + row null/[] → keep row value; client sent none + row array → `[]`; same set by meaning (type + signed offset, order and text form ignored) → **row JSON verbatim**; else new array. carbonate always sends an array (so "defaults" becomes explicit) (`write.go:63-66`); caloxide sends `null` when none (so removing every alarm brings the defaults back) (`calendar.go:1029-1047`) — both lossy [inferred].
- **Our render serves the defaults as VALARMs when the row is `null`** (`ical.Alarms`, `merge.go:117-151`); cal-gateway does not. So we must compare the incoming set with the **effective** set we rendered (defaults when `null`) and keep `null` when equal [inferred, important]: otherwise the first edit in TB pins the defaults and a later change of the calendar defaults no longer reaches the event.
- Alarm-only edits (dismiss/snooze in TB) produce a full PUT (E08 §6.2); see §F for X-MOZ-LASTACK.

## E. Recurrence (Q5, E12)

### E.1 What Proton accepts [conflict on enforcement]
- proton-cal: "server-enforced RRULE limits (mirroring the web client's getIsRruleSupported)": FREQ ∈ {DAILY, WEEKLY, MONTHLY, YEARLY}; COUNT ≤ 49; UNTIL ≤ 2037-12-31; COUNT and UNTIL exclusive; MONTHLY one BYDAY+BYSETPOS (setpos ∈ {-1,1,2,3,4}) or one BYMONTHDAY; YEARLY BYMONTHDAY needs BYMONTH; DTSTART must be the first occurrence (`api.md:386-392`).
- cal-gateway: "the Proton server validates **nothing** (RRULE …); validation lives client-side", so an accepted PUT can still break the Proton app; INTERVAL max per FREQ "999/4999/-/99"; it refuses COUNT > 49 and UNTIL > 2037 itself (`FEATURE-MATRIX.md:9-14,34-44,214-217`; `internal/caldav/ics.go:571-597`).
- UNTIL form: timed = end of that day in the start zone, in UTC; all-day = DATE [claim: proton-cal live] (`api.md:393-396`). RDATE, EXRULE, RANGE=THISANDFUTURE: no Proton equivalent [claim: cal-gateway] (`FEATURE-MATRIX.md:68`).
- Either way the bridge must gatekeep: refusing at the bridge is the only way to keep the Proton web app consistent [inferred].

### E.2 Exceptions
- Edit one occurrence = a **new row**, same UID, `RECURRENCE-ID` (original start) in its shared signed part, no RRULE/EXDATE, SEQUENCE ≥ master's (2001 otherwise); may carry its own key packets [claim: proton-cal live] (`api.md:372-380`; `proton-cal/pkg/event/smart.go:168-252`). RECURRENCE-ID with RRULE → 2011 [claim: carbonate live].
- Delete one occurrence = EXDATE on the master (full master update) and, if the occurrence had an exception row, delete that row [claim: proton-cal live] (`api.md:370-371`; `smart.go:92-115`).
- Delete a master **orphans** its exceptions: a series delete must batch master + all same-UID rows in **one** sync call [claim: proton-cal live] (`api.md:381-382`); [claim: cal-gateway live] (`FEATURE-MATRIX.md:66`; `internal/proton/write.go:252-296`).
- Changing master times/RRULE leaves exceptions describing occurrences that no longer exist: proton-cal purges them (`smart.go:141-166`); cal-gateway mirrors the payload (rows absent from the PUT deleted, present updated, new ones created) [claim: cal-gateway live, "op. 4, 6"] (`FEATURE-MATRIX.md:59-80`; `internal/caldav/backend.go:636-735`).
- cal-gateway also keeps past EXDATEs the client dropped (Apple trims history) (`FEATURE-MATRIX.md:130-140`); TB keeps the full item, so not needed for TB [inferred].

### E.3 Thunderbird → Proton mapping
- TB sends master + every override in one resource (E08 §6.1, §6.4). "This occurrence only" = PUT of the whole resource with a VEVENT carrying `RECURRENCE-ID` (no RANGE in E08) → create/update the exception row matched by **UID + RecurrenceID** (carbonate's lesson, `ARCHITECTURE.md:455-459`; `write.go:336-349`). RECURRENCE-ID (TZID local or DATE) → unix of the original occurrence start, compared with the row's `RecurrenceID` column [inferred].
- Delete one occurrence in TB = PUT of the master with a new EXDATE (override VEVENT gone if there was one) [inferred from E08 §4.1, §6.4] → master update + delete of that exception row.
- "This and all future" in TB: not covered by E08; I believe TB truncates the old series (UNTIL) and creates a new item with a new UID rather than sending RANGE=THISANDFUTURE [inferred, check in TB source]. Then it is two ordinary PUTs. Refuse RANGE anyway (SPEC §8.4).
- Orphan resources (exceptions without master, served by M3 with TB faking a master, E08 §6.4): a PUT without master VEVENT on such a UID updates only the exception rows; never create an exception without a master row (cal-gateway `backend.go:695-725`).

## F. Unknown properties (Q6, E13)
- Proton (web client) puts every VEVENT property outside the table into the **shared encrypted** part ("rest") [claim: proton-cal, read from the web client] (`crypto.md:188-197`). The server cannot read it, so it keeps it byte for byte [inferred, high]. carbonate/caloxide do the same (`carbonate/.../write.go:416-421,943-958`; `caloxide/.../calendar.go:736-743`) and say it round-trips live [claim]. cal-gateway strips URL, CATEGORIES, ATTACH, GEO, CLASS, X-APPLE-* on create and only preserves what was already in the stored parts on update (`FEATURE-MATRIX.md:157-167`; `update.go:33-45`).
- Whether the **Proton web app** keeps "rest" lines when the user later edits the event there: unknown [E13 live].
- Thunderbird: X-MOZ-LASTACK lives at VEVENT **and** VALARM level, X-MOZ-SNOOZE-TIME(-<recid>) at VEVENT level, X-MOZ-GENERATION at VEVENT level (E08 §6.2). VEVENT-level ones go to the shared encrypted part and survive. **VALARM-level ones cannot**: alarms go to `Notifications`, and our render rebuilds every VALARM (`merge.go:164-190`). Whether TB's alarm service needs the VALARM-level LASTACK or only the item-level one decides whether dismissed alarms re-fire [unknown; HUMAN-CHECKS step 6.3 already covers it].
- Exception VEVENTs: their X-MOZ-* go into their own row's shared encrypted part [inferred].
- Must strip on input: `X-PCB-INTEGRITY`, `X-PCB-PLACEHOLDER`, `X-PM-SESSION-KEY`; must not route `COLOR`, VALARM, VTIMEZONE, PRODID/VERSION/CALSCALE/METHOD into parts.

## G. Member passphrase signature and signing keys (Q7, E10, R17)
- Bootstrap `Passphrase.MemberPassphrases[] = {MemberID, Passphrase (armored PGP message to an address key), Signature (armored detached)}` (`go-proton-api/calendar_types.go:94-117`). We do not decode `Signature` (`internal/proton/calendar/unlock.go:60,75-77`); our fake serves `""` (`protontest/calendars.go:445-451`).
- go-proton-api verifies it: decrypt with the address keyring, then `VerifyDetached(decrypted, sig)` with **the same** address keyring [source] (`calendar_types.go:119-140`). carbonate uses that helper on its own calendars and works live [claim: carbonate live] (`carbonate/internal/proton/client.go:510-518`). proton-cal creates calendars by signing the passphrase **plaintext** with the member's address key (`crypto.md:150-156`). cal-gateway and caloxide skip it because it fails on calendars shared by another member (signed by the inviter's key) (`cal-gateway/internal/proton/crypto.go:105-112`; `caloxide/.../calendar.go:237-245`, commented out).
- Why it matters before encrypting: the server can encrypt a passphrase it chose to our **public** address key and serve a calendar key locked with it; we would unlock it and encrypt events to a key the server owns. Only the signature by our address **private** key binds the passphrase to us [inferred]. Rule for M4: write only to calendars whose member-passphrase signature verifies with one of **our** address keys (owned calendars); shared-in calendars stay read-only until a public-key lookup exists [call].
- Session key reuse has the same issue: a substituted `SharedKeyPacket` (a key the server knows, encrypted to our calendar public key) would make our update readable by the server. The server cannot forge a part encrypted under that key **and** signed by us, so reuse a session key only when the row's encrypted part decrypts with it and its signature verifies with our address key (`Part.Verified`, `cards.go:181-188`) [inferred]; otherwise refuse (or, if the server accepts new key packets on update — unknown — mint fresh keys).
- Which key signs which part: every part by the member's address key (B.3). E10's open question (SPEC §5.3) — whether Proton itself ever signs our parts with another key — is unresolved; M3 counts `unverified` at serve time (HUMAN-CHECKS step 6).

## H. Member permissions bitmask (Q8, E14)
- caloxide's constants (no stated origin): `Availability=1, Read=2, ReadMemberList=4, Write=8, Admin=16, Owner=32` (`caloxide/protonmail/calendar.go:83-92`). go-proton-api has the field with "TODO: What is this?" (`calendar_types.go:81-91`). Our fake serves `127` for our member and `1` for a foreign one (`protontest/calendars.go:423-443`), event rows `Permissions:127` (`protontest/events.go:392`). SevenOfNine treats `>0` as writable (`internal/provider/proton.go:80-84`): wrong for an availability-only share [inferred].
- Where: `GET /calendar/v1` `Calendars[].Members[].Permissions` (we already read it into `calendar.Calendar.Permissions`, `internal/proton/calendar/list.go:232,264-268`) and bootstrap `Members[].Permissions` (not decoded, `unlock.go:58-69`).
- Proposal: writable ⇔ `Type == Normal` ∧ `Permissions & 8 != 0` ∧ passphrase signature verified by our key (G) [inferred]; bits for owner/edit/view shares: E14 live.

## I. VTIMEZONE and TZID on write (Q9)
- Proton accepts `DTSTART;TZID=<IANA>` with no VTIMEZONE and derives `StartTimezone` from it [claim: proton-cal live] (`crypto.md:273-286`). Non-IANA TZIDs: "never stored / rejected" [claim: cal-gateway] (`FEATURE-MATRIX.md:166`); cal-gateway maps a present but unknown TZID to UTC (`internal/caldav/ics.go:618-650`). Floating times are refused by Proton (`UNEXPECTED_FLOATING_TIME`) [claim: cal-gateway, from the web client] (`FEATURE-MATRIX.md:165`).
- TB always adds VTIMEZONE for each non-UTC TZID, generated from its ICU database, so the TZIDs are IANA names (E08 §6.1, §6.3). VTIMEZONE is a VCALENDAR child, not part of the VEVENT, so splitting the VEVENT drops it naturally.
- Proposal: accept a TZID only if Go's tz database (embed `time/tzdata`) resolves it; map TB's legacy `/mozilla.org/<date>/<Area/City>` form to `Area/City`; refuse floating date-times and unknown TZIDs with 403 rather than shifting times [call].

## J. Thunderbird write flow → Proton (Q10)

| TB request (E08 §4) | Bridge action | Answer |
|---|---|---|
| `PUT <name>.ics`, `If-None-Match: *`, new UID | check UID free (cache **and** Proton `GET …/events?UID=`), create master (+ one create per override VEVENT) | **201** |
| same, but the name or the UID already exists | — | **412** if the href exists; **409 + `<C:no-uid-conflict><D:href>existing</D:href></C:no-uid-conflict>`** if the UID lives at another href (TB treats both as `UID_CONFLICT`, E08 §4.2) |
| `PUT` with `If-Match: <etag>` | refresh the UID's rows from Proton, recompute the fold ETag, compare exact string; then plan master update / exception create-update-delete | **204**; **412** on mismatch (TB opens its conflict dialog, E08 §4.2) |
| `PUT` without precondition (forced) | same, no check | 201 or 204 |
| `DELETE` with `If-Match` | refresh, compare, one sync call deleting every row of the UID | **204**; **404** if no row; **412** on mismatch (TB then HEADs; 404 ends it, E08 §4.3) |
| any write to a placeholder, an undecryptable or unverified row, a read-only calendar | refuse, never touch Proton | **403** (`need-privileges` for read-only; a pcb precondition element otherwise; OPEN.md "Parked by M3") |
| Proton 429/5xx/network | no retry of the write | **503 + Retry-After** (temporary for TB) |
| Proton per-entry validation code | | **403** with a pcb precondition element (deterministic) |

Names and UIDs:
- TB names a new event `<UID with every char outside [A-Za-z0-9_.-] → "_">.ics` (E08 §4.4); our canonical name is the UID itself over `[A-Za-z0-9._@-]` or `~base64url` (`internal/caldav/server.go:338-381`). They differ for any UID with `@` or other characters (invitations accepted from mail, imports). The server must accept any name (E08 §4.4, MUST 9) and TB learns the location from its own request URI, not from a `Location` header (E08 §4.1).
- So a persistent **alias** (calendar, client name → UID) is needed, used to resolve GET/HEAD/DELETE/PUT and to serve that UID at the client's href in listings [inferred]; carbonate keeps such a map (in memory, `ARCHITECTURE.md:88-90`; `internal/caldav/backend.go:51-60,646-649`), cal-gateway persists one (`FEATURE-MATRIX.md:104-110`). What TB does if the same UID shows up under a second href in a sync is not settled by E08 [live check].
- A PUT whose body UID differs from the UID already at that href: 409 `no-uid-conflict` [inferred from RFC 4791 §5.3.2].

After the write:
- TB ignores the response ETag and multigets the href at once (E08 §4.1, §4.5). Since we transform the data (VALARMs, COLOR, VTIMEZONE dropped), send no `ETag` header [inferred, RFC 7231 §7.2].
- **Read-your-writes**: the Proton event loop lags (A.5), so the write must patch the cache synchronously before answering: GET each written row by ID (or the UID group) and upsert it without moving the feed cursor; a later feed read of the same row then finds the same row ETag and changes nothing (SPEC §8.1 "a re-read row whose ETag is unchanged changes nothing") [inferred]. Prefer a GET over the echoed row: updates may not echo, and an echo with a different field set would change the ETag twice [inferred]. The LRU is keyed by the fold ETag, so it invalidates itself.
- Ambiguous failure (timeout after the request left): re-read by UID; if the change landed, answer success, else 503 [inferred].
- Cached TB calendars replay failed writes silently forever (E08 §3.8): every 403 leaves a TB-only event. Prefer to accept-and-normalise where nothing is lost; refuse only where writing would lose or corrupt data [call].

## K. How each reference bridge writes (Q11)

| | Architecture | Preconditions | Read-after-write | Recurrence | Pitfalls they document |
|---|---|---|---|---|---|
| proton-cal (CLI/MCP, not CalDAV) | `pkg/event/write.go`: create builds four fragments from a model; update = GET row, decrypt, **patch in place**, reseal with the row's session keys | n/a | n/a | smart update/delete: EXDATE, exception rows, SEQUENCE ≥ master, batch series delete (`smart.go`) | full-replace semantics, tri-state Notifications, no colour reset, web events without calendar card (`api.md:225-267`) [live] |
| carbonate (go-webdav) | `internal/calendar/write.go`: rebuild every part from the client VEVENT (go-ical), unknown props → shared encrypted, VALARM → Notifications, attendees + iMIP mail | **ignored** (`internal/caldav/backend.go:615-656` never reads `opts`) | drops its cache, re-reads from Proton (`backend.go:651-656`) | writes master then each override, matched by UID+RecurrenceID; deletes every same-UID row one call each (`write.go:138-162,230-275`) | SEQUENCE strictly increasing / bare / sign with member address / organizer required (`ARCHITECTURE.md:603-622`) [live] |
| caloxide (forked go-webdav) | `protonmail/calendar.go:965-1240`: rebuild parts (go-ical), forces SEQUENCE ≥ old+1, Notifications nil when none | **ignored** (`caldav/caldav.go:538` TODO) | stores **the client's** calendar in its cache, ETag `LastEditTime+ID` (`caldav.go:612-620`): serves what TB sent, not what Proton stored | **refuses** (403) any PUT touching a series with exceptions (`caldav.go:533-574`; README "Series with edited occurrences are read-only") | create vs update by GET returning 2061 (`calendar.go:1171-1185`) |
| cal-gateway (go-webdav + interceptors, Apple) | `internal/proton/{write,update,notifications}.go`: create from a model, update = patch in place, refuses on undecryptable (`ErrEventDegraded` → 403); `internal/caldav/backend.go` mirrors the full PUT onto master + exception rows | **authoritative**: re-reads the UID from Proton, 412 on stale If-Match, accepts the served ETag too after a cache drift bug (`backend.go:390-500`) | re-reads the group to answer (`backend.go:752-770`) | op. 1-6 table, THISANDFUTURE refused, RRULE limits, past-EXDATE guard, TENTATIVE stripped, floating refused (`FEATURE-MATRIX.md`) | "a refusal must be an honest 403, never a 2xx no-op" (Apple loops on "Error 2"); 201 Location alias (`FEATURE-MATRIX.md:9-14,104-110`) [live for ops 3, 4, invitations] |

---

## L. Recommended M4 design (Q12)

### L.1 Package boundaries (consistent with SPEC §4 and M3)
- **`internal/ical`** (C0, pure): `Split(vevent) → per-kind line lists` with the B.1 table (rest → shared encrypted, EXDATE → both signed parts); a per-line **diff/patch** against the merged lines of the current parts so untouched lines keep their part and bytes; input normalisation (strip X-PCB-*, X-PM-SESSION-KEY, COLOR, VALARM, VCALENDAR-level props, VTIMEZONE); VALARM → `{Type,Trigger}` per §D; RRULE/TZID/floating/STATUS/RANGE gatekeeping returning typed refusals.
- **`internal/proton/cards`** (C1 codec): add `Seal` beside `Open`: fragments + signer ring + calendar primary key + optional reused session keys → wire `Event` body (`Type`, `Data`, `Signature`, key packets on create only). Expose the opened session keys and per-part verification to the writer. Round-trip property test: `Open(Seal(x)) == x`, signatures verify, key packets open with the calendar key only.
- **`internal/proton/calendar`**: decode `Keys[].Flags` (primary), `MemberPassphrases[].Signature` (verify, R17), bootstrap `Members[].Permissions`; return the member address's signer ring; `Writable()`.
- **`internal/proton/events`**: `ListByUID` (`GET …/events?UID=`, paged by `More`), `Sync(ctx, c, calID, memberID, entries) → per-entry results` with typed errors (2001 sequence, 2001 signature, 2000, 2011, 2501).
- **`internal/store/cache`**: `ApplyWrite(calID, upserts, gone)` (seq bump, tombstones with UID, cursor untouched); alias table (calendar, name → UID); schema bump.
- **`internal/service`**: `PutObject(ctx, calID, name, body, cond) (created bool, err)` and `DeleteObject(ctx, calID, name, cond)`; errors as typed causes the adapter maps (precondition failed, UID conflict with href, not writable, refused with reason, unavailable). Flow: resolve name/alias → writable? → refresh the UID group from Proton into the cache → precondition on the fresh fold ETag → refuse placeholder/unverified/foreign rows (service state, not the `X-PCB-PLACEHOLDER` property, OPEN.md) → plan (create/update/delete per row, SEQUENCE rule, Notifications tri-state, re-sent Color/Attendees) → seal → `events.Sync` → GET rows → `ApplyWrite`. Serialise writes with the syncer's per-calendar apply (a stale feed read must not overwrite a fresher write-through) [inferred].
- **`internal/caldav`**: PUT/DELETE handlers (Content-Type `text/calendar`, 8 MiB cap already, If-Match exact string, If-None-Match `*`), status mapping of §J, privileges from `Writable()` (M3-R15), `supported-calendar-component-set` VEVENT only (already).
- **`internal/proton/protontest`**: implement `PUT /calendar/v1/{cal}/events/sync` and `GET …/events?UID=` with Proton's rules as researched: parse the shared signed part into the columns (incl. `Exdates`, `RecurrenceID`, `StartTimezone`), **verify every signature with the member's address key** (2001), require key packets on create and refuse/ignore them on update, decrypt with the calendar key (catches wrong recipients), SEQUENCE ≥ master for exceptions (2001), RRULE+RECURRENCE-ID (2011), STATUS TENTATIVE (2000), palette Color (2011), tri-state Notifications with "absent = keep", full replace of everything else, per-entry codes in a 1001 envelope, `Responses: []` for deletes, feed entries; switches for each [conflict]/[unknown] (silent drop on unchanged SEQUENCE, UID uniqueness, new key packets on update, mixed batches) so tests cover both hypotheses. Fix the fake's EXDATE/DURATION/VALARM routing (B.1).

### L.2 Rules to settle in the plan [call]
1. SEQUENCE: keep `max(incoming, stored)`; bump to stored+1 when the shared signed content changes (times, RRULE, EXDATE, RECURRENCE-ID) and the client did not; exceptions written with SEQUENCE ≥ master. Do **not** bump on alarm/text-only edits (TB dismissals would otherwise churn SEQUENCE) until the [conflict] is tested live.
2. Attendees: pass the stored attendees part and `Attendees` rows through verbatim; refuse a PUT that adds/removes ATTENDEE or adds ORGANIZER+ATTENDEE on create (SPEC §8.4).
3. STATUS:TENTATIVE: refuse or map to CONFIRMED (cal-gateway strips) — TB offers "Tentative".
4. Notifications: unsupported VALARMs dropped (like Proton's import) rather than refusing the PUT.
5. Colour: re-send the row's `Color` verbatim; ignore client COLOR.
6. Alias naming for non-canonical client names (J).

### L.3 Risks, ranked
1. **Silent data loss on update** (full-replace endpoint): Notifications tri-state, Color, Attendees rows, untouched card lines, other members' personal parts (unknown fate). Mitigation: patch in place, re-send row fields, refuse on any undecryptable/unverified part, golden tests per field.
2. **Malformed events in Proton** (wrong routing/signing/recipient): events unreadable in the web app while our own reader is happy (carbonate's contacts story, `ARCHITECTURE.md:370-386`). Mitigation: fake verifies like the server; live check in the web app.
3. **Recurrence mapping**: exception rows, SEQUENCE ≥ master, orphans, non-atomic multi-row writes. Mitigation: plan computed and validated before the first call; order master → exceptions; after any failure re-read the group and answer 5xx/412 so TB re-syncs.
4. **Key trust** (R17 passphrase signature, session-key reuse on a substituted key packet). Mitigation: §G rules.
5. **Stale overwrite / duplicates**: If-Match against a stale cache, retried creates, UID collisions. Mitigation: authoritative UID refresh before preconditions, no write retries, UID uniqueness enforced by the bridge.
6. **SEQUENCE semantics** [conflict]: wrong choice = silently dropped edits or invalidated exceptions.
7. **Refusals vs TB cache** (E08 §3.8): a 403 on a cached calendar diverges silently. Keep refusals few and logged with counts.
8. **Alarm state**: VALARM-level X-MOZ-LASTACK lost; dismissals cost a full Proton write each.
9. **Hrefs**: non-canonical names, UID shown under two hrefs.
10. **readOnly persistence** in TB after M3 subscriptions (HUMAN-CHECKS step 6.4).

### L.4 Facts that need a live account (HUMAN-CHECKS, after the CAPTCHA)
1. Create/update/delete round trip seen correctly in the Proton web app (and our reader), with binary-mode signatures and PKESK to the primary calendar key only.
2. SEQUENCE: text-only edit without bump — kept or silently dropped? Time edit without bump?
3. New key packets on an update: accepted, ignored or refused?
4. Mixed create/update/delete entries in one batch; per-entry vs all-or-nothing.
5. Second master with an existing UID: refused (which code) or stored?
6. RRULE limits enforced by the server or not (COUNT 50, UNTIL 2038, BYSETPOS 5, FREQ=HOURLY, INTERVAL bounds) — E12.
7. Unknown properties (X-MOZ-*, CLASS, CATEGORIES, URL, ATTACH) kept in the shared encrypted part, and after an edit in the web app — E13.
8. STATUS TENTATIVE code 2000; floating time; non-IANA TZID; DURATION instead of DTEND.
9. Member `Permissions` value on an owned calendar and on view/edit shares — E14; passphrase `Signature` verifies with our address key — E10/R17.
10. What happens to `PersonalEvents` of a row on a sync update; whether `Notifications` absent on update keeps the old value.
11. Delay between a write and its feed entry; whether the echoed row equals a later GET (ETag stability).
12. Thunderbird: dismissed alarm does not re-fire after the PUT round trip (VEVENT-level LASTACK only); PUT at a `_`-sanitised name; "this and future" request shape; 409 no-uid-conflict recovery.

## What stays unknown without a live account
- Server validation surface (RRULE, properties, UID uniqueness, size limits) and every error code above except those four bridges observed.
- The SEQUENCE rule (carbonate/caloxide vs proton-cal), new key packets on update, batch atomicity, `Overwrite:1`.
- Member permission bits; whether our owned calendars' passphrase signatures verify; E10's signer-per-part question.
- Proton web app behaviour on our events and on "rest" properties after its own edits.
- Thunderbird's live behaviour on aliases, alarms and conflicts (E08 is source-only).
