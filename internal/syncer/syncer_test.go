package syncer_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"
	_ "modernc.org/sqlite" // the "sqlite" driver, to plant a failing trigger

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

var t0 = time.Unix(1_800_000_000, 0)

// setup takes a live session on a fresh fake (calendar.List needs a real one)
// and opens an empty cache. The client never sleeps between retries.
func setup(t *testing.T) (*protontest.Fake, *api.Client, *cache.Cache) {
	t.Helper()
	f := protontest.New(t, protontest.Options{})
	c := api.New(f.Login(t, f.Username), api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		Sleep:   func(context.Context, time.Duration) error { return nil },
	})
	db, err := cache.Open(cache.Path(t.TempDir()), f.UserID(f.Username))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return f, c, db
}

func at(now time.Time) syncer.Options {
	return syncer.Options{Now: func() time.Time { return now }}
}

func pass(t *testing.T, c *api.Client, db *cache.Cache, o syncer.Options) syncer.Report {
	t.Helper()
	rep, err := syncer.Pass(t.Context(), c, db, o)
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return rep
}

func report(t *testing.T, rep syncer.Report, calID string) syncer.CalendarReport {
	t.Helper()
	for _, r := range rep.Calendars {
		if r.ID == calID {
			return r
		}
	}
	t.Fatalf("no report for calendar %s in %+v", calID, rep)
	return syncer.CalendarReport{}
}

func cached(t *testing.T, db *cache.Cache, calID string) cache.Calendar {
	t.Helper()
	cals, err := db.Calendars(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cals {
		if c.ID == calID {
			return c
		}
	}
	t.Fatalf("calendar %s not in the cache", calID)
	return cache.Calendar{}
}

func etags(t *testing.T, db *cache.Cache, calID string) map[string]string {
	t.Helper()
	evs, err := db.Events(t.Context(), calID)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range evs {
		m[e.ID] = e.ETag
	}
	return m
}

// matches fails unless the cache holds exactly the fake's rows of calID,
// byte for byte (same IDs, same ETags of the raw rows). It lists the
// calendar through the fake: measure call counts before calling it.
func matches(t *testing.T, c *api.Client, db *cache.Cache, calID string) {
	t.Helper()
	rows, err := events.ListAll(t.Context(), c, calID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, r := range rows {
		tag, err := cache.ETag(r.Raw)
		if err != nil {
			t.Fatal(err)
		}
		want[r.ID] = tag
	}
	got := etags(t, db, calID)
	if len(got) != len(want) {
		t.Errorf("cache holds %d rows of %s, Proton %d", len(got), calID, len(want))
	}
	for id, tag := range want {
		if got[id] != tag {
			t.Errorf("row %s: cached ETag %q, Proton's %q", id, got[id], tag)
		}
	}
}

func listCalls(f *protontest.Fake, cal string) int {
	return f.Calls("GET", "/calendar/v1/"+cal+"/events")
}
func getCalls(f *protontest.Fake, cal string) int {
	return f.Calls("GET", "/calendar/v1/"+cal+"/events/")
}
func feedCalls(f *protontest.Fake, cal string) int {
	return f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/")
}

func addEvents(f *protontest.Fake, cal string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = f.AddEvent(cal, protontest.Event{UID: "u" + strconv.Itoa(i), Start: int64(1000 + i), End: int64(2000 + i)})
	}
	return ids
}

func TestInitialPassFillsCache(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Feed", Type: 1})
	addEvents(f, a, 150) // two listing pages
	addEvents(f, b, 2)
	rep := pass(t, c, db, at(t0))
	if len(rep.Calendars) != 2 || rep.Calendars[0].ID != a || rep.Calendars[1].ID != b {
		t.Fatalf("reports %+v, want %s then %s", rep.Calendars, a, b)
	}
	for _, w := range []struct {
		id, name, typ string
		n             int
	}{{a, "Perso", "normal", 150}, {b, "Feed", "subscribed", 2}} {
		r := report(t, rep, w.id)
		if r.Mode != "initial" || r.Name != w.name || r.Err != nil || r.Events != w.n || r.Changes != (cache.Changes{Added: w.n}) {
			t.Errorf("report %+v, want initial %q with %d added, %d events, no error", r, w.name, w.n, w.n)
		}
		cal := cached(t, db, w.id)
		head, err := events.Latest(t.Context(), c, w.id)
		if err != nil {
			t.Fatal(err)
		}
		if cal.Name != w.name || cal.Type != w.typ || cal.Cursor != head || !cal.FullAt.Equal(t0) || !cal.SyncedAt.Equal(t0) || cal.Error != "" {
			t.Errorf("cached %+v, want %q %s, cursor %q, synced and listed at %v", cal, w.name, w.typ, head, t0)
		}
		matches(t, c, db, w.id)
	}
}

func TestIncrementalPassAppliesFeed(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 3)
	pass(t, c, db, at(t0))
	before := cached(t, db, a)
	f.AddEvent(a, protontest.Event{UID: "new", Start: 5, End: 6})
	f.UpdateEvent(a, ids[0], protontest.Event{UID: "u0", Start: 7, End: 8})
	f.DeleteEvent(a, ids[1])
	feed0, get0, list0 := feedCalls(f, a), getCalls(f, a), listCalls(f, a)
	t1 := t0.Add(time.Hour)
	r := report(t, pass(t, c, db, at(t1)), a)
	if feed, gets, lists := feedCalls(f, a)-feed0, getCalls(f, a)-get0, listCalls(f, a)-list0; feed != 1 || gets != 3 || lists != 0 {
		t.Errorf("requests: %d feed, %d row reads, %d listings; want 1, 3, 0", feed, gets, lists)
	}
	if r.Mode != "incremental" || r.Err != nil || r.Changes != (cache.Changes{Added: 1, Updated: 1, Deleted: 1}) || r.Events != 3 {
		t.Errorf("report %+v, want incremental, 1 added, 1 updated, 1 deleted, 3 events", r)
	}
	after := cached(t, db, a)
	if after.Cursor == before.Cursor || !after.FullAt.Equal(t0) || !after.SyncedAt.Equal(t1) {
		t.Errorf("cached %+v (before %+v): want a new cursor, FullAt kept at %v, SyncedAt %v", after, before, t0, t1)
	}
	matches(t, c, db, a)
}

