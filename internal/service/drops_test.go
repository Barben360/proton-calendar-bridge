package service_test

import (
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// A dry run counts the occurrences starting after now (the rig's clock,
// t0) that a series change removes: an earlier UNTIL, a smaller COUNT, a
// sparser rule or no rule at all remove some; a later UNTIL, a denser rule,
// a moved start, a new title, a single event or a series already over
// remove none.
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
	for _, tc := range []struct {
		name  string
		start int64
		rrule string
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
		{name: "denser rule", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{RRule: ptr("FREQ=DAILY;COUNT=10")}},
		{name: "endless start an hour later", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{Start: timed(base + 3600)}},
		{name: "counted start a day later", start: base, rrule: "FREQ=WEEKLY;COUNT=10", f: service.EventFields{Start: timed(base + day)}},
		{name: "until start a day earlier", start: base, rrule: "FREQ=WEEKLY;UNTIL=" + stamp(base+9*week), f: service.EventFields{Start: timed(base - day)}},
		{name: "new title", start: base, rrule: "FREQ=WEEKLY", f: service.EventFields{Summary: ptr("Renamed")}},
		{name: "single event moved to the past", start: base, f: service.EventFields{Start: timed(now - week)}},
		{name: "series already over", start: over, rrule: "FREQ=WEEKLY;COUNT=5", f: service.EventFields{RRule: ptr("FREQ=WEEKLY;COUNT=2")}},
		{name: "one occurrence moved to the past", start: base, rrule: "FREQ=WEEKLY", scope: service.ScopeOccurrence,
			rid: &service.EventTime{At: at(base + week)}, f: service.EventFields{Start: timed(now - week)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, protontest.Options{}, func(f *protontest.Fake, cal string) {
				var extra []string
				if tc.rrule != "" {
					extra = append(extra, "RRULE:"+tc.rrule)
				}
				f.AddEvent(cal, protontest.Event{UID: "s@x", Start: tc.start, End: tc.start + 3600, RRule: tc.rrule, Color: green,
					Card: card("s@x", "Series", tc.start, extra...)})
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
