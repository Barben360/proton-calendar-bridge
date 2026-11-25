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
//   - the original runs past the horizon and the edited series ends sooner
//     there (endsNoSooner: an endless rule given an end or removed, a
//     far-reaching one whose COUNT or UNTIL changes, or a nearer end past
//     the horizon): DroppedUnknown, as what goes cannot be counted.
//
// A moved start is allowed for: the window's end, and the end the edited
// series must reach, follow it by at most one period of the original (the
// gap between its last two occurrences there, none for a single one),
// later or earlier, so a small move is not a drop but a postponement past
// an occurrence is, and moving the start back gives no room to end sooner.
// Each bound has a slack of zoneSlackDrop, for a change of zone whose
// offset moves with daylight saving dates of its own.
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
	late := min(max(shift, 0)+zoneSlackDrop, period)        // how much later the edited series may end
	early := max(min(shift, late), -period) - zoneSlackDrop // how much earlier it may end
	// Past the horizon nothing is counted: a loss there makes the change
	// drop, as DroppedUnknown when the window shows no loss of its own.
	far := beyond != nil && !endsNoSooner(bm, bo, am, ao, *beyond, early, late)
	if len(old) == 0 {
		if far {
			return DroppedUnknown
		}
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
	if far && dropped == 0 {
		return DroppedUnknown
	}
	return dropped
}

// endsNoSooner: the original series (bm, bo), whose occurrences go on
// past the horizon from beyond, is matched there by the edited one (am,
// ao), at most early sooner (early ≤ 0 is how much sooner). The rules'
// own COUNT and UNTIL decide what an expansion cannot see:
//
//   - an endless original (neither COUNT nor UNTIL) needs an endless
//     edited rule: one given an end, or removed, ends sooner however dense;
//     and over the original's capped expansion from beyond (moved by early,
//     and by late at its end), at least as many edited occurrences: a
//     sparser rule there (every ten years instead of two) loses some;
//   - a bounded original with more than the cap from beyond to
//     endlessProbe (a huge stored COUNT, an UNTIL far ahead) needs the
//     edited rule's COUNT and UNTIL unchanged, so a new title keeps it,
//     and for a COUNT no start moved earlier, which ends it sooner;
//   - a shorter one needs an edited occurrence from its last one (moved
//     by early) on.
//
// At most one capped expansion of each decides: the cost of a page of
// events_list, whatever a stored COUNT says.
func endsNoSooner(bm ical.Component, bo []ical.Component, am ical.Component, ao []ical.Component, beyond time.Time, early, late time.Duration) bool {
	be, _, ok := ruleEnd(bm)
	if !ok {
		return false
	}
	ae, recurring, ok := ruleEnd(am)
	if !ok {
		return false
	}
	orig, cut, ok := startsCapped(bm, bo, beyond, endlessProbe)
	if !ok || len(orig) == 0 {
		return false
	}
	if be.Endless() {
		if !recurring || !ae.Endless() {
			return false
		}
		edited, editedCut, ok := startsCapped(am, ao, beyond.Add(early), orig[len(orig)-1].Add(late).Add(time.Second))
		return ok && (editedCut || len(edited) >= len(orig))
	}
	if cut {
		return recurring && ae == be && (be.Count == "" || early >= -zoneSlackDrop)
	}
	edited, editedCut, ok := startsCapped(am, ao, orig[len(orig)-1].Add(early), endlessProbe)
	return ok && (len(edited) > 0 || editedCut)
}

// ruleEnd is the end of master's RRULE; recurring is false when it has
// none, ok false when the rule does not parse.
func ruleEnd(master ical.Component) (end ical.RuleEnd, recurring, ok bool) {
	p, has := master.Get("RRULE")
	if !has || p.Value == "" {
		return ical.RuleEnd{}, false, true
	}
	end, err := ical.EndOfRule(p.Value)
	return end, true, err == nil
}

// endlessProbe ends the expansions after the horizon: no rule pcb writes
// ends that late (UNTIL is at most 2037).
var endlessProbe = time.Date(8000, 1, 1, 0, 0, 0, 0, time.UTC)

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
	starts, cut, ok := startsCapped(master, overrides, from, to)
	return starts, ok && !cut
}

// startsCapped is startsFrom that reports a cut (more than
// MaxSeriesOccurrences in the range) instead of failing; ok is false when
// they cannot be expanded.
func startsCapped(master ical.Component, overrides []ical.Component, from, to time.Time) (starts []time.Time, cut, ok bool) {
	occ, truncated, err := ical.Expand(master, overrides, from, to, time.UTC, MaxSeriesOccurrences)
	if err != nil {
		return nil, false, false
	}
	for _, o := range occ {
		if !o.Start.Before(from) && o.Start.Before(to) {
			starts = append(starts, o.Start)
		}
	}
	return starts, truncated, true
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