// The baseline cursor is read before the listing: a row created while the
// listing runs (and absent from it) comes in with the next feed read.
func TestEventCreatedDuringListingIsNotLost(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 1)
	// A row is created during every listing, so none of them is stable
	// (ruling R17 re-lists at most twice): the union holds the rows created
	// during the first two, not the one created during the last.
	var late []string
	f.OnListPage(func(calID string, page int) {
		if calID == a && page == 0 {
			late = append(late, f.AddEvent(a, protontest.Event{UID: "late", Start: 1, End: 2}))
		}
	})
	pass(t, c, db, at(t0))
	f.OnListPage(nil)
	if len(late) != syncer.MaxListings {
		t.Fatalf("%d listings, want %d (an unstable listing is repeated)", len(late), syncer.MaxListings)
	}
	last := late[len(late)-1]
	if _, ok := etags(t, db, a)[last]; ok {
		t.Fatal("the row created during the last listing is already cached: the test proves nothing")
	}
	r := report(t, pass(t, c, db, at(t0.Add(time.Minute))), a)
	if r.Mode != "incremental" || r.Changes != (cache.Changes{Added: 1}) {
		t.Errorf("second pass %+v, want incremental adding the late row %s", r, last)
	}
	cachedRows := etags(t, db, a)
	for _, id := range late {
		if _, ok := cachedRows[id]; !ok {
			t.Errorf("the row %s created during the listing was lost", id)
		}
	}
	matches(t, c, db, a)
}

// Every listed ID is re-read; one Proton says is gone (404, or 422/2501) is
// deleted whatever the feed's Action said.
func TestFeedIDGoneIsDeleted(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 3)
	pass(t, c, db, at(t0))
	f.DeleteEvent(a, ids[0])                                                          // feed: delete; row read: 422/2501
	f.UpdateEvent(a, ids[1], protontest.Event{UID: "u1", Start: 3, End: 4})           // feed: update ...
	f.FailNext("GET", "/calendar/v1/"+a+"/events/"+ids[1], http.StatusNotFound, 0, 0) // ... but the row is gone (404)
	brief := f.AddEvent(a, protontest.Event{UID: "brief", Start: 1, End: 2})          // created then deleted
	f.DeleteEvent(a, brief)
	r := report(t, pass(t, c, db, at(t0.Add(time.Minute))), a)
	if r.Err != nil || r.Mode != "incremental" || r.Changes != (cache.Changes{Deleted: 2}) || r.Events != 1 {
		t.Errorf("report %+v, want incremental, 2 deleted, 1 event left, no error", r)
	}
	got := etags(t, db, a)
	if len(got) != 1 || got[ids[2]] == "" {
		t.Errorf("cached rows %v, want only %s", got, ids[2])
	}
}

func TestRefreshTriggersFullResync(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 2)
	pass(t, c, db, at(t0))
	f.AddEvent(a, protontest.Event{UID: "new", Start: 5, End: 6})
	f.ForceRefresh(a)
	list0 := listCalls(f, a)
	t1 := t0.Add(time.Hour)
	r := report(t, pass(t, c, db, at(t1)), a)
	if r.Mode != "full" || r.Err != nil || r.Changes != (cache.Changes{Added: 1}) || r.Events != 3 {
		t.Errorf("report %+v, want full, 1 added, 3 events", r)
	}
	if n := listCalls(f, a) - list0; n != 1 {
		t.Errorf("%d listings, want 1", n)
	}
	if cal := cached(t, db, a); !cal.FullAt.Equal(t1) {
		t.Errorf("FullAt %v, want %v", cal.FullAt, t1)
	}
	matches(t, c, db, a)
}

