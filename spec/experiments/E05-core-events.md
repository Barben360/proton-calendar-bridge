# E05 — Core event loop for calendar list and membership changes

Source-based, **pending live run**. Feeds C4 sync and SPEC §5.5. Evidence: `raw/m2-feed-research.md`.

## Question

Does `/core/v4/events/{id}` carry calendar-level changes (calendar created or deleted, membership, calendar settings, default calendar), under which keys, and is it usable to avoid re-reading the calendar list?

## Source evidence

| Point | Evidence | Mark |
|---|---|---|
| go-proton-api models core events with only `EventID, Refresh, User, UserSettings, MailSettings, Messages, Labels, Addresses, Notifications, UsedSpace`; no calendar field; its fake server is mail only | go-proton-api `event_types.go:10-30`, `server/events.go` | [source] |
| caloxide models `Calendars` and `CalendarMembers` (`{ID, Action}`) and re-lists calendars on any entry; "a deleted member carries no calendar ID" | caloxide `protonmail/events.go:15-56`, `caldav/caldav.go:66-80` | [source] code; its only test is a synthetic fixture the author wrote (`notifier_test.go:234-242`); its own comment says it was logging to "confirm whether Proton signals calendar deltas here" |
| `CalendarUserSettings` in the core event: nobody models it | grep in all repos | unknown |
| core `latest` moves on any mail arrival: unsuitable as a calendar change token | carbonate `docs/ARCHITECTURE.md:108-112` | [claim] |
| Core cadence 30 s | caloxide `caldav/standby.go:326-327` | [source] |

No source captured a real core event carrying calendar data.

## What the engine assumes (ruling R2)

M2 runs **no core event loop**. The calendar list is re-read on every pass (one request: `GET /calendar/v1`), so a new, renamed or removed calendar is seen within one interval whatever the core loop does. Cost if wrong: one extra request per pass until this experiment is settled.

## Live run that settles it

Same session as E04 (`pcb-lab probe feed` records `/core/v4/events/latest` before the edits and `/core/v4/events/{id}` after, `raw/E04/feed-core-*.json`). To answer, the author also does in the web app during the wait, in this order, each as its own edit: create a calendar, rename it, change its colour, change calendar settings, set it default, delete it (membership share/unshare only if a second test account exists; otherwise "not exercised").

Fill: which keys appear (`Calendars`, `CalendarMembers`, others), their `Action` values, `Refresh` bits, whether each edit above produced an entry, whether `More` paging occurs.

Results: not run yet.

## Decision it feeds

If calendar changes ride the core loop reliably, a later milestone may poll it instead of re-listing calendars (fewer requests in the daemon, M5). If not, the per-pass re-list stays and SPEC §5.5 says so.
