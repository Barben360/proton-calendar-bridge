package service_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"

	_ "modernc.org/sqlite" // the "sqlite" driver, to damage a cache file
)

const day = 24 * 3600

// base is 2027-01-15 08:00 UTC, the start of the test events.
const base = int64(1_800_000_000)

func (e *env) sync(t *testing.T, full bool) service.SyncResult {
	t.Helper()
	res, err := e.svc.Sync(t.Context(), service.SyncRequest{Full: full})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return res
}

func (e *env) events(t *testing.T, req service.EventsRequest) []service.Event {
	t.Helper()
	evs, err := e.svc.Events(t.Context(), req)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	return evs
}

func calSync(t *testing.T, res service.SyncResult, id string) service.CalendarSync {
	t.Helper()
	for _, c := range res.Calendars {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no calendar %s in %+v", id, res)
	return service.CalendarSync{}
}

func TestSyncThenEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	a := e.f.AddEvent(work, protontest.Event{UID: "a@x", Start: base, End: base + 3600})
	b := e.f.AddEvent(work, protontest.Event{UID: "b@x", Start: base + day, End: base + 2*day, FullDay: true})
	e.login(t, e.req())

	first := calSync(t, e.sync(t, false), work)
	if first.Name != "Work" || first.Mode != "initial" || first.Added != 2 || first.Events != 2 || first.Error != "" {
		t.Fatalf("first sync = %+v", first)
	}
	calls := e.f.Calls("", "/")
	evs := e.events(t, service.EventsRequest{Calendar: "Work"})
	if n := e.f.Calls("", "/"); n != calls {
		t.Errorf("Events made %d requests, want none", n-calls)
	}
	if len(evs) != 2 || evs[0].ID != a || evs[0].UID != "a@x" || evs[0].CalendarID != work ||
		!evs[0].Start.Equal(time.Unix(base, 0)) || !evs[0].End.Equal(time.Unix(base+3600, 0)) || evs[0].AllDay || evs[0].Recurring ||
		evs[0].ETag == "" || evs[0].LastEdit.IsZero() {
		t.Fatalf("events = %+v", evs)
	}
	if evs[1].ID != b || !evs[1].AllDay {
		t.Errorf("second event = %+v, want the all-day one", evs[1])
	}

	e.f.UpdateEvent(work, a, protontest.Event{UID: "a@x", Start: base + 7200, End: base + 9000})
	e.f.DeleteEvent(work, b)
	e.f.AddEvent(work, protontest.Event{UID: "c@x", Start: base + 3*day, End: base + 3*day + 60})
	second := calSync(t, e.sync(t, false), work)
	if second.Mode != "incremental" || second.Added != 1 || second.Updated != 1 || second.Deleted != 1 || second.Events != 2 {
		t.Fatalf("second sync = %+v", second)
	}
	if full := calSync(t, e.sync(t, true), work); full.Mode != "full" || full.Added+full.Updated+full.Deleted != 0 || full.Events != 2 {
		t.Fatalf("--full sync of an unchanged calendar = %+v", full)
	}
	// By ID this time.
	evs = e.events(t, service.EventsRequest{Calendar: work})
	if len(evs) != 2 || evs[0].ID != a || !evs[0].Start.Equal(time.Unix(base+7200, 0)) || evs[1].UID != "c@x" {
		t.Fatalf("events after the change = %+v", evs)
	}
}