// A cursor Proton refuses with a 4xx other than 429 is unusable: the
// calendar is listed again and its new baseline works.
func TestExpiredCursorTriggersFullResync(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 2)
	pass(t, c, db, at(t0))
	f.AddEvent(a, protontest.Event{UID: "new", Start: 5, End: 6})
	f.ExpireFeed(a) // 422/2001
	list0 := listCalls(f, a)
	r := report(t, pass(t, c, db, at(t0.Add(10*time.Minute))), a)
	if r.Mode != "full" || r.Err != nil || r.Changes != (cache.Changes{Added: 1}) || listCalls(f, a)-list0 != 1 {
		t.Errorf("expired cursor: report %+v, %d listings; want full, 1 added, 1 listing", r, listCalls(f, a)-list0)
	}
	matches(t, c, db, a)

	cur := cached(t, db, a).Cursor
	f.FailNext("GET", "/calendar/v1/"+a+"/modelevents/"+cur, http.StatusForbidden, 2011, 0)
	r = report(t, pass(t, c, db, at(t0.Add(20*time.Minute))), a)
	if r.Mode != "full" || r.Err != nil {
		t.Errorf("feed answering 403: report %+v, want full, no error", r)
	}

	late := f.AddEvent(a, protontest.Event{UID: "late", Start: 1, End: 2})
	r = report(t, pass(t, c, db, at(t0.Add(30*time.Minute))), a)
	if r.Mode != "incremental" || r.Changes != (cache.Changes{Added: 1}) || etags(t, db, a)[late] == "" {
		t.Errorf("after the resync: report %+v, want incremental adding %s", r, late)
	}
}

func TestRateLimitedFeedStopsThePass(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 1)
	pass(t, c, db, at(t0))
	before := cached(t, db, a)
	f.AddEvent(a, protontest.Event{UID: "new", Start: 5, End: 6})
	// Retry-After beyond the client's 60 s: it gives up at once.
	f.FailNext("GET", "/calendar/v1/"+a+"/modelevents/"+before.Cursor, http.StatusTooManyRequests, 0, 3600)
	list0 := listCalls(f, a)
	rep, err := syncer.Pass(t.Context(), c, db, at(t0.Add(time.Minute)))
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests || len(rep.Calendars) != 0 {
		t.Fatalf("Pass = %+v, %v; want the 429, no report", rep, err)
	}
	if after := cached(t, db, a); after.Cursor != before.Cursor || listCalls(f, a) != list0 || after.Error != "" {
		t.Errorf("after a 429: cached %+v, %d listings; want cursor %q kept, no listing, no calendar error",
			after, listCalls(f, a)-list0, before.Cursor)
	}
}

func TestStaleFullSyncRunsFull(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 2)
	pass(t, c, db, at(t0))
	t23 := t0.Add(23 * time.Hour)
	if r := report(t, pass(t, c, db, at(t23)), a); r.Mode != "incremental" {
		t.Errorf("23 h after the listing: mode %q, want incremental", r.Mode)
	}
	if cal := cached(t, db, a); !cal.FullAt.Equal(t0) {
		t.Errorf("FullAt %v after an incremental pass, want %v", cal.FullAt, t0)
	}
	t25 := t0.Add(25 * time.Hour)
	list0 := listCalls(f, a)
	if r := report(t, pass(t, c, db, at(t25)), a); r.Mode != "full" || listCalls(f, a)-list0 != 1 {
		t.Errorf("25 h after the listing: mode %q, %d listings; want full, 1", r.Mode, listCalls(f, a)-list0)
	}
	if cal := cached(t, db, a); !cal.FullAt.Equal(t25) {
		t.Errorf("FullAt %v, want %v", cal.FullAt, t25)
	}
	if r := report(t, pass(t, c, db, syncer.Options{Now: func() time.Time { return t25 }, FullEvery: time.Hour}), a); r.Mode != "incremental" {
		t.Errorf("FullEvery 1 h, listed just now: mode %q, want incremental", r.Mode)
	}
	if r := report(t, pass(t, c, db, syncer.Options{Now: func() time.Time { return t25.Add(2 * time.Hour) }, FullEvery: time.Hour}), a); r.Mode != "full" {
		t.Errorf("FullEvery 1 h, listed 2 h ago: mode %q, want full", r.Mode)
	}
	if r := report(t, pass(t, c, db, syncer.Options{Now: func() time.Time { return t25.Add(2 * time.Hour) }, Full: true}), a); r.Mode != "full" {
		t.Errorf("Full: mode %q, want full", r.Mode)
	}
}

