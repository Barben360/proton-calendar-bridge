package syncer

import (
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// Progress is where a running pass is, sent to Options.Progress: at the
// pass's start (Calendar 0), when each calendar starts, after each page of
// a listing, and when each calendar ends.
type Progress struct {
	// Calendar is the calendar being synced, 1-based (0: none started
	// yet); Calendars is how many the pass syncs.
	Calendar, Calendars int
	// Name and Mode are the current calendar's (Mode: ModeInitial,
	// ModeIncremental or ModeFull; "" before the first). PrevFullAt is its
	// last full listing before this pass (zero: never listed).
	Name       string
	Mode       string
	PrevFullAt time.Time
	// Fetched is how many rows the pass's listings read so far; Expected
	// how many they should read in all, or -1 while a calendar still to
	// list has no estimate (never synced, and its listing has not
	// announced a Total yet). An incremental calendar counts for neither.
	Fetched, Expected int
}

// tracker keeps a pass's progress and sends it.
type tracker struct {
	send func(Progress)
	cals []calProgress
	cur  int // index in cals of the current calendar, -1: none yet
	mode string
	prev time.Time
}

// calProgress is one calendar of the pass.
type calProgress struct {
	name     string
	lists    bool // listed in full, or expected to be
	expected int  // rows its listing should read; -1: unknown
	fetched  int  // rows its listing read so far
	counted  bool // a listing announced a Total: the cached estimate is replaced
}

// newTracker plans the pass over cals: which calendars will be listed (the
// same rules as syncCalendar's, a mid-pass fallback is added when it
// starts) and their expected rows (the cached count; none for a calendar
// never synced). send nil: a tracker that sends nothing.
func newTracker(send func(Progress), names []string, prev []cache.Calendar, o Options, now time.Time) *tracker {
	t := &tracker{send: send, cur: -1}
	if send == nil {
		return t
	}
	t.cals = make([]calProgress, len(names))
	for i, p := range prev {
		cp := calProgress{name: names[i], expected: -1}
		cp.lists = willList(p, o, now)
		if p.Cursor != "" {
			cp.expected = p.Events
		}
		t.cals[i] = cp
	}
	t.emit()
	return t
}

// willList reports whether syncCalendar lists prev's calendar in full
// without first reading its feed.
func willList(prev cache.Calendar, o Options, now time.Time) bool {
	switch {
	case prev.Cursor == "", o.Full, now.Sub(prev.FullAt) > o.FullEvery:
		return true
	case prev.Resync != "":
		return !now.Before(prev.FullAt.Add(FallbackGap))
	}
	return false
}

// start marks calendar i as the current one, in mode; lists says whether
// it is listed now. A listed calendar counts from now on, planned or not:
// the plan read the clock at the pass's start, and a calendar can come due
// while the ones before it list. One that is not (read incrementally, or
// skipped while its fallback listing waits) counts for nothing.
func (t *tracker) start(i int, mode string, prevFull time.Time, lists bool) {
	if t.send == nil {
		return
	}
	t.cur, t.mode, t.prev = i, mode, prevFull
	t.cals[i].lists = lists
	t.emit()
}

// page records a page of the current calendar's listing: read unique rows
// so far, total announced (-1: none). A listing repeated because it was
// unstable starts its count again: the calendar keeps the highest count
// and estimate, so the progress never goes back.
func (t *tracker) page(read, total int) {
	if t.send == nil || t.cur < 0 {
		return
	}
	c := &t.cals[t.cur]
	c.fetched = max(c.fetched, read)
	switch {
	case total < 0:
	case !c.counted: // Proton's count replaces the cached one, larger or not
		c.expected, c.counted = total, true
	default:
		c.expected = max(c.expected, total)
	}
	t.emit()
}

// done ends the current calendar: a listed one counts as complete with the
// rows its listing read.
func (t *tracker) done() {
	if t.send == nil || t.cur < 0 {
		return
	}
	c := &t.cals[t.cur]
	if c.lists {
		c.expected = c.fetched
	}
	t.emit()
}

func (t *tracker) emit() {
	p := Progress{Calendars: len(t.cals), Mode: t.mode, PrevFullAt: t.prev}
	if t.cur >= 0 {
		p.Calendar, p.Name = t.cur+1, t.cals[t.cur].name
	}
	for _, c := range t.cals {
		if !c.lists {
			continue
		}
		p.Fetched += c.fetched
		switch {
		case p.Expected < 0:
		case c.expected < 0:
			p.Expected = -1
		default:
			p.Expected += max(c.expected, c.fetched)
		}
	}
	t.send(p)
}
