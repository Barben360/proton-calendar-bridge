# M2 Store and Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A local SQLite cache of every calendar's event rows (ciphertext as Proton returns it, nothing decrypted), filled by an initial sync and kept current by Proton's per-calendar change feed, driven by `pcb sync [--watch]` and inspected with `pcb event list`; proven offline against the fake Proton, then live (deferred) by following edits made in the Proton web app.

**Architecture:** C1 gains `internal/proton/events` (row type, unscoped paged listing, get by ID, change feed `modelevents`). C2 gains `internal/store/cache` (SQLite through `modernc.org/sqlite`, pure Go: rows, tombstones, per-calendar cursor, one monotonic change counter that M3 turns into CTags and sync tokens). C4 is `internal/syncer` (one pass over all calendars, and a watch loop with backoff); it needs only the session, never the keys. C3 (`service`) exposes `Sync`, `Events` and a sync section in `Status`, and keeps one API client per stored session (R12m). The CLI still calls only `service.API`.

**Tech Stack:** Go 1.27, `modernc.org/sqlite v1.61.1` (same version as backsight; `database/sql`, WAL), cobra v1.10.2, the M1 `api.Client`, `filelock`, `atomicfile`.

**Spec:** `spec/SPEC.md` draft v2 §5.4, §5.5, §7.1, §7.2, §8.1, §8.2, §13, §15, §16, §17 (E04, E05, E07, E09), §18 M2. Source research (read it, cite it, never copy code): `spec/experiments/raw/m2-feed-research.md` (added by Task 6 from the controller's research; until then the controller gives its path). Reference clients (audit only): caloxide, jmdlab cal-gateway, carbonate, cheeseandcereal proton-cal.

**Out of scope (YAGNI, say so in review):** decrypting event cards, the JSCalendar model and the iCalendar codec (M3, where CalDAV first needs content; E09, the decrypt-cost benchmark, moves with them — ruling R1); windowed `Type` queries (the cache holds every row, M3 filters locally); the core event loop `/core/v4/events` (E05 unverified: the calendar list is re-read every cycle instead — ruling R2); the push stream `/event-notifier` (dormant per research); the idle/active poll cadence (needs CalDAV clients, M3); the daemon, `pcb serve` and the sync TTL/state lock carry-overs (M5); tombstone pruning; any config key for sync (flags only until the daemon).

## Global Constraints

- Everything in the M1 plan's Global Constraints still holds (module, `make lint test`, `--json` envelope, exit codes 0/2/10/20/30/40, secrets never in argv/logs/errors, `internal/cli` imports only `service` and CLI packages — `arch_test.go`, every `service.API` error is an `*exitcode.Error` with a hint, live calls only against the test account and only as deferred **Live verification** steps, tests bite, commit subjects `<emoji> <imperative>`, workflow files pushed over SSH).
- Cache file `$XDG_STATE_HOME/pcb/pcb.db` (§7.1), mode 0600, WAL; plus `sync.lock` in the state dir. Nothing decrypted is ever written (§7.2): rows are stored as the raw JSON Proton returned.
- Every cache record is keyed by account ID (= Proton user ID): the cache records its account in `meta`; opened for another account it is emptied first.
- Listing page size 100 (`PageSize` above 100 → 400/2021 [doc/claim: cheeseandcereal docs/api.md:312]; go-proton-api's 150 is not used).
- Feed (E04, source-based, `[assumed → E04]` until the live run): `GET /calendar/v1/{cal}/modelevents/latest` → `{CalendarModelEventID}`; `GET /calendar/v1/{cal}/modelevents/{cursor}` → `{CalendarModelEventID, More, Refresh, CalendarEvents:[{ID, Action, Event?}]}`. The engine ignores `Action` and `Event`: every listed ID is re-read by `GET /calendar/v1/{cal}/events/{id}` (gone = 404, or 422 with code 2501 → delete) [carbonate/jmdlab design].
- Baseline cursor = `latest` read **before** the full listing (carbonate's measured pitfall: a cursor derived at `latest` lost events); after a feed read, the returned cursor is stored in the same transaction as the row changes it produced.
- Full resync of a calendar when: never synced; `Refresh != 0`; the feed answers a 4xx other than 429 (cursor unusable); more than 50 feed pages in one pass; the last full sync is older than 24 h.
- Session-wide failures (429, ≥ 500, network, session ended — `service.sessionWide`) stop the whole pass with the M1 mapping; any other per-calendar failure is recorded on that calendar, the other calendars continue, and the pass exits 40 (partial).
- ETag of a row = `"v1-" + first 32 hex chars of sha256(canonical JSON of the raw row)` (canonical: decoded with `UseNumber`, re-encoded with sorted keys); a re-read row whose ETag is unchanged changes nothing (no counter bump).
- Good citizen: requests go through the one per-account `api.Client` and its limiter; the watch loop waits ≥ 10 s between passes (default 30 s), backs off exponentially on session-wide failures (×2 up to 10 min, jitter ±10 %), and stops (exit 10) when a person is needed.

## Review Focus

1. **An event created while the initial listing is running** → it is in the cache after the next incremental pass (baseline taken before the listing); pinned by `TestEventCreatedDuringListingIsNotLost` (Task 4).
2. **An event edited then deleted between two passes, or a feed ID whose row is already gone** → tombstoned, never an error, the pass exits 0; pinned by `TestFeedIDGoneIsDeleted` (Task 4).
3. **Proton returns 5xx/429 in the middle of a pass** → nothing half-applied: a calendar's cursor never moves past rows not stored; the next pass picks up from the old cursor; pinned by `TestFailureMidPassKeepsOldCursor` (Task 4).
4. **A different account logs in on the same state dir** → the cache never shows the previous account's rows; `pcb logout` deletes `pcb.db` (and `-wal`, `-shm`); pinned by `TestOpenForAnotherAccountEmpties` (Task 3) and `TestLogoutDeletesCache` (Task 5).
5. **`pcb sync` while `pcb sync --watch` runs** → exit 20 "a sync is already running" within a second, no Proton call; `pcb event list` keeps working meanwhile; pinned by `TestSecondSyncIsBlocked` (Task 5).

---

### Task 1: One API client per stored session (R12m) and Abort after a refresh (R18m)

**Files:**
- Modify: `internal/service/local.go`, `internal/service/session.go`, `internal/service/login.go`, `.github/workflows/ci.yml` (`timeout-minutes: 30`: the M1 merge run took 17 min 16 s of its 20)
- Test: `internal/service/session_test.go`, `internal/service/login_test.go`

**Interfaces:**
- Produces: `(*Local).session(ctx)` returns the same `*api.Client` for the same stored session (same account ID and UID) across calls in one `Local`, so its limiter and refreshed tokens persist; a relogin, logout, `Close` or a sealed UID change drops it. Signature unchanged.
- [ ] **Step 1: Failing tests**: `TestSessionClientIsReused` (two `Calendars` calls → one client: the fake sees the second call with the access token the first one refreshed, and `POST /auth/v4/refresh` count stays 1 after `ExpireAccessTokens` once); `TestReloginDropsClient` (after a relogin the next call uses the new UID); `TestAbortAfterRefreshRevokesLiveSession` (a login whose key calls refresh once, then fails before sealing → the fake's session for the refreshed tokens is revoked: a call with them gets 401).
- [ ] **Step 2: Run** `go test ./internal/service/ -run 'SessionClient|ReloginDrops|AbortAfterRefresh'` → FAIL.
- [ ] **Step 3: Implement**: cache `{accountID, uid, *api.Client}` on `Local` under `mu`-free access (its own small mutex); `session` adopts the sealed tokens into a reused client only when the UID matches; Abort revokes with the pending client's current `Tokens()`.
- [ ] **Step 4: Run** `make lint test` → PASS.
- [ ] **Step 5: Commit** `♻️ Reuse one API client per stored session`.

### Task 2: Fake Proton events and feed; C1 `internal/proton/events`

**Files:**
- Create: `internal/proton/events/events.go`, `internal/proton/events/feed.go`, `internal/proton/protontest/events.go`
- Test: `internal/proton/events/events_test.go`, `internal/proton/events/feed_test.go`, `internal/proton/protontest/events_test.go`

**Interfaces:**
- Produces (C1):
  - `events.Row{ID, CalendarID, UID string; Start, End int64; FullDay bool; RecurrenceID *int64; RRule string; LastEditTime int64; Raw json.RawMessage}` — typed plaintext fields decoded from the row; `Raw` is the row object exactly as received.
  - `events.ListAll(ctx, c *api.Client, calID string) ([]events.Row, error)`: `GET /calendar/v1/{cal}/events?Page=N&PageSize=100` from page 0 until a page shorter than 100; IDs path-escaped.
  - `events.Get(ctx, c, calID, id string) (events.Row, error)`; `events.ErrGone` when the row no longer exists (HTTP 404, or 422 with code 2501).
  - `events.Latest(ctx, c, calID) (cursor string, err error)`.
  - `events.Changes(ctx, c, calID, cursor string) (events.Delta, error)` with `Delta{Cursor string; IDs []string; Refresh bool}`: follows `More` up to 50 pages (more → `Refresh: true`); `IDs` deduplicated, first-appearance order; `Refresh` set when any page has `Refresh != 0`.
- Produces (fake, `protontest`): `Event{UID string; Start, End int64; FullDay bool; RRule string; RecurrenceID *int64}`; `(*Fake).AddEvent(calID string, e Event) (id string)`, `UpdateEvent(calID, id string, e Event)` (bumps `LastEditTime`), `DeleteEvent(calID, id string)`, `ForceRefresh(calID string)` (next feed page answers `Refresh: 1`), `ExpireFeed(calID string)` (cursors issued so far answer 422 code 2001), `SetFeedPageSize(n int)` (default 50 entries per page); every add/update/delete appends `{ID, Action}` to the calendar's feed; rows carry the research's field set with opaque placeholder parts; listing refuses `PageSize > 100` with 400/2021, returns `Total`; a get of an ID of another calendar or a deleted one → 422/2501. Feed entries are visible only to the account owning the calendar. `(*Fake).Session(t, username) api.Tokens` returns live tokens for the account without an SRP login (the front mints and accepts them like issued ones), so C1/C4 tests that need a session but test no login skip the SRP and bcrypt cost (CI budget, see Task 1).
- [ ] **Step 1: Failing tests**: fake — `TestFakeFeedRecordsEveryChange`, `TestFakeListingRefusesLargePages`; events — `TestListAllPagesUntilShortPage` (250 rows → 3 requests, all rows, `Raw` byte-equal to the served object), `TestGetGone` (404 → `ErrGone`; 422/2501 → `ErrGone`; 422/other → not `ErrGone`), `TestChangesFollowsMore` (120 changes, page 50 → 3 pages, final cursor, IDs deduped), `TestChangesRefresh`, `TestChangesTooManyPagesMeansRefresh`, `TestChangesNothingNew` (same cursor back, no IDs), `TestPathsEscaped` (an ID with `/` never reaches another route).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**; decode rows with a typed struct for the plaintext fields and keep `Raw` from `json.RawMessage` (no re-encoding).
- [ ] **Step 4: Run** `make lint test` → PASS.
- [ ] **Step 5: Commit** `✨ Read Proton event rows and the calendar change feed`.

### Task 3: C2 cache (`internal/store/cache`)

**Files:**
- Create: `internal/store/cache/cache.go`, `internal/store/cache/schema.go`, `internal/store/cache/etag.go`, `internal/store/cache/doctor.go`
- Create: `internal/doctor/cache.go`
- Modify: `go.mod`/`go.sum` (`modernc.org/sqlite v1.61.1`), `internal/cli/doctor.go` (list the check)
- Test: `internal/store/cache/cache_test.go`, `internal/store/cache/etag_test.go`

**Interfaces:**
- Produces (C2 depends on no C1 package, §4: the cache has its own row type; the syncer converts):
  - `cache.Row{ID, UID string; Start, End int64; FullDay bool; RecurrenceID *int64; RRule string; LastEditTime int64; Raw json.RawMessage}`.
  - `cache.Path(stateDir string) string` (`<stateDir>/pcb.db`); `cache.Open(path, accountID string) (*cache.Cache, error)` (creates 0600, WAL, `busy_timeout` 5 s, schema v1 via `PRAGMA user_version`; a newer version → error naming it; another account recorded → all tables emptied, then the new account recorded); `(*Cache).Close() error`; `cache.Remove(path string) error` (db, `-wal`, `-shm`; missing is not an error).
  - `cache.ETag(raw json.RawMessage) (string, error)` per the Global Constraints.
  - Calendars: `(*Cache).Calendars(ctx) ([]cache.Calendar, error)`, `cache.Calendar{ID, Name, Type string; Cursor string; FullAt, SyncedAt time.Time; Error string; Events int}`; `(*Cache).PutCalendar(ctx, cache.Calendar) error` (name/type/error/sync times, never the cursor), `(*Cache).DropCalendar(ctx, id) error` (its rows and tombstones too).
  - `(*Cache).ReplaceCalendar(ctx, calID, cursor string, rows []cache.Row, at time.Time) (cache.Changes, error)`: one transaction — upsert rows whose ETag changed, tombstone rows absent from `rows`, set cursor and `FullAt`/`SyncedAt`.
  - `(*Cache).ApplyDelta(ctx, calID, cursor string, upserts []cache.Row, gone []string, at time.Time) (cache.Changes, error)`: one transaction — upserts (only on ETag change), tombstones for `gone` that exist, cursor and `SyncedAt`.
  - `cache.Changes{Added, Updated, Deleted int}`; every row or tombstone write takes the next value of one monotonic counter (`(*Cache).Seq(ctx) (int64, error)`); a re-created ID removes its tombstone.
  - Reads: `(*Cache).Events(ctx, calID string) ([]cache.Event, error)`, `cache.Event{cache.Row fields minus Raw; ETag string; Seq int64}`; `(*Cache).Raw(ctx, calID, id string) (json.RawMessage, error)` (for M3).
  - `cache.Inspect(path string) (cache.Info, error)`, `cache.Info{Exists bool; Mode fs.FileMode; Schema int; Account string; QuickCheck string}` (opens read-only, never creates); `doctor.CacheCheck{StateDir string}` in `internal/doctor` (like `SecretsCheck`): absent → ok "no cache yet"; present → fails on a mode other than 0600, an unknown schema or a `quick_check` other than `ok`, each with the fix (`pcb sync --full` after removing the file).
- [ ] **Step 1: Failing tests**: `TestOpenCreates0600WAL`, `TestOpenRefusesNewerSchema`, `TestOpenForAnotherAccountEmpties`, `TestReplaceTombstonesMissingRows`, `TestApplyDeltaUnchangedETagNoBump` (same raw → `Changes{}` and `Seq` unchanged), `TestApplyDeltaIsAtomic` (a failing upsert in the batch → cursor and rows unchanged; use a row the schema refuses, e.g. empty ID), `TestRecreatedIDDropsTombstone`, `TestETagCanonical` (same object with reordered keys / spacing → same ETag; one changed value → different; numbers kept exact: `1731000000123` survives), `TestRemoveDeletesWalAndShm`, `TestTwoProcessesReadWhileWriting` (a second `Open` reads while a transaction is open on the first).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** with `database/sql` + `modernc.org/sqlite`; schema: `meta(key PK, value)`, `calendars(id PK, name, type, cursor, full_at, synced_at, error)`, `events(calendar_id, id, uid, start, end, full_day, recurrence_id, rrule, last_edit, etag, seq, raw, PK(calendar_id, id))`, `tombstones(calendar_id, id, seq, PK(calendar_id, id))`, counter in `meta`. Timestamps stored as Unix seconds.
- [ ] **Step 4: Run** `make lint test` and `GOOS=windows go vet ./...` → PASS.
- [ ] **Step 5: Commit** `✨ Cache Proton event rows in SQLite`.

### Task 4: C4 sync engine (`internal/syncer`)

**Files:**
- Create: `internal/syncer/syncer.go`, `internal/syncer/watch.go`
- Test: `internal/syncer/syncer_test.go`, `internal/syncer/watch_test.go`

**Interfaces:**
- Consumes: Task 2 (`events.*`, fake), Task 3 (`cache.*`; the syncer converts `events.Row` to `cache.Row`), M1 `calendar.List`.
- Produces:
  - `syncer.Pass(ctx, c *api.Client, db *cache.Cache, o syncer.Options) (syncer.Report, error)`; `syncer.Options{Full bool; Now func() time.Time; FullEvery time.Duration (default 24 h)}`; `syncer.Report{Calendars []syncer.CalendarReport}`, `syncer.CalendarReport{ID, Name, Mode string ("initial"|"incremental"|"full"); Changes cache.Changes; Events int; Err error}`. The pass: `calendar.List` → `PutCalendar` for each, `DropCalendar` for cached calendars no longer listed → per calendar: full (`Latest` then `ListAll` then `ReplaceCalendar`) when the Global Constraints say so, else `Changes` → `Get` each ID (`ErrGone` → gone) → `ApplyDelta`; a cursor answer of 4xx (not 429) → full. A session-wide error returns it at once (the reports so far are dropped); other errors land in `CalendarReport.Err` and `PutCalendar`'s `Error`.
  - `syncer.Watch(ctx, pass func(context.Context) (syncer.Report, error), o syncer.WatchOptions) error`; `WatchOptions{Interval time.Duration (≥ 10 s, default 30 s); Sleep func(ctx, d) error; OnPass func(syncer.Report, error); Stop func(error) bool}`: runs `pass`, reports it, sleeps `Interval` on success or the backoff (×2 per consecutive failure from `Interval`, cap 10 min, ±10 % jitter) after an error for which `Stop` is false; returns the error for which `Stop` is true, or `ctx.Err()`.
- [ ] **Step 1: Failing tests**: `TestInitialPassFillsCache`, `TestIncrementalPassAppliesFeed` (add/update/delete in the fake → one `Changes`, N `Get`s, cache matches the fake), `TestEventCreatedDuringListingIsNotLost` (fake hook adds an event while page 1 is served), `TestFeedIDGoneIsDeleted`, `TestRefreshTriggersFullResync`, `TestExpiredCursorTriggersFullResync`, `TestStaleFullSyncRunsFull` (FullAt 25 h ago), `TestFailureMidPassKeepsOldCursor` (`FailNext` 503 on a `Get` → pass returns the error; cached cursor unchanged; the next pass applies every change), `TestOneCalendarFailingOthersContinue` (403 on one calendar's feed → its `Err` set, others synced), `TestCalendarRemovedIsDropped`, `TestNewCalendarGetsInitialSync`, `TestWatchBacksOffAndStops` (fake `Sleep` records waits: 30, 60, 120 … capped 600 ±10 %; reset after a success; `Stop` true → returns that error), `TestWatchHonoursContext`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**; the engine never unlocks keys and never logs row content (IDs only).
- [ ] **Step 4: Run** `make lint test` → PASS.
- [ ] **Step 5: Commit** `✨ Sync the cache from Proton's change feed`.

### Task 5: C3 service and CLI — `pcb sync`, `pcb event list`, status, logout

**Files:**
- Create: `internal/service/sync.go`, `internal/cli/sync.go`, `internal/cli/event.go`
- Modify: `internal/service/api.go`, `internal/service/local.go` (Logout removes the cache), `internal/cli/status.go`, `internal/cli/root.go` (or wherever commands register), `internal/cli/clitest` contract list, doctor wiring (cache check), `README.md` (Usage)
- Test: `internal/service/sync_test.go`, `internal/cli/sync_test.go`, `internal/cli/event_test.go`

**Interfaces:**
- Produces (`service.API` additions):
  - `Sync(ctx, SyncRequest) (SyncResult, error)`; `SyncRequest{Full bool}`; `SyncResult{Calendars []CalendarSync}`, `CalendarSync{ID, Name, Mode string; Added, Updated, Deleted, Events int; Error string}`. Takes `sync.lock` with `TryLock` (busy → Blocked "a sync is already running", hint "wait for it or stop pcb sync --watch"); opens the cache for the session's account; runs `syncer.Pass` inside `withSession`; any calendar error → the result plus an `*exitcode.Error{Code: Partial}` the CLI prints with the data.
  - `WatchSync(ctx, SyncRequest, interval time.Duration, onPass func(SyncResult, error)) error` (holds the lock for its lifetime; `Stop` = the error is MustAct).
  - `Events(ctx, EventsRequest) ([]Event, error)`; `EventsRequest{Calendar string (ID or exact name, required); From, To *time.Time}`; `Event{ID, UID, CalendarID string; Start, End time.Time; AllDay, Recurring bool; RecurrenceID *time.Time; LastEdit time.Time; ETag string}`; from the cache only, no network (never synced → MustAct "run pcb sync"); `From/To` keep events overlapping the window, recurring masters starting before `To` always kept.
  - `Status` gains `Sync *SyncStatus` (`{Calendars []{ID, Name, Events int, SyncedAt, FullAt *time.Time, Error string}}`, nil without a cache).
  - `Logout` removes the cache (`cache.Remove`) with the secrets wipe.
- Produces (CLI): `pcb sync [--full] [--watch] [--interval 30s]` (text: one line per calendar and pass; `--json` without `--watch` prints one envelope; `--json --watch` refused as usage: one envelope per command); `pcb event list --calendar C [--from DATE] [--to DATE]` (RFC 3339 or `YYYY-MM-DD`; text table: start, end, all-day, recurring, ID); `pcb status` shows the sync section.
- [ ] **Step 1: Failing tests**: service — `TestSyncThenEvents`, `TestSecondSyncIsBlocked`, `TestSyncPartialExits40`, `TestEventsWithoutCacheMustAct`, `TestEventsWindow`, `TestLogoutDeletesCache`, `TestWatchStopsWhenLoginNeeded`; CLI — `TestSyncJSONEnvelope`, `TestSyncWatchJSONRefused`, `TestEventListByName`, `TestEventListBadDate` (exit 2), contract tests for the new command paths (`clitest.CheckUsageContract`).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Run** `make lint test` → PASS.
- [ ] **Step 5: Commit** `✨ Add pcb sync and pcb event list`.

### Task 6: Feed probe, experiments, docs, M2 live session (deferred)

**Files:**
- Create: `spec/experiments/E04-calendar-feed.md`, `spec/experiments/E05-core-events.md`, `spec/experiments/E07-row-changes.md`, `spec/experiments/raw/m2-feed-research.md` (the controller's research, as given), `internal/lab/probe/feed.go`
- Modify: `cmd/pcb-lab` (register `probe feed`), `spec/SPEC.md` (§5.5, §7.2, §8.1, §8.2 markers and the rulings; §17 status; §18 M2), `spec/HUMAN-CHECKS.md` ("M2 live session"), `spec/OPEN.md`, `README.md` (credits: caloxide, jmdlab cal-gateway, carbonate design notes)
- Test: `internal/lab/probe/feed_test.go`

**Interfaces:**
- Produces: `pcb-lab probe feed --calendar ID [--wait 2m]`: records `latest`, waits while the author edits in the web app (or until Enter), then records every `modelevents/{cursor}` page, the re-read rows, and `/core/v4/events/{id}` from a `latest` taken at the start, through the M0 fail-closed redaction and recorder, under `spec/experiments/raw/E04/`.
- [ ] **Step 1: Failing tests**: `TestProbeFeedRecordsRedacted` (against the fake: recordings exist, contain no token, no `Data`/`Signature` part content, cursors kept).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `make lint test` → PASS.
- [ ] **Step 5: Write-ups**: E04/E05/E07 each with question, method, source evidence (from the research, with its citations and confidence marks), what the engine assumes, and the live run that settles it; E09 noted as moved to M3 (R1).
- [ ] **Step 6: HUMAN-CHECKS "M2 live session"**: after the M1 live session: `pcb sync` (initial) → the author creates, edits (title, time, notification only, colour only), moves and deletes events in the web app → `pcb-lab probe feed` recording → `pcb sync` → `pcb event list` matches the web app; `pcb sync --watch` follows a new edit within one interval; each step names its expected output.
- [ ] **Step 7: Commit** `📝 Write up the change-feed experiments and the M2 live session`.