// A session-wide failure while reading rows stores nothing of the
// calendar's delta: the cursor stays, and the next pass applies it all.
func TestFailureMidPassKeepsOldCursor(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 3)
	pass(t, c, db, at(t0))
	before := cached(t, db, a)
	rows := etags(t, db, a)
	f.AddEvent(a, protontest.Event{UID: "new", Start: 5, End: 6})
	f.UpdateEvent(a, ids[0], protontest.Event{UID: "u0", Start: 7, End: 8})
	f.DeleteEvent(a, ids[1])
	last := f.AddEvent(a, protontest.Event{UID: "last", Start: 9, End: 10}) // read last
	for range 3 {                                                           // the client retries a GET's 5xx three times
		f.FailNext("GET", "/calendar/v1/"+a+"/events/"+last, http.StatusServiceUnavailable, 0, 0)
	}
	rep, err := syncer.Pass(t.Context(), c, db, at(t0.Add(time.Minute)))
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable || len(rep.Calendars) != 0 {
		t.Fatalf("Pass = %+v, %v; want the 503, no report", rep, err)
	}
	after := cached(t, db, a)
	if after.Cursor != before.Cursor || !after.SyncedAt.Equal(t0) {
		t.Errorf("after the failure: cursor %q synced %v, want %q, %v", after.Cursor, after.SyncedAt, before.Cursor, t0)
	}
	if got := etags(t, db, a); len(got) != len(rows) || got[ids[0]] != rows[ids[0]] || got[ids[1]] == "" {
		t.Errorf("after the failure the rows changed: %v, want %v", got, rows)
	}
	r := report(t, pass(t, c, db, at(t0.Add(2*time.Minute))), a)
	if r.Mode != "incremental" || r.Err != nil || r.Changes != (cache.Changes{Added: 2, Updated: 1, Deleted: 1}) {
		t.Errorf("next pass %+v, want incremental, 2 added, 1 updated, 1 deleted", r)
	}
	matches(t, c, db, a)
}

func TestOneCalendarFailingOthersContinue(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Broken"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Fine"})
	addEvents(f, a, 2)
	addEvents(f, b, 3)
	f.FailNext("GET", "/calendar/v1/"+a+"/modelevents/latest", http.StatusForbidden, 2011, 0)
	rep, err := syncer.Pass(t.Context(), c, db, at(t0))
	if err != nil {
		t.Fatalf("Pass: %v, want the failure reported on the calendar", err)
	}
	ra, rb := report(t, rep, a), report(t, rep, b)
	var ae *api.Error
	if !errors.As(ra.Err, &ae) || ae.Status != http.StatusForbidden || ra.Events != 0 {
		t.Errorf("failing calendar: %+v, want the 403 and no events", ra)
	}
	if rb.Err != nil || rb.Events != 3 || rb.Mode != "initial" {
		t.Errorf("other calendar: %+v, want synced with 3 events", rb)
	}
	if ca := cached(t, db, a); ra.Err == nil || ca.Error != ra.Err.Error() || ca.Cursor != "" {
		t.Errorf("failing calendar cached %+v, want Error %v and no cursor", ca, ra.Err)
	}
	if cb := cached(t, db, b); cb.Error != "" {
		t.Errorf("other calendar cached Error %q, want none", cb.Error)
	}
	matches(t, c, db, b)

	ra = report(t, pass(t, c, db, at(t0.Add(time.Minute))), a)
	if ra.Err != nil || ra.Mode != "initial" || ra.Events != 2 {
		t.Errorf("next pass, recovered calendar: %+v, want an initial sync of 2 events", ra)
	}
	if ca := cached(t, db, a); ca.Error != "" {
		t.Errorf("after a successful sync the calendar's Error is still %q", ca.Error)
	}
	matches(t, c, db, a)
}

func TestCalendarRemovedIsDropped(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 1)
	// A calendar cached by an earlier pass that Proton no longer lists.
	ctx := t.Context()
	if err := db.PutCalendar(ctx, cache.Calendar{ID: "gone-cal", Name: "Old"}); err != nil {
		t.Fatal(err)
	}
	row := cache.Row{ID: "gone-ev", UID: "u", Start: 1, End: 2, Raw: []byte(`{"ID":"gone-ev"}`)}
	if _, err := db.ReplaceCalendar(ctx, "gone-cal", "cur", []cache.Row{row}, t0); err != nil {
		t.Fatal(err)
	}
	rep := pass(t, c, db, at(t0))
	if len(rep.Calendars) != 1 || rep.Calendars[0].ID != a {
		t.Errorf("reports %+v, want only %s", rep.Calendars, a)
	}
	cals, err := db.Calendars(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cals) != 1 || cals[0].ID != a {
		t.Errorf("cached calendars %+v, want only %s", cals, a)
	}
	if evs := etags(t, db, "gone-cal"); len(evs) != 0 {
		t.Errorf("rows of the dropped calendar remain: %v", evs)
	}
}

func TestNewCalendarGetsInitialSync(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 1)
	pass(t, c, db, at(t0))
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Work"})
	addEvents(f, b, 2)
	rep := pass(t, c, db, at(t0.Add(time.Minute)))
	if ra := report(t, rep, a); ra.Mode != "incremental" {
		t.Errorf("known calendar: mode %q, want incremental", ra.Mode)
	}
	if rb := report(t, rep, b); rb.Mode != "initial" || rb.Name != "Work" || rb.Changes != (cache.Changes{Added: 2}) || rb.Events != 2 {
		t.Errorf("new calendar: %+v, want an initial sync adding 2", rb)
	}
	if cb := cached(t, db, b); cb.Name != "Work" || cb.Cursor == "" {
		t.Errorf("new calendar cached %+v, want named with a cursor", cb)
	}
	matches(t, c, db, b)
}

