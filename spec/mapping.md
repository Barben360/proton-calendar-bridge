# Property mapping: CalDAV PUT → Proton event rows (M4)

What the bridge does with each part of a PUT body, as built in M4. SPEC §8.4 points here. Code:
- `internal/ical/validate.go` (`ParseWrite`: refusals and normalisation);
- `internal/ical/write.go` (`Route`, `Patch`: routing into card parts);
- `internal/ical/alarms_write.go` (VALARM → `Notifications`);
- `internal/service/writeplan.go` (`Plan`: rows, row fields, SEQUENCE, attendees, group refusals);
- `internal/service/write.go` (the executor: refresh, precondition, one Sync call, read-back).

The routing table comes from research B.1 (four independent bridges agree; proton-cal live). Every other rule cites its ruling (M4-R#, `.superpowers/sdd/2026-11-11-m4-caldav-write/progress.md`). Proton's own behaviour on these writes is **not yet observed live**: see E10, E12, E13, E14 and the M4 live session (`HUMAN-CHECKS.md` §6).

Legend:

| Outcome | Meaning |
|---|---|
| **routed** | written into the named card part(s) |
| **transformed** | rewritten before routing (same meaning, Proton's form) |
| **stripped** | dropped; the PUT goes on (accepted and normalised; noted in a debug log line `write normalised`, which `pcb serve`'s default log level does not print) |
| **refused** | 403 `<pcb:CODE xmlns:pcb="urn:pcb"/>` with a short `pcb:reason` of the bridge's own; nothing is sent to Proton |

Part names: **SS** shared signed (clear text, detached signature), **SE** shared encrypted, **CS** calendar signed, **CE** calendar encrypted, **AT** attendees (encrypted under the shared session key). Every part is signed by the primary key of our member's address, in binary mode (`cards.Seal`).

## 1. The request

| Item | Outcome |
|---|---|
| `Content-Type` other than `text/calendar` (parameters ignored) | 415 |
| Body over 8 MiB | 413 |
| Not valid UTF-8, not iCalendar, or not exactly one top-level VCALENDAR | refused `pcb:bad-data` |
| Resource name: empty, `.`, `..`, a `/`, a control character, invalid UTF-8; or over 255 bytes when it is about to become an alias | refused `pcb:bad-resource-name` |
| Resource name that is another UID's alternative name (`~~<base64url>.ics`, M4-R38) | refused `pcb:bad-resource-name` |
| `If-None-Match` naming ETags | evaluated (weak comparison; a missing object passes); an empty list is 412 |
| `If-Match` | `*`: the object must exist; else a comma-separated list (several headers are one list), one of which must be the object's ETag by strong comparison: a weak `W/"…"` tag never matches (RFC 9110 §13.1.1); otherwise 412; a present but blank `If-Match` (empty, spaces or commas only) is 412 too, never an unconditional write |

## 2. VCALENDAR level

| Item | Outcome |
|---|---|
| `VERSION`, `PRODID`, `CALSCALE`, `METHOD`, any other VCALENDAR property (`X-WR-CALNAME`…) | stripped: never read, never routed |
| `VTIMEZONE` | stripped: a TZID is resolved by name with Go's zone database (embedded `time/tzdata`) |
| `VTODO`, `VJOURNAL`, `VFREEBUSY` or any other component | refused `pcb:bad-data` |
| No VEVENT | refused `pcb:bad-data` |
| More than 100 VEVENTs | refused `pcb:too-many-events` |
| VEVENTs of more than one UID | refused `pcb:not-one-uid` |
| A VEVENT without exactly one non-empty UID | refused `pcb:bad-data` |
| Two VEVENTs without RECURRENCE-ID, or two with the same one | refused `pcb:bad-data` |
| A RECURRENCE-ID of another value type (DATE vs DATE-TIME) than the master's DTSTART | refused `pcb:bad-data` |
| A VEVENT with `X-MOZ-FAKED-MASTER:1` (Thunderbird's stand-in master for an orphan exception, E08 §6.4) | stripped, whole VEVENT (M4-R11); the rest is written as exceptions only |

## 3. VEVENT properties

Single-valued properties (`UID`, `DTSTAMP`, `DTSTART`, `DTEND`, `DURATION`, `RRULE`, `RECURRENCE-ID`, `SEQUENCE`, `SUMMARY`, `DESCRIPTION`, `LOCATION`, `STATUS`, `CLASS`, `TRANSP`, `ORGANIZER`, `URL`, `CREATED`, `LAST-MODIFIED`, `GEO`, `PRIORITY`, `COLOR`) given twice in one VEVENT: refused `pcb:bad-data`. A property name outside the RFC 5545 name characters, or a control character other than HTAB in a parameter or value: refused `pcb:bad-data`.

| Property | Outcome | Part(s) | Notes |
|---|---|---|---|
| `UID` | routed | every part | required; the object's identity |
| `DTSTAMP` | routed | every part | absent → the write time, UTC (M4-R8) |
| `DTSTART` | routed | SS | required (`pcb:bad-data` without); floating date-time → `pcb:floating-time`; a TZID Go cannot resolve → `pcb:unknown-tzid`; `/mozilla.org/<date>/<Area/City>` → `Area/City` (transformed); a DATE with a TZID, a UTC time with a TZID, `VALUE` other than DATE or DATE-TIME, malformed → `pcb:bad-data` |
| `DTEND` | routed | SS | same time checks; of DTSTART's value type and not before it, else `pcb:bad-data`; absent (and no DURATION) → all-day: the next day, timed: DTSTART itself (M4-R8) |
| `DURATION` | transformed | → `DTEND`, SS | the DTEND it means, written in DTSTART's form (weeks and days nominal, on the local calendar; hours, minutes, seconds exact); negative, malformed, or hours on an all-day event → `pcb:bad-data`; with a DTEND → `pcb:bad-data` |
| `RECURRENCE-ID` | routed | SS | marks an exception row; `RANGE=…` (this and future) → `pcb:recurrence-range` |
| `RRULE` | routed (maybe transformed) | SS | the supported set in §4; an exception with an RRULE → `pcb:unsupported-recurrence` |
| `EXDATE` | routed | SS **and** CS | every value checked like DTSTART; on an exception → `pcb:unsupported-recurrence` |
| `RDATE` | refused | | `pcb:rdate`: no Proton equivalent |
| `EXRULE` | refused | | `pcb:exrule`: no Proton equivalent |
| `SEQUENCE` | routed (maybe transformed) | SS | 1 to 9 digits, else `pcb:bad-data`; written bare (`SEQUENCE:n`, a parameter is a Proton 500, research A.4); value rules in §6 |
| `ORGANIZER` | routed | SS | a new exception of a series with attendees takes the master's line; another address there → `pcb:attendee-change` (§7) |
| `X-PM-CONFERENCE-ID` | routed | SS | M4-R6 |
| `STATUS` | routed | CS | `STATUS:TENTATIVE` stripped (Proton refuses it with 2000, research A.4); CONFIRMED and CANCELLED kept |
| `TRANSP` | routed | CS | |
| `COMMENT` | routed | CE | needs the row's calendar session key on update: a row created without a CE part (no `CalendarKeyPacket`) refuses a first COMMENT with `pcb:unsupported-change` (M4-R13) |
| `ATTENDEE` | routed | AT | `X-PM-TOKEN` stripped from the incoming line; the stored line (with its token) is kept; adding or removing an attendee → `pcb:attendee-change`; parameter edits (ROLE, CN, PARTSTAT, RSVP) ignored, stored line kept (M4-R9); any attendee on a new event → `pcb:attendee-change`; rules for new exceptions in §7 |
| `SUMMARY`, `DESCRIPTION`, `LOCATION`, `CREATED`, `LAST-MODIFIED`, `CLASS`, `URL`, `GEO`, `PRIORITY`, `CATEGORIES`, `ATTACH`, `CONTACT`, `RESOURCES`, `RELATED-TO`, `REQUEST-STATUS` | routed | SE | the "rest" rule: every VEVENT property not named elsewhere in this table goes to SE (research B.1, F) |
| `X-MOZ-*` at VEVENT level (`X-MOZ-GENERATION`, `X-MOZ-LASTACK`, `X-MOZ-SNOOZE-TIME*`, `X-MOZ-SEND-INVITATIONS`…) | routed | SE | kept byte for byte on later pcb edits; whether the Proton web app keeps them after its own edit: E13 |
| any other `X-` property, any unknown IANA property | routed | SE | as above |
| `COLOR` | stripped | | the client's colour is ignored: a new event sends the row `Color` null, a new exception its master's, an update re-sends the row's (§5); a `COLOR` line already stored in a part (another client's) is kept as it is (M4-R7) |
| `X-PCB-INTEGRITY`, `X-PCB-PLACEHOLDER`, any `X-PCB-*` | stripped | | pcb's own markers; also removed from stored parts when they are patched (M4-R7). A placeholder is refused by service state, never by this property (§8) |
| `X-PM-SESSION-KEY` | stripped | | Proton key material; also removed from stored parts |
| `VERSION`, `PRODID`, `CALSCALE`, `METHOD` inside a VEVENT | stripped | | |
| `VALARM` | transformed | → row `Notifications` | never in a part: §5 |
| any other nested component in a VEVENT | refused | | `pcb:bad-data` |

The served object afterwards is what Proton stored, merged again (Thunderbird re-reads it at once, E08 §4.1). So a client sees: VALARMs regenerated from `Notifications`, `DURATION` as `DTEND`, rewritten RRULEs, no `STATUS:TENTATIVE`, no VTIMEZONE, `COLOR` from the row, `PARTSTAT` from the row's attendee statuses.

## 4. RRULE (rulings M4-R5, M4-R10, M4-R12; research E.1)

Accepted, per FREQ (anything else → `pcb:unsupported-recurrence`; a malformed rule, a part given twice, no FREQ, INTERVAL or COUNT not a positive number, an UNTIL that is neither a DATE nor a UTC date-time → `pcb:bad-data`):

| FREQ | Parts allowed besides FREQ, INTERVAL, COUNT or UNTIL, WKST (a weekday) | INTERVAL at most |
|---|---|---|
| DAILY | none | 999 |
| WEEKLY | `BYDAY` of plain weekdays (`MO,WE`) | 4999 |
| MONTHLY | none; or one `BYMONTHDAY` (1 to 31, -31 to -1); or one plain `BYDAY` weekday with one `BYSETPOS` in {-1, 1, 2, 3, 4} | no bound |
| YEARLY | none; or one `BYMONTH` (1 to 12) with one `BYMONTHDAY` | 99 |

Every FREQ: `COUNT` at most 49; `UNTIL` on or before 2037-12-31 (the date in DTSTART's zone); not both COUNT and UNTIL. HOURLY, MINUTELY, SECONDLY: refused.

A master's DTSTART must be an occurrence of its rule: its calendar day, in DTSTART's zone (a DATE as written), matches the rule's `BYDAY`, `BYMONTHDAY`, `BYDAY` + `BYSETPOS` or `BYMONTH` + `BYMONTHDAY` (INTERVAL does not matter: DTSTART sets the phase); otherwise `pcb:unsupported-recurrence`, reason `DTSTART is not an occurrence of the RRULE`. Proton drops such a DTSTART and counts COUNT from the first match while Thunderbird shows it [verified: live 2026-11-13, `spec/HUMAN-CHECKS.md` W8]. The rule is checked after the rewrites below, which keep its meaning. A rule without a BY part always holds its DTSTART. Exceptions carry no rule. Reads are not checked: such an event already in Proton is served as it is, and only its rewrite is refused.

Transformed (same meaning, Proton's form):

| Client form | Written | Ruling |
|---|---|---|
| `FREQ=MONTHLY;BYDAY=<n><DAY>`, n in 1..4 or -1 (`+n` allowed), one day, no BYSETPOS | `FREQ=MONTHLY;BYDAY=<DAY>;BYSETPOS=<n>` (other ordinals: refused) | M4-R5 |
| `FREQ=DAILY;BYDAY=<plain days>` with INTERVAL absent or 1 (Thunderbird's "every weekday" [inferred]) | `FREQ=WEEKLY;BYDAY=<same days>`; DAILY BYDAY with INTERVAL above 1: refused | M4-R12 |

Not checked by the bridge: that DTSTART is the rule's first occurrence (research E.1 says the web client requires it; E12 live step). Known gaps (final audit): `INTERVAL=01` and duplicate days in a DAILY BYDAY are rewritten as sent.

## 5. VALARM → `Notifications`, and the other row fields

Proton keeps reminders in the row's plaintext `Notifications` (`[{Type, Trigger}]`), never in a card part (research D). Rules ported, as rules, from cal-gateway (README credits).

| VALARM | Outcome |
|---|---|
| `ACTION:EMAIL` | `Type` 0 |
| `ACTION:DISPLAY` or `AUDIO` | `Type` 1 |
| other ACTION, no or several ACTION or TRIGGER | dropped |
| TRIGGER `VALUE=DATE-TIME` (absolute) or `RELATED=END` | dropped |
| TRIGGER after the start | dropped, except on an all-day event less than 24 h after its start (that day at that time) |
| TRIGGER relative to the start, before it or at it | kept: seconds cut, written on one unit (W, D, H or M, the largest that divides it) for a timed event, as days plus a time of day for an all-day one |
| a unit over Proton's bound (weeks 999, days 6999, hours 999, minutes 9999) | dropped |
| more than 10 kept | the first 10 kept, the rest dropped |
| `DESCRIPTION`, `SUMMARY`, `REPEAT`, `DURATION`, `ATTACH`, `X-MOZ-LASTACK` and every other VALARM-level property | lost: Proton has nowhere to keep them (research F). The VEVENT-level `X-MOZ-LASTACK` and `X-MOZ-SNOOZE-TIME*` survive in SE; whether Thunderbird needs the VALARM-level one is a live check (`HUMAN-CHECKS.md` §6) |

Dropping an alarm never refuses the PUT. The tri-state, compared by meaning (type and signed offset, any order or spelling: `-P1D` is `-PT24H`):

| Case | `Notifications` sent |
|---|---|
| create, no VALARM | `[]`, or `null` when the calendar's default set is empty too |
| create, the set equals the calendar's defaults (part-day or full-day) | `null` (follows later changes of the defaults) |
| create, otherwise | the array |
| update, the set equals the one pcb served (the defaults when the row has `null`) | the row's own value, unchanged (`null` stays `null`) |
| update, no VALARM | `[]` |
| update, otherwise | the new array |

Other row fields:

| Field | Create | Update | Notes |
|---|---|---|---|
| `Color` | `null`; a new exception: its master's | the row's (`null` when it has none: Proton keeps the stored colour) | client `COLOR` ignored |
| `Attendees` (`Token`, `Status`, `Comment`) | `[]`; a new exception: its master's | the row's, as received | an omitted list wipes RSVPs (research A.2) |
| `IsOrganizer` | absent; a new exception: its master's | the row's (absent when absent or null) | |
| `Permissions` | 1 | 1 | what every bridge sends (research A.2) |
| `SharedKeyPacket`, `CalendarKeyPacket` | minted AES-256 keys, each wrapped to the primary calendar key only | never sent: the row's session keys are reused | §8, SPEC §5.3 |
| `PersonalEventContent` | not sent | not sent | personal parts are never written (M4-R29); an update of a row that holds one is refused, `pcb:personal-part` (§8), until live check L.4 #10 shows what Proton does to a part left out |
| Plaintext times, `RRule`, `Exdates`, `RecurrenceID`, `IsImport` | not sent | not sent | Proton derives the columns from SS (research C) |

## 6. SEQUENCE (ruling M4-R4)

- The value written is max(incoming, stored).
- It becomes stored + 1 when the shared signed content changes (DTSTART, DTEND, RRULE, EXDATE or RECURRENCE-ID, compared by meaning: instants and zones, EXDATE as a set, RRULE parts in any order, `INTERVAL=1` as absent) and the client did not bump it.
- No bump on text-only or alarm-only edits.
- An exception's SEQUENCE is at least its master's (Proton refuses less with 2001, research A.4).
- If Proton drops an unbumped text edit (research A.4 [conflict]), the read-back does not show it and the PUT answers 503, never 2xx (`TestDroppedEditIsNeverASuccess`); live check in `HUMAN-CHECKS.md` §6.

## 7. Series and attendees (rulings M4-R9, M4-R27, M4-R28, M4-R29)

| Body | Rows written (one Sync call, master first) |
|---|---|
| new UID | create the master, then one create per exception VEVENT; a body with exceptions only → `pcb:unsupported-change` |
| master + exceptions, rows exist | update the master; update each exception row the body still has; create a new exception row; delete each exception row the body no longer has (PUT replaces the whole resource) |
| delete one occurrence | the master's new EXDATE (update) and, when that occurrence had an exception row, its delete |
| exceptions only, and the UID has no master row | update or delete those exception rows; never create a master or a new exception (`pcb:unsupported-change`) |
| exceptions only, and the master row exists | `pcb:unsupported-change` (the body leaves out the master) |
| DELETE of the object | every row of the UID |

A row whose parts and notifications do not change gets no operation; a body that changes nothing sends no call and answers 204.

Attendees: adding or removing one (compared by address, case-folded) is `pcb:attendee-change`. A new exception must carry its master's attendee set (Thunderbird copies it): it is written with the master's stored ATTENDEE lines (their `X-PM-TOKEN` included), the master's ORGANIZER line and statuses (live check: Proton accepts tokens reused on another row). pcb never sends invitations or updates: attendees are not notified in v1.

## 8. Refusals that come from the stored event, not the body

The executor reads the UID's rows from Proton (`ListByUID`) before planning. Then:

| Code | When | Ruling |
|---|---|---|
| `pcb:placeholder` | a row renders as a placeholder (does not decrypt or parse, the calendar's keys failed, its Notifications do not read) | M3-R4 |
| `pcb:undecryptable` | a part of a row does not decrypt | |
| `pcb:not-ours` | a part is authored by an address that is not ours | M4-R18 |
| `pcb:unverified` | a part of ours is unsigned or its signature does not verify (any kind, attendees and personal included) | M4-R18, R19 |
| `pcb:invitation` | a row carries an `AddressKeyPacket` (someone else's invitation); also refuses a DELETE | B.3, M4-R27 |
| `pcb:untrusted-key` | a row's key packet differs from its pin; or a session key is not proven ours; or a row's UID column is not the event's UID | M4-R14, R17, R19, R30 |
| `pcb:unsupported-change` | the change needs a session key the row lacks; two exception rows for one occurrence; two parts of one kind in a row; series cases of §7 | M4-R13, R15 |
| `pcb:personal-part` | a row holds a personal part (per-user data, such as alarms stored before they moved to `Notifications`): an update never re-sends it (§5), and what Proton does to a part left out is live check L.4 #10; reason `this event has per-user data pcb cannot rewrite yet`. Lifted once L.4 #10 passes | M4-R29, audit write-I3 |
| `pcb:misrouted-stored-line` | a stored signed part holds a property that belongs in an encrypted part (we would sign it in clear) | M4-R16, R19 |
| `pcb:proton-refused` | Proton refused the call or one entry, and the read-back shows no concurrent change by someone else and not every operation done; other entries of the same call may have landed, so Proton can hold part of the change (a half-applied series edit is answered 403 and the client's next read shows it); the reason gives Proton's code only (audit write-M3; SPEC §8.1 step 9) | M4-R34 |

A DELETE skips the trust checks (placeholder, undecryptable, not ours, unverified, untrusted key) and `pcb:personal-part`: it re-signs nothing and leaves no part behind, so an event one cannot read or rewrite can still be deleted (M4-R27). Only `pcb:invitation` and the write gate refuse it.

UIDs are compared unescaped (RFC 5545 TEXT, audit write-I2): should Proton's UID column hold `a,b` for `UID:a\,b` (the UID-escaping question, SPEC §5.3), a PUT of that object at its own name is the same object, never a 409 naming itself; pcb cannot rewrite it while the two forms differ, so it answers 503 (nothing sent). The same UID at another name is a 409 naming the object's name.

Every refusal (each 403 `pcb:CODE`, 403 `need-privileges`, 409 `no-uid-conflict`, 412) is logged by `pcb serve` at warning level (the same code and href at most once per 10 minutes, then with `repeated=N`): `refused a CalDAV write` with the method, status, code, calendar ID, href and reason (fixed text; a 409 adds the conflicting href), never the body or the event's content (audit write-I1).

Not a pcb precondition: a read-only calendar is `DAV:need-privileges` (403); an undecided write gate is 503 (M4-R39). See SPEC §8.1 and §9 for the whole status mapping.

## 9. How stored lines are kept on update (`ical.Patch`)

- Each card part is patched in place, never rebuilt: an unchanged line keeps its part and its stored bytes (folding included).
- A single-valued property is compared with the copy pcb served (the first stored one, in part order); edited, it is replaced in place in every part that holds it.
- A multi-valued line (`ATTENDEE` by address, `EXDATE`, `CATEGORIES`, X- lines…) is kept where stored when the body still has it, as a multiset; a new one goes where §3 routes it.
- A stored line the body no longer carries is removed from every part, except nested components and `COLOR` lines (another client's data, kept).
- A part whose lines do not change keeps its stored bytes, but it is not re-sent as stored: when any line changed (or the reminders), every part of the update is sealed again (`cards.Seal`: signed with our key and encrypted with the row's session key). An update that changes no line and no reminder sends no call at all (204) (audit write-M1) [source: `Plan.update`, `internal/service/writeplan.go`; the no-call case: `TestAttendeeParameterEditsAreIgnored`].
- Part order and VCALENDAR wrapper are rewritten in Proton's fragment form: `BEGIN:VCALENDAR`, `BEGIN:VEVENT` … `END:VEVENT`, `END:VCALENDAR`, CRLF, folded at 75 octets, no VERSION or PRODID, no final CRLF (research B.4).
