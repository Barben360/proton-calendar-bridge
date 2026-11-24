package syncer_test

import (
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/syncer"
)

// progressOf runs a pass at now and returns every Progress it sent.
func progressOf(t *testing.T, o syncer.Options, run func(syncer.Options)) []syncer.Progress {
	t.Helper()
	var got []syncer.Progress
	o.Progress = func(p syncer.Progress) { got = append(got, p) }
	run(o)
	if len(got) == 0 {
		t.Fatal("the pass sent no progress")
	}
	return got
}

// checkMonotonic fails unless Fetched never goes down and the calendar
// index never goes back.
func checkMonotonic(t *testing.T, got []syncer.Progress) {
	t.Helper()
	for i := 1; i < len(got); i++ {
		if got[i].Fetched < got[i-1].Fetched || got[i].Calendar < got[i-1].Calendar {
			t.Fatalf("progress went back at %d: %+v then %+v", i, got[i-1], got[i])
		}
	}
}

// The daily full listing announces what it will read before it reads it,
// counts every page, and ends complete.
func TestProgressOfAFullPassCountsEveryPage(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Sport"})
	addEvents(f, a, 150)
	addEvents(f, b, 2)
	pass(t, c, db, at(t0))
	t25 := t0.Add(25 * time.Hour)
	got := progressOf(t, at(t25), func(o syncer.Options) { pass(t, c, db, o) })
	checkMonotonic(t, got)
	if first := got[0]; first != (syncer.Progress{Calendars: 2, Expected: 152}) {
		t.Errorf("first progress %+v, want 2 calendars, 152 expected, none started", first)
	}
	sawPage := false
	for _, p := range got {
		if p.Calendar == 1 && p.Fetched == 100 {
			sawPage = true
			if p.Name != "Perso" || p.Mode != syncer.ModeFull || !p.PrevFullAt.Equal(t0) || p.Expected != 152 {
				t.Errorf("after the first page: %+v, want Perso, full, last listed %v, 152 expected", p, t0)
			}
		}
	}
	if !sawPage {
		t.Errorf("no progress after the first page of 100: %+v", got)
	}
	last := got[len(got)-1]
	if last.Calendar != 2 || last.Name != "Sport" || last.Fetched != 152 || last.Expected != 152 {
		t.Errorf("last progress %+v, want calendar 2/2 Sport, 152 of 152", last)
	}
}

// A first sync has no estimate until every calendar's listing announced
// its Total; then it is exact.
func TestProgressOfAFirstSyncHasNoEstimateUntilTotalsArrive(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Sport"})
	addEvents(f, a, 150)
	addEvents(f, b, 2)
	got := progressOf(t, at(t0), func(o syncer.Options) { pass(t, c, db, o) })
	checkMonotonic(t, got)
	for _, p := range got {
		if p.Calendar < 2 && p.Expected != -1 {
			t.Errorf("before the second calendar lists: %+v, want no estimate (-1)", p)
		}
		if p.Calendar >= 1 && p.Mode != syncer.ModeInitial {
			t.Errorf("%+v, want mode initial", p)
		}
	}
	sawSport := false
	for _, p := range got {
		if p.Calendar == 2 && p.Fetched == 152 && !sawSport {
			sawSport = true // Sport's first page: every Total is in
			if p.Expected != 152 {
				t.Errorf("once every calendar announced its Total: %+v, want exactly 152 expected", p)
			}
		}
	}
	if !sawSport {
		t.Errorf("no progress after Sport's first page: %+v", got)
	}
	if last := got[len(got)-1]; last.Fetched != 152 || last.Expected != 152 {
		t.Errorf("last progress %+v, want 152 of 152", last)
	}
}

// A listing without Total keeps the cached count as its estimate.
func TestProgressWithoutTotalUsesTheCachedCount(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 150)
	pass(t, c, db, at(t0))
	f.ListWithoutTotal(true)
	got := progressOf(t, at(t0.Add(25*time.Hour)), func(o syncer.Options) { pass(t, c, db, o) })
	for _, p := range got {
		if p.Expected != 150 {
			t.Errorf("%+v, want the cached 150 as the estimate throughout", p)
		}
	}
	if last := got[len(got)-1]; last.Fetched != 150 {
		t.Errorf("last progress %+v, want 150 fetched", last)
	}
}