// Ruling R8: a row Proton refuses to read (neither gone nor session-wide)
// makes the calendar fall back to a full listing in the same pass, rather
// than holding its cursor forever.
func TestRefusedRowReadTriggersFullResync(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 2)
	pass(t, c, db, at(t0))
	f.UpdateEvent(a, ids[0], protontest.Event{UID: "u0", Start: 7, End: 8})
	f.FailNext("GET", "/calendar/v1/"+a+"/events/"+ids[0], http.StatusForbidden, 2011, 0)
	list0 := listCalls(f, a)
	t1 := t0.Add(time.Hour)
	r := report(t, pass(t, c, db, at(t1)), a)
	if r.Mode != "full" || r.Err != nil || r.Changes != (cache.Changes{Updated: 1}) || listCalls(f, a)-list0 != 1 {
		t.Errorf("report %+v, %d listings; want full, 1 updated, no error, 1 listing", r, listCalls(f, a)-list0)
	}
	if cal := cached(t, db, a); cal.Error != "" || !cal.FullAt.Equal(t1) {
		t.Errorf("cached %+v, want no Error, FullAt %v", cal, t1)
	}
	matches(t, c, db, a) // the refused row is cached as listed
}

// A fallback full resync (Refresh, refused cursor, R8) runs at most once per
// calendar per 5 minutes; the first listing, Options.Full and the FullEvery
// age are not limited.
func TestFallbackResyncIsRateLimited(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 1)
	pass(t, c, db, at(t0))

	// Refresh 4 min after the listing: skipped, cursor kept, error recorded.
	before := cached(t, db, a)
	f.AddEvent(a, protontest.Event{UID: "new", Start: 5, End: 6})
	f.ForceRefresh(a)
	list0 := listCalls(f, a)
	rep, err := syncer.Pass(t.Context(), c, db, at(t0.Add(4*time.Minute)))
	if err != nil {
		t.Fatalf("Pass: %v, want the skip reported on the calendar", err)
	}
	r := report(t, rep, a)
	wantErr := "Proton asked to refresh the feed; retrying with a full listing after " + t0.Add(5*time.Minute).UTC().Format(time.RFC3339)
	var deferred *syncer.ResyncDeferredError
	if !errors.As(r.Err, &deferred) || !deferred.Until.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("4 min after a listing: Err %v, want a ResyncDeferredError until %v", r.Err, t0.Add(5*time.Minute))
	}
	if r.Err == nil || r.Err.Error() != wantErr || r.Changes != (cache.Changes{}) || listCalls(f, a) != list0 {
		t.Errorf("4 min after a listing: report %+v, %d listings; want Err %q, no change, no listing", r, listCalls(f, a)-list0, wantErr)
	}
	if after := cached(t, db, a); after.Cursor != before.Cursor || after.Error != wantErr || !after.SyncedAt.Equal(t0) {
		t.Errorf("cached %+v, want cursor %q kept, Error %q, SyncedAt %v", after, before.Cursor, wantErr, t0)
	}

	// 5 min after it: the resync runs and clears the error.
	f.ForceRefresh(a)
	r = report(t, pass(t, c, db, at(t0.Add(5*time.Minute))), a)
	if r.Mode != "full" || r.Err != nil || r.Changes != (cache.Changes{Added: 1}) || listCalls(f, a)-list0 != 1 {
		t.Errorf("5 min after the listing: report %+v, want full adding 1", r)
	}
	if cal := cached(t, db, a); cal.Error != "" {
		t.Errorf("Error %q after the resync, want none", cal.Error)
	}

	// Not limited: Options.Full and the FullEvery age, right after a listing.
	t6 := t0.Add(6 * time.Minute)
	if r := report(t, pass(t, c, db, syncer.Options{Now: func() time.Time { return t6 }, Full: true}), a); r.Mode != "full" || r.Err != nil {
		t.Errorf("Full 1 min after a listing: %+v, want full, no error", r)
	}
	t8 := t0.Add(8 * time.Minute)
	if r := report(t, pass(t, c, db, syncer.Options{Now: func() time.Time { return t8 }, FullEvery: time.Minute}), a); r.Mode != "full" || r.Err != nil {
		t.Errorf("FullEvery 1 min, listed 2 min ago: %+v, want full, no error", r)
	}
	if cal := cached(t, db, a); !cal.FullAt.Equal(t8) {
		t.Errorf("FullAt %v, want %v", cal.FullAt, t8)
	}
}

// More than events.MaxFeedPages feed pages: the calendar is listed instead.
func TestLongFeedTriggersFullResync(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 1)
	pass(t, c, db, at(t0))
	f.SetFeedPageSize(1)
	addEvents(f, a, events.MaxFeedPages+2)
	feed0, get0, list0 := feedCalls(f, a), getCalls(f, a), listCalls(f, a)
	r := report(t, pass(t, c, db, at(t0.Add(time.Hour))), a)
	if r.Mode != "full" || r.Err != nil || r.Changes.Added != events.MaxFeedPages+2 {
		t.Errorf("report %+v, want full adding %d", r, events.MaxFeedPages+2)
	}
	// 50 feed pages, then latest before and after the listing; no row read;
	// one listing.
	if feed, gets, lists := feedCalls(f, a)-feed0, getCalls(f, a)-get0, listCalls(f, a)-list0; feed != events.MaxFeedPages+2 || gets != 0 || lists != 1 {
		t.Errorf("requests: %d feed, %d row reads, %d listings; want %d, 0, 1", feed, gets, lists, events.MaxFeedPages+2)
	}
	matches(t, c, db, a)
}

