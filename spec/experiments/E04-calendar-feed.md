# E04 — Calendar change feed (`modelevents`)

Source-based, **pending live run**. Feeds C4 sync (`internal/syncer`), SPEC §5.5 and §8.1. Evidence: `raw/m2-feed-research.md` (third-party clients, none Proton-documented; citations are file:line in each project). Confidence marks: `[source]` = read in a third-party project's source (never `[verified: live]`, which only a run on the test account earns), `[claim]` = the author's own statement, not reproduced by us, `[inferred]` = ours, `[unknown]`.

## Question

How does `GET /calendar/v1/{cal}/modelevents/{cursor}` behave: payload, cursor semantics, paging, what a stale cursor gets, propagation lag, and is there a push stream?

## Source evidence

| Point | Evidence | Mark |
|---|---|---|
| Endpoints: `modelevents/latest` gives `{CalendarModelEventID}`; `modelevents/{cursor}` gives `{CalendarModelEventID, More, Refresh, CalendarEvents:[{ID, Action, Event?}]}` | caloxide `protonmail/calendar.go:363-405`; jmdlab `internal/proton/delta.go:50-87`; carbonate `internal/caldav/store.go:130-205` | [source] (3 clients agree) |
| One feed per calendar (ID in path); no account-wide calendar feed | jmdlab `store.go:70-80`; caloxide `poller.go:41` | [source] |
| `Action`: 0 delete, 1 create, 2 update | caloxide `events.go:39-46`; jmdlab `delta.go:28-30` | [source] |
| `Event` payload of create/update is metadata only, no encrypted cards; delete has none | jmdlab `delta.go:21-26`, caloxide `calendar.go:351-361`, secondhand from WebClients; no captured response in any repo | [claim]; shape unknown |
| Every client re-reads the full row by ID before use | jmdlab `delta.go:147`, caloxide `poller.go:157` | [source] |
| carbonate ignores `Action`, looks every ID up; jmdlab treats a row GET 404 after Action 1/2 as delete | carbonate `store.go:171-180`; jmdlab `delta.go:147-153` | [source] |
| `latest` is a head, not a consumed position; polling from `latest` with no change returns nothing and the same cursor | carbonate `store.go:150-168` ("measured against a live account"); jmdlab `delta.go:131-134` | [claim] |
| Pitfall: the cursor returned when reading at `latest` sat past changes not yet issued; adopting it lost events. Persist `latest` itself as baseline | carbonate `docs/ARCHITECTURE.md:176-190` ("measured") | [claim], one source |
| Baseline `latest` taken before the full listing; replays are harmless if upserts are idempotent | jmdlab `poller.go:206-224` | [source]; rule inferred |
| Lag of "a second or two" between a write and the feed | carbonate `docs/ARCHITECTURE.md:196-205` | [claim] |
| Paging: loop while `More != 0`; caps seen: jmdlab 1000 pages, carbonate 20 pages then full re-read | caloxide `poller.go:72-111`; jmdlab `delta.go:108-139`; carbonate `store.go:171-205` | [source] |
| `Refresh != 0` means discard and full resync | caloxide `poller.go:94-100`, jmdlab `delta.go:119-122`, carbonate `store.go:195-199` | [source]; bit values unknown |
| Cursor too old: no source documents the answer; carbonate treats any error on the cursor GET as "read everything"; jmdlab and caloxide retry forever | carbonate `store.go:195-200`; caloxide `calendar.go:365`, jmdlab `delta.go:21-23` | server behaviour unknown |
| Cadence: caloxide 30 s per calendar (backoff to 10 min); jmdlab 60 s for all calendars, full resync every 6 h | caloxide `poller.go:13-27`; jmdlab `poller.go:28,67-70` | [source] |
| Push (`GET /event-notifier/v1/events`, SSE): caloxide's author saw 404 on 2026-10-29 and no `PingOMatic` flag; mail only | caloxide `FORK.md:112-135`, `notifier.go:19-64` | [claim]; dormant |

## What the engine assumes (M2, all `[assumed → E04]`)

- Shapes as in the table; unknown extra keys are ignored.
- `Action` and `Event` are **ignored** (M2-R3): every listed ID is re-read with `GET /calendar/v1/{cal}/events/{id}`; 404, or 422 with code 2501, is a delete. Robust to unknown `Action` values and to create-then-delete.
- Baseline cursor = `latest` read before the full listing; the returned cursor is stored in the same transaction as the row changes it produced.
- The listing pages by offset and may shift under a concurrent edit: rows are deduped by ID; a listing during which `latest` moved, or whose unique count differs from `Total`, is repeated (at most 3 listings, M2-R17); with none stable, the union is used and each cached row missing from all of them is re-read before it is tombstoned (question 7).
- Full resync when: never synced; `Refresh != 0`; the feed answers a 4xx other than 429; more than 50 pages in one pass; the last full sync is older than 24 h; a row re-read answers a non-gone 4xx other than 429/401 (M2-R8). Fallback resyncs are limited to one per calendar per 5 min (M2-R10; a calendar deferred by that gap makes the pass partial, exit 40, M2-R11, and its feed is not read again until the gap is over).
- Cadence: 30 s default, at least 10 s between passes, backoff x2 up to 10 min with jitter on session-wide failures. No push.

## Live run that settles it

`spec/HUMAN-CHECKS.md`, "M2 live session": `pcb-lab probe feed --calendar ID` records `latest`, waits while the author edits in the web app, then records every `modelevents` page, each listed row and `/core/v4/events/{id}` under `raw/E04/` (redacted by `internal/lab/redact`; `Data`, `Signature` and `Author` are redacted, cursors and IDs kept).

Read the recordings and fill:

1. Shape of `Event` (cards present or not, `ModifyTime`/`LastEditTime`, extra keys), the `Action` values seen, `More` values, ordering, repeated IDs.
2. Is `latest` taken after the edits equal to the last returned cursor.
3. Stale cursor: re-run `probe feed`-style GET with a garbage cursor (and, if found, an old one) by hand through the lab session: HTTP status and code, or `Refresh:1`. Retention window if learnable.
4. Lag between an edit and its appearance (run `pcb sync` immediately and again 5 s later).
5. Push route `/event-notifier/v1/events`: status with the test account.
6. Whether the engine's assumptions held: no spurious full resync, no lost edit (`pcb event list` equals the web app).
7. Listing order and paging stability under concurrent edits: is the `/events` listing ordered (by creation, start, ID?), and does a row created or deleted while a multi-page listing runs shift the later pages (offset paging: a skipped or repeated row) or not (keyset or snapshot paging)? Does `Total` move between pages? The engine assumes offset paging: it dedupes by ID, re-reads `latest` after each listing, lists again (at most twice, M2-R17) while `latest` moved or the unique count differs from the first page's `Total`, and when no listing settles uses their union and re-reads every cached row missing from all of them before tombstoning it.

Results: not run yet.

## Decision it feeds

Settles the markers in SPEC §5.5/§8.1 and whether the fallback rules (4xx other than 429, 50 pages, 24 h) match what Proton does; a stale cursor answered by something the engine classifies as session-wide would need a classifier fix.
