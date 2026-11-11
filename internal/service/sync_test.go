package service_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
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

func TestEventsWithoutCacheMustAct(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req())
	_, err := e.svc.Events(t.Context(), service.EventsRequest{Calendar: "Work"})
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "pcb sync") {
		t.Errorf("hint = %q, want pcb sync", ee.Hint)
	}
	if _, err := os.Stat(cache.Path(e.dirs.State)); !os.IsNotExist(err) {
		t.Errorf("Events created the cache file: %v", err)
	}
}

func TestEventsCalendarSelection(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	w1 := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Twin"})
	w2 := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Twin"})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Solo"})
	e.login(t, e.req())
	e.sync(t, false)

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
	_, err = e.svc.Events(t.Context(), service.EventsRequest{})
	_ = wantExit(t, err, exitcode.Usage)
	if evs := e.events(t, service.EventsRequest{Calendar: w2}); evs == nil {
		t.Error("an empty calendar returned nil, want an empty list")
	}
	e.events(t, service.EventsRequest{Calendar: "Solo"})
}

func TestEventsWindow(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	cal := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	rec := base + 2*day
	ids := map[string]string{
		"early":   e.f.AddEvent(cal, protontest.Event{UID: "early", Start: base - 5*day, End: base - 5*day + 3600}),
		"master":  e.f.AddEvent(cal, protontest.Event{UID: "master", Start: base - 10*day, End: base - 10*day + 3600, RRule: "FREQ=DAILY"}),
		"future":  e.f.AddEvent(cal, protontest.Event{UID: "future", Start: base + 10*day, End: base + 10*day + 3600, RRule: "FREQ=DAILY"}),
		"span":    e.f.AddEvent(cal, protontest.Event{UID: "span", Start: base - day, End: base + day}),
		"inside":  e.f.AddEvent(cal, protontest.Event{UID: "inside", Start: base + day, End: base + day + 3600}),
		"except":  e.f.AddEvent(cal, protontest.Event{UID: "master", Start: base + 2*day, End: base + 2*day + 3600, RecurrenceID: &rec}),
		"late":    e.f.AddEvent(cal, protontest.Event{UID: "late", Start: base + 7*day, End: base + 7*day + 3600}),
		"touches": e.f.AddEvent(cal, protontest.Event{UID: "touches", Start: base + 3*day, End: base + 3*day + 3600}),
	}
	e.login(t, e.req())
	e.sync(t, false)
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

func TestStatusSyncSection(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	if st, _ := e.svc.Status(t.Context()); st.Sync != nil {
		t.Fatalf("sync section without a cache: %+v", st.Sync)
	}
	cal := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.f.AddEvent(cal, protontest.Event{UID: "a@x", Start: base, End: base + 60})
	e.login(t, e.req())
	e.sync(t, false)
	st, err := e.svc.Status(t.Context())
	if err != nil || st.Sync == nil || len(st.Sync.Calendars) != 1 {
		t.Fatalf("status = %+v, %v", st.Sync, err)
	}
	c := st.Sync.Calendars[0]
	if c.ID != cal || c.Name != "Work" || c.Events != 1 || c.SyncedAt == nil || !c.SyncedAt.Equal(t0) || c.FullAt == nil || c.Error != "" {
		t.Errorf("calendar status = %+v", c)
	}
}

func TestWatchStopsWhenLoginNeeded(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req()) // no unattended relogin: a person is needed once the session ends
	e.f.RevokeSessions()
	passes := 0
	err := e.svc.WatchSync(t.Context(), service.SyncRequest{}, 0, func(_ service.SyncResult, err error) {
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
	err := e.svc.WatchSync(ctx, service.SyncRequest{}, 0, func(res service.SyncResult, err error) {
		if err != nil {
			t.Errorf("pass: %v", err)
		}
		modes = append(modes, calSync(t, res, cal).Mode)
		if len(modes) == 2 {
			cancel()
		}
	})
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Hint != "run the command again" {
		t.Errorf("hint = %q", ee.Hint)
	}
	if len(modes) != 2 || modes[0] != "initial" || modes[1] != "incremental" {
		t.Errorf("modes = %v", modes)
	}
	if len(e.clk.slept) == 0 || e.clk.slept[0] != 30*time.Second {
		t.Errorf("slept %v, want the 30 s default interval", e.clk.slept)
	}
}

func TestWatchKeepsGoingAfterPartial(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	bad := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Bad"})
	e.login(t, e.req())
	e.f.FailNext("GET", "/calendar/v1/"+bad+"/events", 403, 2011, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var codes []exitcode.Code
	_ = e.svc.WatchSync(ctx, service.SyncRequest{}, 0, func(_ service.SyncResult, err error) {
		codes = append(codes, exitcode.From(err))
		if len(codes) == 2 {
			cancel()
		}
	})
	if len(codes) != 2 || codes[0] != exitcode.Partial || codes[1] != exitcode.OK {
		t.Errorf("pass outcomes = %v, want partial then ok", codes)
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