// A delta spanning feed pages whose second page fails stores nothing: the
// first page's rows would be stored with a cursor past the unread ones.
func TestFeedSecondPageFailureKeepsOldCursor(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 1)
	pass(t, c, db, at(t0))
	before := cached(t, db, a)
	rows := etags(t, db, a)
	f.SetFeedPageSize(2)
	addEvents(f, a, 4)
	// The fake's cursor is "<salt>-<position>": page 1 ends 2 entries on.
	i := strings.LastIndex(before.Cursor, "-")
	pos, err := strconv.Atoi(before.Cursor[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	second := before.Cursor[:i+1] + strconv.Itoa(pos+2)
	for range 3 { // the client retries a GET's 5xx three times
		f.FailNext("GET", "/calendar/v1/"+a+"/modelevents/"+second, http.StatusServiceUnavailable, 0, 0)
	}
	feed0 := feedCalls(f, a)
	rep, err := syncer.Pass(t.Context(), c, db, at(t0.Add(time.Hour)))
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusServiceUnavailable || len(rep.Calendars) != 0 {
		t.Fatalf("Pass = %+v, %v; want the 503, no report", rep, err)
	}
	if n := feedCalls(f, a) - feed0; n != 4 { // page 1, then page 2 three times
		t.Fatalf("%d feed requests, want 4: the failure did not hit the second page", n)
	}
	if after := cached(t, db, a); after.Cursor != before.Cursor || len(etags(t, db, a)) != len(rows) {
		t.Errorf("after the failure: cursor %q, %d rows; want %q, %d", after.Cursor, len(etags(t, db, a)), before.Cursor, len(rows))
	}
	r := report(t, pass(t, c, db, at(t0.Add(2*time.Hour))), a)
	if r.Mode != "incremental" || r.Err != nil || r.Changes != (cache.Changes{Added: 4}) {
		t.Errorf("next pass %+v, want incremental adding 4", r)
	}
	matches(t, c, db, a)
}

// A cache that refuses a write fails the whole pass (ruling R13): it is not
// one calendar's problem, and the next calendar would hit it too.
func TestCacheWriteFailureStopsThePass(t *testing.T) {
	t.Parallel()
	f, c, _ := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "First"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Second"})
	addEvents(f, a, 1)
	addEvents(f, b, 1)
	path := cache.Path(t.TempDir())
	db, err := cache.Open(path, f.UserID(f.Username))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(t.Context(), `CREATE TRIGGER refuse BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'refused'); END`); err != nil {
		t.Fatal(err)
	}
	rep, err := syncer.Pass(t.Context(), c, db, at(t0))
	var se *syncer.StoreError
	if !errors.As(err, &se) || len(rep.Calendars) != 0 {
		t.Fatalf("Pass = %+v, %v; want a *syncer.StoreError and no report", rep, err)
	}
	if n := listCalls(f, b); n != 0 {
		t.Errorf("the pass went on to the second calendar (%d listings) after the cache refused a write", n)
	}
}

func feedPageCalls(f *protontest.Fake, cal string) int {
	return feedCalls(f, cal) - f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/latest")
}

// A deferred fallback resync names its cause, and while it waits for the
// FallbackGap the calendar's feed is not read again (it would only fail
// again, up to 50 pages a pass); once the gap is over the calendar is listed
// straight away.
func TestDeferredResyncNamesItsCauseAndSkipsTheFeed(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 1)
	pass(t, c, db, at(t0))
	listedAt := t0
	for _, step := range []struct {
		cause string
		make  func()
	}{
		{"Proton asked to refresh the feed", func() { f.ForceRefresh(a) }},
		{"Proton refused the feed cursor", func() { f.ExpireFeed(a) }},
		{"Proton refused to read a changed row", func() {
			f.UpdateEvent(a, ids[0], protontest.Event{UID: "u0", Start: 3, End: 4})
			f.FailNext("GET", "/calendar/v1/"+a+"/events/"+ids[0], http.StatusForbidden, 2011, 0)
		}},
		{"the feed holds more than 50 pages", func() {
			f.SetFeedPageSize(1)
			addEvents(f, a, events.MaxFeedPages+1)
		}},
	} {
		step.make()
		until := listedAt.Add(syncer.FallbackGap)
		want := step.cause + "; retrying with a full listing after " + until.UTC().Format(time.RFC3339)
		r := report(t, pass(t, c, db, at(listedAt.Add(time.Minute))), a)
		var de *syncer.ResyncDeferredError
		if !errors.As(r.Err, &de) || r.Err.Error() != want {
			t.Fatalf("%s: Err %v, want %q", step.cause, r.Err, want)
		}
		if got := cached(t, db, a).Error; got != want {
			t.Errorf("%s: cached Error %q, want %q", step.cause, got, want)
		}
		// Within the gap: nothing is read for this calendar.
		feed0, get0, list0 := feedPageCalls(f, a), getCalls(f, a), listCalls(f, a)
		r = report(t, pass(t, c, db, at(listedAt.Add(2*time.Minute))), a)
		if !errors.As(r.Err, &de) || r.Err.Error() != want {
			t.Errorf("%s, still within the gap: Err %v, want %q", step.cause, r.Err, want)
		}
		if feed, gets, lists := feedPageCalls(f, a)-feed0, getCalls(f, a)-get0, listCalls(f, a)-list0; feed+gets+lists != 0 {
			t.Errorf("%s, within the gap: %d feed pages, %d row reads, %d listings; want none", step.cause, feed, gets, lists)
		}
		// After it: listed at once, without reading the feed first.
		listedAt = until
		r = report(t, pass(t, c, db, at(listedAt)), a)
		if r.Mode != "full" || r.Err != nil || feedPageCalls(f, a) != feed0 || listCalls(f, a)-list0 != 1 {
			t.Errorf("%s, after the gap: report %+v, %d feed pages, %d listings; want full, no error, 0, 1",
				step.cause, r, feedPageCalls(f, a)-feed0, listCalls(f, a)-list0)
		}
		if cal := cached(t, db, a); cal.Error != "" || !cal.FullAt.Equal(listedAt) {
			t.Errorf("%s, after the resync: cached %+v, want no Error, FullAt %v", step.cause, cal, listedAt)
		}
		matches(t, c, db, a)
	}
	// The resync is not pending any more: the next pass reads the feed.
	f.SetFeedPageSize(0)
	if r := report(t, pass(t, c, db, at(listedAt.Add(time.Minute))), a); r.Mode != "incremental" || r.Err != nil {
		t.Errorf("after the last resync: %+v, want incremental", r)
	}
}