// An incremental pass lists nothing: it moves through the calendars with
// nothing to fetch, until a calendar falls back to a listing, which then
// counts with its cached rows.
func TestProgressOfAnIncrementalPass(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Sport"})
	addEvents(f, a, 3)
	addEvents(f, b, 120)
	pass(t, c, db, at(t0))
	got := progressOf(t, at(t0.Add(time.Minute)), func(o syncer.Options) { pass(t, c, db, o) })
	for _, p := range got {
		if p.Fetched != 0 || p.Expected != 0 {
			t.Errorf("incremental pass: %+v, want nothing to fetch", p)
		}
		if p.Calendar > 0 && p.Mode != syncer.ModeIncremental {
			t.Errorf("incremental pass: %+v, want mode incremental", p)
		}
	}
	if last := got[len(got)-1]; last.Calendar != 2 || last.Calendars != 2 {
		t.Errorf("last progress %+v, want calendar 2 of 2", last)
	}

	f.ForceRefresh(b)
	got = progressOf(t, at(t0.Add(time.Hour)), func(o syncer.Options) { pass(t, c, db, o) })
	checkMonotonic(t, got)
	last := got[len(got)-1]
	if last.Name != "Sport" || last.Mode != syncer.ModeFull || last.Fetched != 120 || last.Expected != 120 {
		t.Errorf("after Sport's fallback listing: %+v, want Sport, full, 120 of 120", last)
	}
	for _, p := range got {
		if p.Calendar == 2 && p.Mode == syncer.ModeFull && p.Fetched == 100 && p.Expected != 120 {
			t.Errorf("mid fallback listing: %+v, want 120 expected", p)
		}
	}
}

// ticking is a clock that moves on by step at each reading, as a real one
// does while a pass runs.
func ticking(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	now := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := now
		now = now.Add(step)
		return t
	}
}

// checkBounded fails unless the estimate, once there is one, never falls
// below what was fetched.
func checkBounded(t *testing.T, got []syncer.Progress) {
	t.Helper()
	for _, p := range got {
		if p.Expected >= 0 && p.Expected < p.Fetched {
			t.Errorf("%+v: fewer expected than fetched", p)
		}
	}
}

// The calendars of a daily listing were listed one after the other, so
// they come due one after the other: one that is not due yet when the pass
// starts but is by its turn is listed, and counted.
func TestProgressCountsACalendarThatComesDueDuringThePass(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Sport"})
	addEvents(f, a, 150)
	addEvents(f, b, 20)
	pass(t, c, db, syncer.Options{Now: ticking(t0, 10*time.Second)}) // Perso listed at t0+10 s, Sport at t0+20 s
	if pa, pb := cached(t, db, a).FullAt, cached(t, db, b).FullAt; !pb.After(pa) {
		t.Fatalf("listed at %v then %v: the clock did not move between the calendars", pa, pb)
	}
	// At the pass's start Sport is 5 s short of a day; at its turn, past it.
	var rep syncer.Report
	got := progressOf(t, syncer.Options{Now: ticking(t0.Add(syncer.DefaultFullEvery+15*time.Second), 10*time.Second)},
		func(o syncer.Options) { rep = pass(t, c, db, o) })
	if r := report(t, rep, b); r.Mode != syncer.ModeFull {
		t.Fatalf("Sport's mode %q, want full (the scenario needs it to come due)", r.Mode)
	}
	checkMonotonic(t, got)
	checkBounded(t, got)
	if last := got[len(got)-1]; last.Fetched != 170 || last.Expected != 170 {
		t.Errorf("last progress %+v, want 170 of 170 (both listings counted)", last)
	}
}

// A listing that shifted under a concurrent edit is read again: the
// progress stays where it was until the new listing passes it.
func TestProgressOfARepeatedListingNeverGoesBack(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 150)
	pass(t, c, db, at(t0))
	once := false
	f.OnListPage(func(_ string, page int) {
		if page == 0 && !once {
			once = true
			f.AddEventAt(a, 0, protontest.Event{UID: "front", Start: 1, End: 2})
		}
	})
	list0 := listCalls(f, a)
	got := progressOf(t, at(t0.Add(25*time.Hour)), func(o syncer.Options) { pass(t, c, db, o) })
	f.OnListPage(nil)
	if n := listCalls(f, a) - list0; n < 4 {
		t.Fatalf("%d listing requests, want a repeated listing (the scenario needs it)", n)
	}
	checkMonotonic(t, got)
	checkBounded(t, got)
	if last := got[len(got)-1]; last.Fetched != 151 || last.Expected != 151 {
		t.Errorf("last progress %+v, want 151 of 151", last)
	}
}

