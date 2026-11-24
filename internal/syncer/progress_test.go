package syncer_test

import (
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