func latestCalls(f *protontest.Fake, cal string) int {
	return f.Calls("GET", "/calendar/v1/"+cal+"/modelevents/latest")
}

// The listing pages by offset: a row deleted or inserted before a page
// already served shifts the later pages, so one unchanged row is skipped
// (or one read twice). Ruling R17: an unstable listing is repeated and the
// first stable one is the content; a quiet listing is listed once.
func TestListingShiftedByAConcurrentEditIsRepeated(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 150) // two pages
	// The review's scenario on the FIRST listing: ids[0] is deleted while
	// page 0 is served (after its rows were read), so page 1 starts one row
	// later and ids[100] is in neither page.
	once := func(fn func()) {
		done := false
		f.OnListPage(func(calID string, page int) {
			if calID == a && page == 0 && !done {
				done = true
				fn()
			}
		})
	}
	once(func() { f.DeleteEvent(a, ids[0]) })
	list0 := listCalls(f, a)
	r := report(t, pass(t, c, db, at(t0)), a)
	f.OnListPage(nil)
	if r.Mode != "initial" || r.Err != nil || r.Events != 149 || r.Changes != (cache.Changes{Added: 149}) {
		t.Errorf("initial listing shifted by a delete: %+v, want the 149 survivors added", r)
	}
	if got := etags(t, db, a); got[ids[100]] == "" || got[ids[0]] != "" {
		t.Errorf("survivor %s cached %v, deleted %s cached %v; want every survivor and not the deleted row",
			ids[100], got[ids[100]] != "", ids[0], got[ids[0]] != "")
	}
	if n := listCalls(f, a) - list0; n != 4 {
		t.Errorf("%d listing pages, want 4: the shifted listing and one stable repeat", n)
	}
	matches(t, c, db, a)
	full := func(now time.Time) syncer.Options {
		return syncer.Options{Now: func() time.Time { return now }, Full: true}
	}

	// Quiet: one listing (two pages), latest before and after, no row read.
	list0, latest0, get0 := listCalls(f, a), latestCalls(f, a), getCalls(f, a)
	if r := report(t, pass(t, c, db, full(t0.Add(time.Hour))), a); r.Err != nil || r.Changes != (cache.Changes{}) {
		t.Errorf("quiet listing: %+v, want no change", r)
	}
	if lists, latests, gets := listCalls(f, a)-list0, latestCalls(f, a)-latest0, getCalls(f, a)-get0; lists != 2 || latests != 2 || gets != 0 {
		t.Errorf("quiet listing: %d pages, %d latest, %d row reads; want 2, 2, 0", lists, latests, gets)
	}

	// The same on a cached calendar: the unchanged row skipped by the shift
	// is not tombstoned, the deleted one is, and no row is read.
	once(func() { f.DeleteEvent(a, ids[1]) })
	get0 = getCalls(f, a)
	r = report(t, pass(t, c, db, full(t0.Add(2*time.Hour))), a)
	f.OnListPage(nil)
	if r.Err != nil || r.Changes != (cache.Changes{Deleted: 1}) || r.Events != 148 || getCalls(f, a) != get0 {
		t.Errorf("delete during a listing: %+v, %d row reads; want %s deleted, 148 events, none", r, getCalls(f, a)-get0, ids[1])
	}
	matches(t, c, db, a)

	// Inserted at the front while page 0 is served: page 1 repeats a row;
	// the repeat listing holds the new row.
	var front string
	once(func() { front = f.AddEventAt(a, 0, protontest.Event{UID: "front", Start: 1, End: 2}) })
	r = report(t, pass(t, c, db, full(t0.Add(3*time.Hour))), a)
	f.OnListPage(nil)
	if r.Err != nil || r.Changes != (cache.Changes{Added: 1}) || r.Events != 149 || etags(t, db, a)[front] == "" {
		t.Errorf("insert during a listing: %+v, want %s added, 149 events", r, front)
	}
	matches(t, c, db, a)
}