// A calendar that grew past its cached count, listed without a Total, never
// shows more fetched than expected.
func TestProgressOfACalendarGrownPastItsEstimate(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 150)
	pass(t, c, db, at(t0))
	for i := range 30 {
		f.AddEvent(a, protontest.Event{UID: "more" + string(rune('a'+i)), Start: 1, End: 2})
	}
	f.ListWithoutTotal(true)
	got := progressOf(t, at(t0.Add(25*time.Hour)), func(o syncer.Options) { pass(t, c, db, o) })
	checkBounded(t, got)
	if last := got[len(got)-1]; last.Fetched != 180 || last.Expected != 180 {
		t.Errorf("last progress %+v, want 180 of 180", last)
	}
}

// A calendar whose listing fails halfway counts as done with what it read:
// the pass's progress still ends complete, and the next calendar counts.
func TestProgressCompletesAfterACalendarFailsMidListing(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	b := f.AddCalendar(f.Username, protontest.Calendar{Name: "Sport"})
	addEvents(f, a, 150)
	addEvents(f, b, 20)
	pass(t, c, db, at(t0))
	once := false
	f.OnListPage(func(calID string, page int) {
		if calID == a && page == 0 && !once {
			once = true
			f.FailNext("GET", "/calendar/v1/"+a+"/events", 400, 2000, 0) // page 1
		}
	})
	var rep syncer.Report
	got := progressOf(t, at(t0.Add(25*time.Hour)), func(o syncer.Options) { rep, _ = syncer.Pass(t.Context(), c, db, o) })
	f.OnListPage(nil)
	if r := report(t, rep, a); r.Err == nil {
		t.Fatalf("Perso's listing did not fail (the scenario needs it): %+v", r)
	}
	checkMonotonic(t, got)
	checkBounded(t, got)
	if last := got[len(got)-1]; last.Fetched != 120 || last.Expected != 120 {
		t.Errorf("last progress %+v, want 120 of 120 (Perso's 100 read, Sport's 20)", last)
	}
}

// A calendar whose fallback listing waits out its gap lists nothing: it is
// left out of the estimate.
func TestProgressLeavesOutADeferredCalendar(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	addEvents(f, a, 150)
	pass(t, c, db, at(t0))
	f.ForceRefresh(a)
	if r := report(t, pass(t, c, db, at(t0.Add(time.Minute))), a); r.Err == nil {
		t.Fatalf("the refresh inside the gap was not deferred (the scenario needs it): %+v", r)
	}
	got := progressOf(t, at(t0.Add(2*time.Minute)), func(o syncer.Options) { pass(t, c, db, o) })
	for _, p := range got {
		if p.Fetched != 0 || p.Expected != 0 {
			t.Errorf("deferred calendar: %+v, want nothing fetched or expected", p)
		}
	}
	// Once the gap is over the listing is planned from the start.
	got = progressOf(t, at(t0.Add(syncer.FallbackGap+time.Minute)), func(o syncer.Options) { pass(t, c, db, o) })
	if got[0].Expected != 150 {
		t.Errorf("after the gap, at the pass's start: %+v, want the cached 150 expected", got[0])
	}
	if last := got[len(got)-1]; last.Mode != syncer.ModeFull || last.Fetched != 150 || last.Expected != 150 {
		t.Errorf("after the gap: %+v, want full, 150 of 150", last)
	}
}

// Proton's Total replaces the cached count as soon as the listing announces
// it, smaller too: rows deleted since the last pass.
func TestProgressTakesTheAnnouncedTotalOverTheCachedCount(t *testing.T) {
	t.Parallel()
	f, c, db := setup(t)
	a := f.AddCalendar(f.Username, protontest.Calendar{Name: "Perso"})
	ids := addEvents(f, a, 150)
	pass(t, c, db, at(t0))
	for _, id := range ids[:40] {
		f.DeleteEvent(a, id)
	}
	got := progressOf(t, at(t0.Add(25*time.Hour)), func(o syncer.Options) { pass(t, c, db, o) })
	if got[0].Expected != 150 {
		t.Errorf("before the listing: %+v, want the cached 150", got[0])
	}
	for _, p := range got {
		if p.Fetched == 100 && p.Expected != 110 {
			t.Errorf("after the first page: %+v, want Proton's 110 expected", p)
		}
	}
}
