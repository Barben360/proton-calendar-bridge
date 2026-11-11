# E07 — What changes in an event row on edit

Source-based, **pending live run**. Marks: `[source]` = read in a third-party project's source, `[claim]` = its author's own statement; neither is `[verified: live]`. Feeds the ETag (SPEC §8.2, `internal/store/cache/etag.go`). Evidence: `raw/m2-feed-research.md`.

## Question

Which edits (title, time, description, notification only, colour only, RSVP, move) emit a feed entry, move `LastEditTime`, move `ModifyTime`, or change any other row column?

## Source evidence

| Point | Evidence | Mark |
|---|---|---|
| Row fields (JSON): `ID, CalendarID, SharedEventID, CalendarKeyPacket, CreateTime, ModifyTime, LastEditTime, StartTime, EndTime, StartTimezone, EndTimezone, FullDay, UID, IsOrganizer, RecurrenceID, Exdates, RRule, Author, Permissions, SharedKeyPacket, SharedEvents, CalendarEvents, PersonalEvents, AttendeesEvents, Attendees, Notifications`, plus `Color` and `IsProtonProtonInvite`, `IsPersonalSingleEdit` | caloxide `protonmail/calendar.go:94-118`; cheeseandcereal `pkg/caltypes/types.go:37-80`, `docs/api.md:353-359` | [source]; no full captured row exists in any repo |
| go-proton-api's calendar types are stale | cheeseandcereal `types.go:5-7` ("verified live") | [claim] |
| Parts: `{Type, Data, Signature, MemberID?, Author?}`; SEQUENCE, DTSTAMP, RRULE live inside the signed shared card, not as a row column | cheeseandcereal `types.go:12-27`, `docs/crypto.md:175` | [source] |
| Timestamps are unix seconds | jmdlab `crypto.go:160` | [source] |
| All three production-style bridges build ETags from `LastEditTime` (caloxide `%X%s`, jmdlab `v{schema}-{LastEditTime}`, carbonate max `LastEditTime` plus hash of IDs); jmdlab: "served content can change without Proton's LastEdit moving" | caloxide `caldav.go:275`; jmdlab `caldav/backend.go:1422-1445`; carbonate `caldav/backend.go:454-473` | [source] |
| `ModifyTime` vs `LastEditTime` semantics | no source explains it | unknown |
| Notification-only and colour-only edits: whether `LastEditTime` moves, whether a feed entry is emitted | no source; notifications are a plaintext row field, a personal-only endpoint exists (`PUT .../events/{id}/personal`) | unknown |
| An edited occurrence is a separate row (same UID, `RecurrenceID` set); deleting a master does not cascade | jmdlab/carbonate; cheeseandcereal `docs/api.md:381` | [source] |

## What the engine assumes (M2-R4)

ETag = `"v1-"` + first 32 hex of sha256 of the canonical JSON of the **whole raw row** (every field, keys sorted, numbers kept as written). Any change to any field changes the ETag, so a notification-only or colour-only edit that leaves `LastEditTime` alone is still seen, provided the row is re-read (every listed feed ID is, M2-R3). An unchanged re-read changes nothing. Cost if wrong: a spurious client re-download when a field changes that does not matter.

Still open if the feed does not list a personal-only edit at all: the cache would miss it until the next full resync (24 h).

## Live run that settles it

During the E04 wait the author edits **one event per category, one at a time**, noting the order and time: title; start/end time; description; notification only (add a reminder); colour only; (RSVP if an invite exists); move to another calendar. Record each from `raw/E04/feed-page-*.json` and `feed-row-*.json`, with `pcb sync` between edits when finer resolution is wanted, then fill:

| Edit | Feed entry (Action) | `LastEditTime` moved | `ModifyTime` moved | Other columns changed | ETag changed |
|---|---|---|---|---|---|

The last column comes from `pcb event list --json` before and after. Also answer: what `ModifyTime` is.

Results: not run yet.

## Decision it feeds

Keep the whole-row ETag (cheap, safe) or narrow it to named fields. If a category produces no feed entry, the fallback resync interval (24 h) must shrink or a windowed re-list must be added (SPEC §5.5 fallback).

## E09 (decrypt cost)

Not part of M2 (M2-R1): the cache stores ciphertext rows only, so decrypt cost cannot change the M2 schema. Event decryption, the JSCalendar model and E09 move to M3, where CalDAV first needs content. Source figure for the record: caloxide's README claims about 1 ms per event (`README.md:162-166`, author's claim, hardware unstated, signature verification off); network, not CPU, dominated in every client.