// When no listing settles (MaxListings in a row shifted), the content is
// the union of their rows: a row one listing skipped, another holds. A
// cached row missing from all of them is re-read: gone is tombstoned, a
// refused read keeps it as cached with a note, the rest is stored.
func TestListingThatNeverSettlesUsesTheUnion(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 150)
	every := func(fn func(n int)) *int {
		n := new(int)
		f.OnListPage(func(calID string, page int) {
			if calID == a && page == 0 {
				fn(*n)
				*n++
			}
		})
		return n
	}
	// Initial: each listing loses a row to a delete, so each skips another.
	n := every(func(i int) { f.DeleteEvent(a, ids[i]) })
	r := report(t, pass(t, c, db, at(t0)), a)
	f.OnListPage(nil)
	if *n != syncer.MaxListings || r.Err != nil {
		t.Fatalf("%d listings, report %+v; want %d, no error", *n, r, syncer.MaxListings)
	}
	got := etags(t, db, a)
	for _, id := range ids[syncer.MaxListings:] {
		if got[id] == "" {
			t.Errorf("survivor %s missing: the union lost it", id)
		}
	}
	// The deleted rows the listings read before their delete are in the
	// union; the feed removes them.
	if r := report(t, pass(t, c, db, at(t0.Add(time.Minute))), a); r.Mode != "incremental" || r.Changes.Deleted != syncer.MaxListings {
		t.Errorf("next pass: %+v, want incremental deleting the %d rows", r, syncer.MaxListings)
	}
	matches(t, c, db, a)

	// Cached rows absent from every listing: one gone, one Proton refuses
	// to read (kept as cached, with a note); a row created during each
	// listing keeps them all unstable.
	gone, refused := ids[10], ids[20]
	f.DeleteEvent(a, gone)
	f.DeleteEvent(a, refused)
	f.FailNext("GET", "/calendar/v1/"+a+"/events/"+refused, http.StatusForbidden, 2011, 0)
	every(func(int) { f.AddEvent(a, protontest.Event{UID: "late", Start: 1, End: 2}) })
	r = report(t, pass(t, c, db, syncer.Options{Now: func() time.Time { return t0.Add(time.Hour) }, Full: true}), a)
	f.OnListPage(nil)
	var ue *syncer.UnverifiedError
	if !errors.As(r.Err, &ue) || ue.Rows != 1 || r.Changes.Deleted != 1 {
		t.Errorf("unverifiable row: %+v, want an UnverifiedError for 1 row and %s tombstoned", r, gone)
	}
	got = etags(t, db, a)
	if got[refused] == "" || got[gone] != "" {
		t.Errorf("refused %s cached %v, gone %s cached %v; want the refused row kept, the gone one tombstoned",
			refused, got[refused] != "", gone, got[gone] != "")
	}
	if cal := cached(t, db, a); !cal.FullAt.Equal(t0.Add(time.Hour)) || cal.Error != ue.Error() {
		t.Errorf("cached %+v, want the listing stored (FullAt %v) and the note recorded", cal, t0.Add(time.Hour))
	}
}

// A calendar Proton no longer lists is dropped under its lock (Options.Lock),
// as its pass would run: a write of it never stores rows beside the drop.
// A lock that cannot be taken ends the pass with its error.
func TestCalendarDropRunsUnderItsLock(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ctx := t.Context()
	if err := db.PutCalendar(ctx, cache.Calendar{ID: "gone-cal", Name: "Old"}); err != nil {
		t.Fatal(err)
	}
	cached := func() bool {
		cals, err := db.Calendars(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(cals, func(c cache.Calendar) bool { return c.ID == "gone-cal" })
	}
	var locked []string
	o := at(t0)
	o.Lock = func(_ context.Context, calID string) (func(), error) {
		locked = append(locked, calID)
		if calID == "gone-cal" && !cached() {
			t.Error("the calendar was dropped before its lock was taken")
		}
		return func() {
			if calID == "gone-cal" && cached() {
				t.Error("the lock was released before the drop")
			}
		}, nil
	}
	pass(t, c, db, o)
	if !slices.Equal(locked, []string{"gone-cal", a}) {
		t.Errorf("locked %v, want gone-cal then %s", locked, a)
	}
	if cached() {
		t.Error("the calendar was not dropped")
	}

	stop := errors.New("lock not taken")
	o.Lock = func(context.Context, string) (func(), error) { return nil, stop }
	if _, err := syncer.Pass(ctx, c, db, o); !errors.Is(err, stop) {
		t.Errorf("Pass with a lock that fails = %v, want its error", err)
	}
}
