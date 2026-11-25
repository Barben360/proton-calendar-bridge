package service_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// A dry run counts the occurrences starting after now (the rig's clock,
// t0) that a series change removes: an earlier UNTIL, a smaller COUNT, a
// sparser rule, no rule, a series that ends earlier however dense, or a
// start postponed past an occurrence remove some; a later UNTIL, a denser
// rule ending as late, a start moved within one period (to another zone
// too), another weekday with the same count and end, a new title, a single
// event or a series already over remove none.
func TestUpdateDryRunCountsDroppedOccurrences(t *testing.T) {
	t.Parallel()
	const week = 7 * day
	now := t0.Unix()
	straddling := now - 3*week + 3600 // three occurrences before now, the fourth an hour after it
	over := now - 20*week             // a series whose occurrences are all in the past
	// An endless daily series from base, cut at the horizon, against a
	// weekly one in the same window: every day but one a week goes.
	daily := (now + int64(service.DropHorizon/time.Second) - base + day - 1) / day
	weekly := (daily-1)/7 + 1
	timed := func(unix int64) *service.EventTime { return &service.EventTime{At: at(unix)} }
	until := func(y int, m time.Month, d int) string { return stamp(time.Date(y, m, d, 8, 0, 0, 0, time.UTC).Unix()) }
	// An endless weekly series from base has endless occurrences in the
	// window; the last of them starts at lastWeekly.
	horizon := now + int64(service.DropHorizon/time.Second)
	endless := (horizon - base + week - 1) / week
	lastWeekly := base + (endless-1)*week
	// A weekly series in Paris from 2027-03-15 09:00, between the US and the
	// EU daylight saving dates: New York's 09:00 is five hours later then,
	// six hours later from 2027-03-28 on.
	paris, _ := time.LoadLocation("Europe/Paris")
	newYork, _ := time.LoadLocation("America/New_York")
	zoned := time.Date(2027, 3, 15, 9, 0, 0, 0, paris).Unix()
	parisLines := func(rule string) []string {
		return []string{"DTSTART;TZID=Europe/Paris:20270315T090000", "DTEND;TZID=Europe/Paris:20270315T100000", "SUMMARY:Zoned", "RRULE:" + rule}
	}
	for _, tc := range []struct {
		name  string
		start int64
		rrule string
		lines []string // the card's lines instead of card's, when set
		scope service.EditScope
		rid   *service.EventTime
		f     service.EventFields
		want  int
	}{
		{name: "earlier until", start: base, rrule: "FREQ=WEEKLY;UNTIL=" + stamp(base+9*week), f: service.EventFields{RRule: ptr("FREQ=WEEKLY;UNTIL=" + stamp(base+2*week))}, want: 7},
		{name: "smaller count", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=4")}, want: 6},
		{name: "sparser rule", start: base, rrule: "FREQ=DAILY;COUNT=14", f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=2")}, want: 12},
		{name: "sparser endless rule", start: base, rrule: "FREQ=DAILY", f: service.EventFields{RRule: ptr("FREQ=WEEKLY")}, want: int(daily - weekly)},
		{name: "rule removed", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{RRule: ptr("")}, want: 9},
		{name: "only those after now", start: straddling, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=5")}, want: 5},
		{name: "later until", start: base, rrule: "FREQ=WEEKLY;UNTIL=" + stamp(base+2*week), f: service.EventFields{RRule: ptr("FREQ=WEEKLY;UNTIL=" + stamp(base+9*week))}},
		{name: "larger count", start: base, rrule: "FREQ=WEEKLY;COUNT=4", f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=10")}},
		{name: "denser rule", start: base, rrule: "FREQ=WEEKLY;UNTIL=" + stamp(base+9*week), f: service.EventFields{RRule: ptr("FREQ=DAILY;UNTIL=" + stamp(base+9*week))}},
		{name: "denser endless rule", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{RRule: ptr("FREQ=DAILY")}},
		{name: "denser with the same count, ending earlier", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{RRule: ptr("FREQ=DAILY;COUNT=10")}, want: 8},
		{name: "endless start an hour later", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{Start: timed(base + 3600)}},
		{name: "counted start a day later", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{Start: timed(base + day)}},
		{name: "until start a day earlier", start: base, rrule: "FREQ=WEEKLY;UNTIL=" + stamp(base+9*week), f: service.EventFields{Start: timed(base - day)}},
		{name: "new title", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "single event moved to the past", start: base, f: service.EventFields{Start: timed(now - week)}},
		{name: "series already over", start: over, rrule: "FREQ=WEEKLY;COUNT=5", f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=2")}},
		{name: "one occurrence moved to the past", start: base, rrule: "FREQ=WEEKLY", scope: service.ScopeOccurrence,
			rid: &service.EventTime{At: at(base + week)}, f: service.EventFields{Start: timed(now - week)}},
		// Denser but ending earlier: fewer occurrences go by count, but the
		// series ends before the original.
		{name: "denser and counted, ending earlier", start: base, rrule: "FREQ=WEEKLY;COUNT=40", f: service.EventFields{RRule: ptr("FREQ=DAILY;COUNT=49")}, want: 33},
		{name: "denser until, ending an endless series", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{RRule: ptr("FREQ=DAILY;UNTIL=" + stamp(base+100*day))}, want: service.DroppedUnknown},
		{name: "denser until, ending earlier", start: base, rrule: "FREQ=WEEKLY;UNTIL=" + stamp(base+20*week), f: service.EventFields{RRule: ptr("FREQ=DAILY;UNTIL=" + stamp(base+5*week))}, want: 15},
		// Every occurrence in the window is kept, but none after it.
		{name: "ending at the horizon", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{RRule: ptr("FREQ=WEEKLY;UNTIL=" + stamp(lastWeekly))}, want: service.DroppedUnknown},
		// A start moved later by more than one period removes what it skips.
		{name: "endless start postponed to 2036", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{Start: timed(time.Date(2036, 1, 4, 8, 0, 0, 0, time.UTC).Unix())}, want: int(endless)},
		{name: "counted start postponed past an occurrence", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{Start: timed(base + 8*day)}, want: 1},
		{name: "counted start six days later", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{Start: timed(base + 6*day)}},
		{name: "other weekday, same count and end", start: base, rrule: "FREQ=WEEKLY;BYDAY=FR,MO;UNTIL=" + stamp(base+5*week),
			f: service.EventFields{RRule: ptr("FREQ=WEEKLY;BYDAY=FR,TU;UNTIL=" + stamp(base+5*week))}},
		// A start moved earlier is allowed for by one period at most: moved
		// a year earlier, a denser series ending sooner still drops.
		{name: "start a year earlier, denser and ending sooner", start: base, rrule: "FREQ=WEEKLY;COUNT=40",
			f: service.EventFields{Start: timed(base - 52*week), RRule: ptr("FREQ=DAILY;UNTIL=" + stamp(base+40*day))}, want: 33},
		{name: "start three years earlier, ending an endless series", start: base, rrule: "FREQ=WEEKLY",
			f: service.EventFields{Start: timed(base - 157*week), RRule: ptr("FREQ=DAILY;UNTIL=" + stamp(now+120*day))}, want: service.DroppedUnknown},
		// Past the horizon, an end that comes sooner cannot be counted.
		{name: "endless ending a week after the horizon", start: base, rrule: "FREQ=WEEKLY",
			f: service.EventFields{RRule: ptr("FREQ=WEEKLY;UNTIL=" + stamp(horizon+week))}, want: service.DroppedUnknown},
		{name: "start three years earlier, ending a day before the horizon", start: base, rrule: "FREQ=WEEKLY",
			f: service.EventFields{Start: timed(base - 157*week), RRule: ptr("FREQ=WEEKLY;UNTIL=" + stamp(horizon-day))}, want: service.DroppedUnknown},
		{name: "series after the horizon, rule removed", start: horizon + 10*day, rrule: "FREQ=WEEKLY", f: service.EventFields{RRule: ptr("")}, want: service.DroppedUnknown},
		{name: "counted series after the horizon, shortened", start: horizon + 10*day, rrule: "FREQ=WEEKLY;COUNT=10",
			f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=4")}, want: service.DroppedUnknown},
		{name: "until series past the horizon, extended", start: horizon - 2*week, rrule: "FREQ=WEEKLY;UNTIL=" + stamp(horizon+5*week),
			f: service.EventFields{RRule: ptr("FREQ=WEEKLY;UNTIL=" + stamp(horizon+9*week))}},
		{name: "endless series moved a week earlier", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{Start: timed(base - week)}},
		// Past the horizon, an endless rule given an end, or a far-reaching
		// one whose COUNT or UNTIL changes, ends sooner however dense: what
		// goes cannot be counted. A new title of such a series drops none.
		{name: "endless daily ending in 2037", start: base, rrule: "FREQ=DAILY", f: service.EventFields{RRule: ptr("FREQ=DAILY;UNTIL=" + until(2037, 12, 30))}, want: service.DroppedUnknown},
		{name: "endless daily ending in 2033", start: base, rrule: "FREQ=DAILY", f: service.EventFields{RRule: ptr("FREQ=DAILY;UNTIL=" + until(2033, 11, 11))}, want: service.DroppedUnknown},
		{name: "endless daily ending in 2031", start: base, rrule: "FREQ=DAILY", f: service.EventFields{RRule: ptr("FREQ=DAILY;UNTIL=" + until(2031, 12, 1))}, want: service.DroppedUnknown},
		{name: "endless weekdays ending in 2037", start: base, rrule: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR",
			f: service.EventFields{RRule: ptr("FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;UNTIL=" + until(2037, 12, 30))}, want: service.DroppedUnknown},
		{name: "endless three days a week ending in 2037", start: base, rrule: "FREQ=WEEKLY;BYDAY=MO,WE,FR",
			f: service.EventFields{RRule: ptr("FREQ=WEEKLY;BYDAY=MO,WE,FR;UNTIL=" + until(2037, 12, 30))}, want: service.DroppedUnknown},
		{name: "daily until 2100 ending in 2037", start: base, rrule: "FREQ=DAILY;UNTIL=" + until(2100, 1, 1),
			f: service.EventFields{RRule: ptr("FREQ=DAILY;UNTIL=" + until(2037, 12, 30))}, want: service.DroppedUnknown},
		{name: "endless daily renamed", start: base, rrule: "FREQ=DAILY", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "endless yearly renamed", start: base, rrule: "FREQ=YEARLY", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "endless every ten years renamed", start: base, rrule: "FREQ=YEARLY;INTERVAL=10", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "daily until 2100 renamed", start: base, rrule: "FREQ=DAILY;UNTIL=" + until(2100, 1, 1), f: service.EventFields{Summary: ptr("Renamed")}},
		// An endless rule made sparser past the horizon loses occurrences
		// there, though the window shows none lost: a series with a single
		// occurrence in the window, or one wholly after it. A start
		// postponed there loses some too. A series stored with a COUNT past
		// the cap ends sooner when its start moves earlier.
		{name: "every two years to every ten", start: now + 30*day, rrule: "FREQ=YEARLY;INTERVAL=2", f: service.EventFields{RRule: ptr("FREQ=YEARLY;INTERVAL=10")}, want: service.DroppedUnknown},
		{name: "every two years to every ninety-nine", start: now + 30*day, rrule: "FREQ=YEARLY;INTERVAL=2", f: service.EventFields{RRule: ptr("FREQ=YEARLY;INTERVAL=99")}, want: service.DroppedUnknown},
		{name: "every 25 months to every 1000", start: now + 30*day, rrule: "FREQ=MONTHLY;INTERVAL=25", f: service.EventFields{RRule: ptr("FREQ=MONTHLY;INTERVAL=1000")}, want: service.DroppedUnknown},
		{name: "yearly to every two years, counted in the window", start: now + 30*day, rrule: "FREQ=YEARLY", f: service.EventFields{RRule: ptr("FREQ=YEARLY;INTERVAL=2")}, want: 1},
		{name: "daily after the horizon to weekly", start: horizon + 10*day, rrule: "FREQ=DAILY", f: service.EventFields{RRule: ptr("FREQ=WEEKLY")}, want: service.DroppedUnknown},
		{name: "daily after the horizon to every 99 years", start: horizon + 10*day, rrule: "FREQ=DAILY", f: service.EventFields{RRule: ptr("FREQ=YEARLY;INTERVAL=99")}, want: service.DroppedUnknown},
		{name: "daily after the horizon postponed five years", start: horizon + 10*day, rrule: "FREQ=DAILY", f: service.EventFields{Start: timed(horizon + 10*day + 5*365*day)}, want: service.DroppedUnknown},
		{name: "daily after the horizon renamed", start: horizon + 10*day, rrule: "FREQ=DAILY", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "every two years renamed", start: now + 30*day, rrule: "FREQ=YEARLY;INTERVAL=2", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "stored count past the cap, start a year earlier", start: base, rrule: "FREQ=DAILY;COUNT=2200", f: service.EventFields{Start: timed(base - 365*day)}, want: service.DroppedUnknown},
		{name: "stored count past the cap renamed", start: base, rrule: "FREQ=DAILY;COUNT=2200", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "zoned series shortened", start: zoned, lines: parisLines("FREQ=WEEKLY;COUNT=10"), f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=4")}, want: 6},
		{name: "zoned series moved to another zone", start: zoned, lines: parisLines("FREQ=WEEKLY;COUNT=10"),
			f: service.EventFields{Start: &service.EventTime{At: time.Date(2027, 3, 15, 9, 0, 0, 0, newYork), TZID: "America/New_York"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
				var extra []string
				if tc.rrule != "" {
					extra = append(extra, "RRULE:"+tc.rrule)
				}
				c := card("s@x", "Series", tc.start, extra...)
				rule := tc.rrule
				if tc.lines != nil {
					c, rule = eventCard("s@x", tc.lines...), strings.TrimPrefix(tc.lines[len(tc.lines)-1], "RRULE:")
				}
				f.AddEvent(cal, protontest.Event{UID: "s@x", Start: tc.start, End: tc.start + 3600, RRule: rule, Color: green, Card: c})
			}).start(t)
			obj := r.object(t, "s@x")
			scope := tc.scope
			if scope == "" {
				scope = service.ScopeSeries
			}
			ref := service.EventRef{CalendarID: r.cal, UID: "s@x", RecurrenceID: tc.rid}
			res, err := r.svc.UpdateEvent(t.Context(), ref, obj.ETag, scope, tc.f, true)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if res.Dropped != tc.want {
				t.Errorf("Dropped = %d, want %d", res.Dropped, tc.want)
			}
			if len(r.f.SyncCalls()) != 0 {
				t.Error("the dry run wrote to Proton")
			}
		})
	}
}

// A stored rule may have any COUNT (an invitation's), and a COUNT rule is
// walked from its start: renaming such a series must cost about what
// listing it costs and drop nothing. The bound is 3 s: the dry run walks
// the series a few times, 3 and 36 ms here (20 and 230 ms under -race), so
// a slow or busy CI runner has a margin of ten; bisecting for the last
// occurrence took 5 and 22 s without -race, so the test still tells them
// apart.
func TestUpdateDryRunCostIsBoundedForAHugeCount(t *testing.T) {
	t.Parallel()
	from := func(year int) int64 { return time.Date(year, 1, 1, 9, 0, 0, 0, time.UTC).Unix() }
	for _, tc := range []struct {
		name  string
		start int64
		card  string
	}{
		{"daily, a million from 2020", from(2020), card("s@x", "Series", from(2020), "RRULE:FREQ=DAILY;COUNT=1000000")},
		{"daily in Paris, two million from 1900", from(1900), eventCard("s@x", "DTSTART;TZID=Europe/Paris:19000101T100000",
			"DTEND;TZID=Europe/Paris:19000101T110000", "SUMMARY:Series", "RRULE:FREQ=DAILY;COUNT=2000000")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
				f.AddEvent(cal, protontest.Event{UID: "s@x", Start: tc.start, End: tc.start + 3600, Color: green, Card: tc.card})
			}).start(t)
			obj := r.object(t, "s@x")
			began := time.Now()
			res, err := r.svc.UpdateEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "s@x"}, obj.ETag, service.ScopeSeries,
				service.EventFields{Summary: ptr("Renamed")}, true)
			took := time.Since(began)
			if err != nil {
				t.Fatalf("dry run: %v", err)
			}
			if took > 3*time.Second {
				t.Errorf("the dry run took %v, want well under 3 s", took)
			}
			if res.Dropped != 0 {
				t.Errorf("Dropped = %d for a new title, want 0", res.Dropped)
			}
		})
	}
}

// Past the horizon the series' real last occurrence counts, not its
// UNTIL: an UNTIL brought back to that occurrence drops nothing, one a
// second before it drops it (uncountable there).
func TestUpdateDryRunComparesTheLastOccurrencePastTheHorizon(t *testing.T) {
	t.Parallel()
	const week = 7 * day
	horizon := t0.Unix() + int64(service.DropHorizon/time.Second)
	last := base + ((horizon-base)/week+4)*week // an occurrence four weeks past the horizon
	for _, tc := range []struct {
		until int64
		want  int
	}{
		{last, 0},
		{last - 1, service.DroppedUnknown},
	} {
		r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
			rule := "FREQ=WEEKLY;UNTIL=" + stamp(last+3*day)
			f.AddEvent(cal, protontest.Event{UID: "s@x", Start: base, End: base + 3600, RRule: rule, Color: green, Card: card("s@x", "Series", base, "RRULE:"+rule)})
		}).start(t)
		obj := r.object(t, "s@x")
		res, err := r.svc.UpdateEvent(t.Context(), service.EventRef{CalendarID: r.cal, UID: "s@x"}, obj.ETag, service.ScopeSeries,
			service.EventFields{RRule: ptr("FREQ=WEEKLY;UNTIL=" + stamp(tc.until))}, true)
		if err != nil {
			t.Fatalf("UNTIL %s: %v", stamp(tc.until), err)
		}
		if res.Dropped != tc.want {
			t.Errorf("UNTIL %s (last occurrence %s): Dropped = %d, want %d", stamp(tc.until), stamp(last), res.Dropped, tc.want)
		}
	}
}