func TestSecondSyncIsBlocked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req())
	unlock, ok, err := filelock.TryLock(filepath.Join(e.dirs.State, "sync.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	calls := e.f.Calls("", "/")
	_, err = e.svc.Sync(t.Context(), service.SyncRequest{})
	ee := wantExit(t, err, exitcode.Blocked)
	if !strings.Contains(ee.Error(), "a sync is already running") || !strings.Contains(ee.Hint, "pcb sync --watch") {
		t.Errorf("error = %q, hint = %q", ee.Error(), ee.Hint)
	}
	err = e.svc.WatchSync(t.Context(), service.SyncRequest{}, 0, nil)
	_ = wantExit(t, err, exitcode.Blocked)
	if n := e.f.Calls("", "/"); n != calls {
		t.Errorf("a blocked sync made %d requests", n-calls)
	}
	unlock()
	e.sync(t, false) // the lock is free again
}

func TestSyncPartialExits40(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	bad := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Bad"})
	good := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Good"})
	e.f.AddEvent(good, protontest.Event{UID: "g@x", Start: base, End: base + 60})
	e.login(t, e.req())
	e.f.FailNext("GET", "/calendar/v1/"+bad+"/events", 403, 2011, 0)
	res, err := e.svc.Sync(t.Context(), service.SyncRequest{})
	ee := wantExit(t, err, exitcode.Partial)
	if ee.Hint == "" {
		t.Error("partial error has no hint")
	}
	if b := calSync(t, res, bad); b.Error == "" || b.Events != 0 {
		t.Errorf("bad calendar = %+v, want an error", b)
	}
	if g := calSync(t, res, good); g.Error != "" || g.Events != 1 || g.Added != 1 {
		t.Errorf("good calendar = %+v, want synced", g)
	}
	a := e.account(t)
	for _, s := range []string{e.f.Password, a.AccessToken, a.RefreshToken, string(a.KeyPass)} {
		if strings.Contains(res.Calendars[0].Error+res.Calendars[1].Error+ee.Error(), s) {
			t.Error("the result contains a secret")
		}
	}
	// The failure is cached: status shows it, and the next pass heals it.
	st, err := e.svc.Status(t.Context())
	if err != nil || st.Sync == nil || len(st.Sync.Calendars) != 2 {
		t.Fatalf("status = %+v, %v", st.Sync, err)
	}
	// Status shows the same secret-free text as the pass, not the raw error
	// (request path, Proton's own message).
	want := calSync(t, res, bad).Error
	for _, c := range st.Sync.Calendars {
		if c.ID == bad && (c.Error != want || strings.Contains(c.Error, "/calendar/v1/")) {
			t.Errorf("status error %q, want the pass's %q", c.Error, want)
		}
	}
	healed := calSync(t, e.sync(t, false), bad)
	if healed.Error != "" || healed.Mode != "initial" {
		t.Errorf("after retry = %+v", healed)
	}
}

// Ruling R11: a calendar skipped by the 5-minute fallback gap is partial too.
func TestSyncDeferredResyncIsPartial(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	cal := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	e.sync(t, false)
	e.f.ForceRefresh(cal) // the clock did not move: the fallback listing is deferred
	res, err := e.svc.Sync(t.Context(), service.SyncRequest{})
	_ = wantExit(t, err, exitcode.Partial)
	if c := calSync(t, res, cal); c.Error == "" || c.Events != 1 {
		t.Errorf("calendar = %+v, want the deferral reported and the rows kept", c)
	}
}

// TestReadPaths runs the read-only checks of the cache (Status, Events) on
// one logged-in env, one SRP login for all (CI cost): before any sync, in
// order; after it, in parallel subtests that only read the shared cache.
// Each case adds its own calendars before the login.
func TestReadPaths(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	if st, _ := e.svc.Status(t.Context()); st.Sync != nil {
		t.Fatalf("sync section without a cache: %+v", st.Sync)
	}
	// Calendar selection: two calendars share a name.
	w1 := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Twin"})
	w2 := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Twin"})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Solo"})
	// Windows: instants ("Work") and dates ("AllDay").
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	rec := base + 2*day
	ids := map[string]string{
		"early":   e.f.AddEvent(work, protontest.Event{UID: "early", Start: base - 5*day, End: base - 5*day + 3600}),
		"master":  e.f.AddEvent(work, protontest.Event{UID: "master", Start: base - 10*day, End: base - 10*day + 3600, RRule: "FREQ=DAILY"}),
		"future":  e.f.AddEvent(work, protontest.Event{UID: "future", Start: base + 10*day, End: base + 10*day + 3600, RRule: "FREQ=DAILY"}),
		"span":    e.f.AddEvent(work, protontest.Event{UID: "span", Start: base - day, End: base + day}),
		"inside":  e.f.AddEvent(work, protontest.Event{UID: "inside", Start: base + day, End: base + day + 3600}),
		"except":  e.f.AddEvent(work, protontest.Event{UID: "master", Start: base + 2*day, End: base + 2*day + 3600, RecurrenceID: &rec}),
		"late":    e.f.AddEvent(work, protontest.Event{UID: "late", Start: base + 7*day, End: base + 7*day + 3600}),
		"touches": e.f.AddEvent(work, protontest.Event{UID: "touches", Start: base + 3*day, End: base + 3*day + 3600}),
	}
	allDay := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "AllDay"})
	d := func(day int) int64 { return time.Date(2027, 1, day, 0, 0, 0, 0, time.UTC).Unix() }
	dayIDs := map[string]string{
		"jan15":  e.f.AddEvent(allDay, protontest.Event{UID: "15", Start: d(15), End: d(16), FullDay: true}),
		"jan16":  e.f.AddEvent(allDay, protontest.Event{UID: "16", Start: d(16), End: d(17), FullDay: true}),
		"jan17":  e.f.AddEvent(allDay, protontest.Event{UID: "17", Start: d(17), End: d(19), FullDay: true}),
		"master": e.f.AddEvent(allDay, protontest.Event{UID: "m", Start: d(1), End: d(2), FullDay: true, RRule: "FREQ=WEEKLY"}),
	}
	// Status: one calendar with one event.
	status := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Status"})
	e.f.AddEvent(status, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())

	t.Run("events without a cache", func(t *testing.T) {
		before := dirNames(t, e.dirs.State)
		_, _ = e.svc.Status(t.Context())
		_, err := e.svc.Events(t.Context(), service.EventsRequest{Calendar: "Work"})
		if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "pcb sync") {
			t.Errorf("hint = %q, want pcb sync", ee.Hint)
		}
		if _, err := os.Stat(cache.Path(e.dirs.State)); !os.IsNotExist(err) {
			t.Errorf("Events created the cache file: %v", err)
		}
		// The read paths never create, migrate or empty the cache.
		if after := dirNames(t, e.dirs.State); after != before {
			t.Errorf("state directory changed: %q -> %q", before, after)
		}
	})
	e.sync(t, false)
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Later"}) // on Proton, not in the cache

	t.Run("calendar selection", func(t *testing.T) {
		t.Parallel()

		_, err := e.svc.Events(t.Context(), service.EventsRequest{Calendar: "Twin"})
		ee := wantExit(t, err, exitcode.Usage)
		if !strings.Contains(ee.Error(), w1) || !strings.Contains(ee.Error(), w2) {
			t.Errorf("ambiguous error %q does not list both IDs", ee.Error())
		}
		_, err = e.svc.Events(t.Context(), service.EventsRequest{Calendar: "Nope"})
		ee = wantExit(t, err, exitcode.Usage)
		if !strings.Contains(ee.Error(), `no calendar named "Nope"`) || !strings.Contains(ee.Hint, "pcb calendar list") {
			t.Errorf("error = %q, hint = %q", ee.Error(), ee.Hint)
		}
		// A calendar Proton has but the cache does not (added since the last
		// sync) asks for pcb sync, not for a check of its name.
		_, err = e.svc.Events(t.Context(), service.EventsRequest{Calendar: "Later"})
		if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "not synced yet") || ee.Hint != "run pcb sync" {
			t.Errorf("not yet synced: error %q, hint %q", ee.Error(), ee.Hint)
		}
		_, err = e.svc.Events(t.Context(), service.EventsRequest{})
		_ = wantExit(t, err, exitcode.Usage)
		if evs := e.events(t, service.EventsRequest{Calendar: w2}); evs == nil {
			t.Error("an empty calendar returned nil, want an empty list")
		}
		e.events(t, service.EventsRequest{Calendar: "Solo"})
	})
	t.Run("window", func(t *testing.T) {
		t.Parallel()
		from, to := time.Unix(base, 0), time.Unix(base+3*day, 0)
		got := map[string]bool{}
		for _, ev := range e.events(t, service.EventsRequest{Calendar: "Work", From: &from, To: &to}) {
			got[ev.ID] = true
		}
		want := map[string]bool{ids["master"]: true, ids["span"]: true, ids["inside"]: true, ids["except"]: true}
		for name, id := range ids {
			if got[id] != want[id] {
				t.Errorf("event %s in window = %v, want %v", name, got[id], want[id])
			}
		}
		// Only From: everything ending after it, masters included.
		got = map[string]bool{}
		for _, ev := range e.events(t, service.EventsRequest{Calendar: "Work", From: &from}) {
			got[ev.ID] = true
		}
		if got[ids["early"]] || !got[ids["late"]] || !got[ids["future"]] || !got[ids["master"]] {
			t.Errorf("From-only window = %v", got)
		}
		// Only To: a master starting at or after To is out.
		got = map[string]bool{}
		for _, ev := range e.events(t, service.EventsRequest{Calendar: "Work", To: &to}) {
			got[ev.ID] = true
		}
		if !got[ids["early"]] || !got[ids["master"]] || got[ids["future"]] || got[ids["late"]] {
			t.Errorf("To-only window = %v", got)
		}
		// Recurring is reported.
		for _, ev := range e.events(t, service.EventsRequest{Calendar: "Work"}) {
			if ev.ID == ids["master"] && !ev.Recurring {
				t.Error("the master is not marked recurring")
			}
			if ev.ID == ids["except"] && (ev.Recurring || ev.RecurrenceID == nil || !ev.RecurrenceID.Equal(time.Unix(rec, 0))) {
				t.Errorf("exception = %+v", ev)
			}
		}
	})
	t.Run("all-day window by date", func(t *testing.T) {
		t.Parallel()
		keep := func(req service.EventsRequest) map[string]bool {
			req.Calendar = "AllDay"
			got := map[string]bool{}
			for _, ev := range e.events(t, req) {
				got[ev.ID] = true
			}
			return got
		}
		// Window [Jan 16, Jan 17): only the 16th, and the master.
		got := keep(service.EventsRequest{FromDate: "2027-01-16", ToDate: "2027-01-17"})
		for name, want := range map[string]bool{"jan15": false, "jan16": true, "jan17": false, "master": true} {
			if got[dayIDs[name]] != want {
				t.Errorf("date window: %s kept = %v, want %v", name, got[dayIDs[name]], want)
			}
		}
		// A multi-day event overlapping from the 18th.
		got = keep(service.EventsRequest{FromDate: "2027-01-18"})
		if got[dayIDs["jan16"]] || !got[dayIDs["jan17"]] {
			t.Errorf("From 2027-01-18: %v", got)
		}
		// An RFC 3339 bound compares instants, even for all-day events.
		to := time.Date(2027, 1, 16, 12, 0, 0, 0, time.UTC)
		got = keep(service.EventsRequest{To: &to})
		if !got[dayIDs["jan16"]] || got[dayIDs["jan17"]] {
			t.Errorf("To noon Jan 16: %v", got)
		}
		// Dates are reported for all-day rows, the end being exclusive.
		for _, ev := range e.events(t, service.EventsRequest{Calendar: "AllDay"}) {
			if ev.ID == dayIDs["jan17"] && (!ev.AllDay || ev.StartDate != "2027-01-17" || ev.EndDate != "2027-01-19") {
				t.Errorf("jan17 = %+v", ev)
			}
		}
		_, err := e.svc.Events(t.Context(), service.EventsRequest{Calendar: "AllDay", FromDate: "01/16/2027"})
		_ = wantExit(t, err, exitcode.Usage)
	})
	t.Run("status section", func(t *testing.T) {
		t.Parallel()
		st, err := e.svc.Status(t.Context())
		if err != nil || st.Sync == nil {
			t.Fatalf("status = %+v, %v", st.Sync, err)
		}
		i := slices.IndexFunc(st.Sync.Calendars, func(c service.CalendarStatus) bool { return c.ID == status })
		if i < 0 || len(st.Sync.Calendars) != 6 {
			t.Fatalf("status calendars = %+v, want the 6 synced ones", st.Sync.Calendars)
		}
		c := st.Sync.Calendars[i]
		if c.Name != "Status" || c.Events != 1 || c.SyncedAt == nil || !c.SyncedAt.Equal(t0) || c.FullAt == nil || c.Error != "" {
			t.Errorf("calendar status = %+v", c)
		}
	})
}

