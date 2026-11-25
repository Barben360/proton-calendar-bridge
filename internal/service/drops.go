package service

import (
	"slices"
	"sort"
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

// droppedOccurrences is how many occurrences starting after now the change
// removes from the series, or DroppedUnknown. A change drops occurrences
// when, compared with the original series:
//
//   - fewer occurrences start in the window, from now to the original's last
//     occurrence within DropHorizon: Dropped is how many fewer; or
//   - the edited series ends earlier: it has no occurrence from the
//     original's last one on. Dropped is then the original occurrences
//     after the edited series' last one (the larger of the two counts
//     wins: both are occurrences that go); or
//   - the original has an occurrence after the horizon and the edited
//     series none: DroppedUnknown, as what goes cannot be counted.
//
// A start moved later is allowed for: the window's end, and the end the
// edited series must reach, follow it by at most one period of the
// original (the gap between its last two occurrences there, none for a
// single one), so a small move is not a drop but a postponement past an
// occurrence is. A start moved earlier needs no allowance. Each bound has a
// slack of zoneSlackDrop, for a change of zone whose offset moves with
// daylight saving dates of its own.
//
// Only a series change of a recurring event can drop occurrences: an
// occurrence change moves or alters one, as an edit of a single event does.
// Floating and all-day times are read in UTC, as Event's check does. Any
// error, and an expansion its cap cuts, is DroppedUnknown (fail closed);
// nothing of the event goes into it.
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
	horizon := now.Add(DropHorizon)
	old, ok := startsFrom(bm, bo, now, horizon)
	if !ok {
		return DroppedUnknown
	}
	beyond, ok := firstStartFrom(bm, bo, horizon)
	if !ok {
		return DroppedUnknown
	}
	shift, ok := startShift(before, after)
	if !ok {
		return DroppedUnknown
	}
	// The original's period at its end: between its last two occurrences
	// from now on, the first one after the horizon included.
	ends := old
	if beyond != nil {
		ends = append(slices.Clone(old), *beyond)
	}
	var period time.Duration
	if n := len(ends); n >= 2 {
		period = ends[n-1].Sub(ends[n-2])
	}
	late := min(max(shift, 0)+zoneSlackDrop, period) // how much later the edited series may end
	early := min(shift, late) - zoneSlackDrop        // how much earlier it may end
	if beyond != nil {
		reach, ok := firstStartFrom(am, ao, horizon.Add(early))
		if !ok || reach == nil {
			return DroppedUnknown
		}
	}
	if len(old) == 0 {
		return 0
	}
	last := old[len(old)-1]
	kept, ok := startsFrom(am, ao, now, last.Add(late).Add(time.Second))
	if !ok {
		return DroppedUnknown
	}
	dropped := max(len(old)-len(kept), 0)
	if beyond == nil {
		reach, ok := firstStartFrom(am, ao, last.Add(early))
		if !ok {
			return DroppedUnknown
		}
		if reach == nil {
			// The edited series ends before the original: the original's
			// occurrences after its last one (or all, when none is left
			// after now) go.
			tail := len(old)
			if len(kept) > 0 {
				end := kept[len(kept)-1].Add(-early)
				tail = len(old) - sort.Search(len(old), func(i int) bool { return old[i].After(end) })
			}
			dropped = max(dropped, tail)
		}
	}
	return dropped
}

// zoneSlackDrop is the slack of droppedOccurrences' bounds: two hours, the
// largest daylight saving change.
const zoneSlackDrop = 2 * time.Hour

// farFuture bounds the expansions after the horizon (Expand stops at year
// 9999 anyway).
var farFuture = time.Date(9000, 1, 1, 0, 0, 0, 0, time.UTC)

// firstStartFrom is the first start at or after from of the occurrences of
// master and overrides, nil when there is none; ok is false when they
// cannot be expanded, or the series' cap cut them before one was found.
func firstStartFrom(master ical.Component, overrides []ical.Component, from time.Time) (first *time.Time, ok bool) {
	occ, truncated, err := ical.Expand(master, overrides, from, farFuture, time.UTC, MaxSeriesOccurrences)
	if err != nil {
		return nil, false
	}
	for _, o := range occ {
		if !o.Start.Before(from) {
			return &o.Start, true
		}
	}
	return nil, !truncated
}

// startsFrom is the starts of the occurrences of master and overrides that
// start in [from, to), in order; ok is false when they cannot be expanded
// or the series' cap cut them.
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
