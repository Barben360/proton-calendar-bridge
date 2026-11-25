package service

import (
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
)

// A series change can end the series early (an earlier UNTIL, a smaller
// COUNT, a sparser rule, the rule removed), which deletes occurrences as
// surely as a delete. The MCP adapter treats such a change as a delete; the
// agenda counts what it removes, with the expansion events_list uses.

// DropHorizon is how far after now an update's removed occurrences are
// counted: two years.
const DropHorizon = 2 * 366 * 24 * time.Hour

// DroppedUnknown is UpdateResult.Dropped for a change whose occurrences
// cannot be counted (a series that cannot be expanded): it is taken as
// removing some.
const DroppedUnknown = -1

// droppedOccurrences is how many fewer occurrences start after now in the
// series after the change than before, counted from now to the original
// series' last occurrence within DropHorizon. The window's end follows a
// start moved later (an occurrence moved, not removed); occurrences before
// now never count. Only a series change of a recurring event can drop
// occurrences: an occurrence change moves or alters one, as an edit of a
// single event does. Floating and all-day times are read in UTC, as
// Event's check does. Any error, and an expansion its cap cuts, is
// DroppedUnknown (fail closed); nothing of the event goes into it.
func droppedOccurrences(before, after []byte, scope EditScope, now time.Time) int {
	if scope != ScopeSeries {
		return 0
	}
	bm, bo, err := splitObject(before)
	if err != nil {
		return DroppedUnknown
	}
	if _, recurring := bm.Get("RRULE"); bm.Name == "" || !recurring {
		return 0
	}
	am, ao, err := splitObject(after)
	if err != nil {
		return DroppedUnknown
	}
	old, ok := startsFrom(bm, bo, now, now.Add(DropHorizon))
	if !ok {
		return DroppedUnknown
	}
	if len(old) == 0 {
		return 0
	}
	shift, ok := startShift(before, after)
	if !ok {
		return DroppedUnknown
	}
	end := old[len(old)-1].Add(max(shift, 0))
	kept, ok := startsFrom(am, ao, now, end.Add(time.Second))
	if !ok {
		return DroppedUnknown
	}
	return max(len(old)-len(kept), 0)
}

// startsFrom is the starts of the occurrences of master and overrides that
// start in [from, to); ok is false when they cannot be expanded or the
// series' cap cut them.
func startsFrom(master ical.Component, overrides []ical.Component, from, to time.Time) (starts []time.Time, ok bool) {
	occ, truncated, err := ical.Expand(master, overrides, from, to, time.UTC, MaxSeriesOccurrences)
	if err != nil || truncated {
		return nil, false
	}
	for _, o := range occ {
		if !o.Start.Before(from) && o.Start.Before(to) {
			starts = append(starts, o.Start)
		}
	}
	return starts, true
}

// startShift is how far the change moves the series' start.
func startShift(before, after []byte) (time.Duration, bool) {
	b, err := ical.DecodeEvent(before)
	if err != nil {
		return 0, false
	}
	a, err := ical.DecodeEvent(after)
	if err != nil {
		return 0, false
	}
	bs, ok1 := startInstant(b.Start)
	as, ok2 := startInstant(a.Start)
	return as.Sub(bs), ok1 && ok2
}

// startInstant is a start as an instant: a date's midnight in UTC, a
// floating wall clock as UTC (as Expand reads both in UTC).
func startInstant(t EventTime) (time.Time, bool) {
	if t.Date == "" {
		return t.At, !t.At.IsZero()
	}
	d, err := time.Parse(time.DateOnly, t.Date)
	return d, err == nil
}