func TestLogoutDeletesCache(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	cal := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	e.sync(t, false)
	path := cache.Path(e.dirs.State)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no cache after a sync: %v", err)
	}
	if _, err := e.svc.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the logout: %v", p, err)
		}
	}
	if st, err := e.svc.Status(t.Context()); err != nil || st.Sync != nil {
		t.Errorf("status after logout = %+v, %v; want no sync section", st.Sync, err)
	}
}

func TestWatchStopsWhenLoginNeeded(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req()) // no unattended relogin: a person is needed once the session ends
	e.f.RevokeSessions()
	passes := 0
	err := e.svc.WatchSync(t.Context(), service.SyncRequest{}, 0, func(p service.WatchPass) {
		err := p.Err
		passes++
		_ = wantExit(t, err, exitcode.MustAct)
	})
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Hint, "pcb login") {
		t.Errorf("hint = %q", ee.Hint)
	}
	if passes != 1 {
		t.Errorf("%d passes, want 1", passes)
	}
	if len(e.clk.slept) != 0 {
		t.Errorf("the watch slept %v after a MustAct", e.clk.slept)
	}
}

func TestWatchRunsPassesUntilCancelled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	cal := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var modes []string
	err := e.svc.WatchSync(ctx, service.SyncRequest{}, 0, func(p service.WatchPass) {
		res, err := p.Result, p.Err
		if err != nil {
			t.Errorf("pass: %v", err)
		}
		modes = append(modes, calSync(t, res, cal).Mode)
		if len(modes) == 2 {
			cancel()
		}
	})
	if err != nil {
		t.Errorf("WatchSync after the caller cancelled = %v, want nil (a normal stop)", err)
	}
	if len(modes) != 2 || modes[0] != "initial" || modes[1] != "incremental" {
		t.Errorf("modes = %v", modes)
	}
	if len(e.clk.slept) == 0 || e.clk.slept[0] != 30*time.Second {
		t.Errorf("slept %v, want the 30 s default interval", e.clk.slept)
	}

	// --full lists in full until one pass gets through, then reads the feed:
	// a first pass failing session-wide (503, retried 3 times) keeps it.
	for range 3 {
		e.f.FailNext("GET", "/calendar/v1", 503, 0, 0)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	var outcomes []string
	err = e.svc.WatchSync(ctx, service.SyncRequest{Full: true}, 0, func(p service.WatchPass) {
		res, err := p.Result, p.Err
		if err != nil {
			outcomes = append(outcomes, "failed")
		} else {
			outcomes = append(outcomes, calSync(t, res, cal).Mode)
		}
		if len(outcomes) == 3 {
			cancel()
		}
	})
	if err != nil || !slices.Equal(outcomes, []string{"failed", "full", "incremental"}) {
		t.Errorf("watch --full: %v, passes %v; want nil, failed then full then incremental", err, outcomes)
	}
}

// A partial pass is reported but is no session-wide failure: the watch
// does not back off, so the calendars that sync keep their interval.
func TestWatchKeepsGoingAfterPartial(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	bad := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Bad"})
	e.login(t, e.req())
	for range 2 {
		e.f.FailNext("GET", "/calendar/v1/"+bad+"/events", 403, 2011, 0)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var codes []exitcode.Code
	_ = e.svc.WatchSync(ctx, service.SyncRequest{}, 0, func(p service.WatchPass) {
		err := p.Err
		codes = append(codes, exitcode.From(err))
		if len(codes) == 3 {
			cancel()
		}
	})
	if len(codes) != 3 || codes[0] != exitcode.Partial || codes[1] != exitcode.Partial || codes[2] != exitcode.OK {
		t.Errorf("pass outcomes = %v, want partial, partial, ok", codes)
	}
	if want := slices.Repeat([]time.Duration{30 * time.Second}, 3); !slices.Equal(e.clk.slept, want) {
		t.Errorf("slept %v, want %v: a partial pass must not back off", e.clk.slept, want)
	}
}

func TestWatchRefusesShortInterval(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	calls := e.f.Calls("", "/")
	err := e.svc.WatchSync(t.Context(), service.SyncRequest{}, 9*time.Second, nil)
	_ = wantExit(t, err, exitcode.Usage)
	if n := e.f.Calls("", "/"); n != calls {
		t.Errorf("a refused watch made %d requests", n-calls)
	}
}

func TestLogoutDeletesCacheEvenWhenCancelled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	cal := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	e.sync(t, false)
	path := cache.Path(e.dirs.State)
	for _, p := range []string{path + "-wal", path + "-shm"} { // leftovers of a crashed writer
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := e.svc.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived a cancelled logout: %v", p, err)
		}
	}
}

func dirNames(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return strings.Join(names, ",")
}

func TestStatusReportsAnUnreadableCache(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req())
	e.sync(t, false)
	if err := os.WriteFile(cache.Path(e.dirs.State), []byte(strings.Repeat("not sqlite ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.Status(t.Context())
	if err != nil || st.Sync == nil || st.Sync.Error == "" || len(st.Sync.Calendars) != 0 {
		t.Fatalf("status = %+v, %v; want a sync error", st.Sync, err)
	}
	_, err = e.svc.Events(t.Context(), service.EventsRequest{Calendar: "Work"})
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "pcb sync --full") {
		t.Errorf("hint = %q", ee.Hint)
	}
	// A watch stops on it: a person must remove the file.
	err = e.svc.WatchSync(t.Context(), service.SyncRequest{}, 0, nil)
	_ = wantExit(t, err, exitcode.MustAct)
}

// damageTables overwrites the root pages of the named tables (and their
// indexes) with garbage, leaving the header and meta readable.
func damageTables(t *testing.T, path string, tables ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	if err := db.QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&size); err != nil {
		t.Fatal(err)
	}
	var roots []int64
	for _, tbl := range tables {
		rows, err := db.QueryContext(t.Context(), `SELECT rootpage FROM sqlite_schema WHERE tbl_name = ?`, tbl)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var r int64
			if err := rows.Scan(&r); err != nil {
				t.Fatal(err)
			}
			roots = append(roots, r)
		}
		_ = rows.Close()
	}
	if err := db.Close(); err != nil || len(roots) < len(tables) {
		t.Fatalf("roots %v, close %v", roots, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	for _, r := range roots {
		if _, err := f.WriteAt([]byte(strings.Repeat("\xff", int(size))), (r-1)*size); err != nil {
			t.Fatal(err)
		}
	}
}

// In a watch, a 429 whose Retry-After is past the client's own 60 s is
// waited out (up to 1 h) instead of retried after the back-off; a cache
// written by a newer pcb stops the watch (no retry can help); so does a
// cache whose tables are damaged (ruling R13 during a pass, not only at
// Open: MustAct with the way out). One env for the three (CI cost).
func TestWatchRetryAfterAndCachesNoRetryFixes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	cal := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	e.f.FailNext("GET", "/calendar/v1", 429, 0, 20*60)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var passes []service.WatchPass
	err := e.svc.WatchSync(ctx, service.SyncRequest{}, 0, func(p service.WatchPass) {
		if passes = append(passes, p); len(passes) == 2 {
			cancel()
		}
	})
	if err != nil || len(passes) != 2 || exitcode.From(passes[0].Err) != exitcode.Blocked || passes[1].Err != nil {
		t.Fatalf("WatchSync = %v, passes %+v; want a blocked pass then a good one", err, passes)
	}
	// The pass is told when the next one runs; the one-shot "retry in"
	// hint is dropped (the watch retries by itself).
	if passes[0].Next != 20*time.Minute || exitcode.HintOf(passes[0].Err) != "" {
		t.Errorf("rate-limited pass: next %v, hint %q; want 20m0s and no hint", passes[0].Next, exitcode.HintOf(passes[0].Err))
	}
	if len(e.clk.slept) == 0 || e.clk.slept[0] != 20*time.Minute {
		t.Errorf("slept %v, want the 20 min Proton asked for first", e.clk.slept)
	}

	db, err := sql.Open("sqlite", cache.Path(e.dirs.State))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	slept := len(e.clk.slept)
	passes = nil
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	err = e.svc.WatchSync(ctx, service.SyncRequest{}, 0, func(p service.WatchPass) {
		if passes = append(passes, p); len(passes) == 3 {
			cancel() // the watch did not stop by itself: end the test
		}
	})
	if ee := wantExit(t, err, exitcode.Blocked); !strings.Contains(ee.Hint, "upgrade pcb") || len(passes) != 1 || len(e.clk.slept) != slept {
		t.Errorf("newer cache: %v (hint %q) after %d passes, %d sleeps; want it to stop after 1 pass, no sleep",
			ee, ee.Hint, len(passes), len(e.clk.slept)-slept)
	}
	if len(passes) > 0 && passes[0].Next != 0 {
		t.Errorf("the pass the watch stops on: next %v, want 0", passes[0].Next)
	}

	// Back to this pcb's schema, then damage the tables.
	db, err = sql.Open("sqlite", cache.Path(e.dirs.State))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	e.sync(t, false)
	damageTables(t, cache.Path(e.dirs.State), "calendars", "events")
	_, err = e.svc.Sync(t.Context(), service.SyncRequest{})
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "damaged") || !strings.Contains(ee.Hint, "pcb sync --full") {
		t.Errorf("error %q, hint %q; want the damaged cache and its way out", ee.Error(), ee.Hint)
	}
	passes = nil
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	err = e.svc.WatchSync(ctx, service.SyncRequest{}, 0, func(p service.WatchPass) {
		if passes = append(passes, p); len(passes) == 3 {
			cancel() // the watch did not stop by itself: end the test
		}
	})
	_ = wantExit(t, err, exitcode.MustAct)
	if len(passes) != 1 {
		t.Errorf("the watch ran %d passes on a damaged cache, want it to stop after 1", len(passes))
	}
}
