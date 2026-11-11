package ical

import (
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // TZID resolution must not depend on the host's zoneinfo
)

const prodID = "-//pcb//proton-calendar-bridge//EN"

// recurrenceTime resolves a RECURRENCE-ID to an instant: UTC "Z", TZID, floating
// (read as UTC) and date forms.
func recurrenceTime(p Property) (time.Time, bool) {
	loc := time.UTC
	for _, prm := range splitParams(p.Params) {
		if k, v, ok := strings.Cut(prm, "="); ok && strings.EqualFold(k, "TZID") {
			l, err := time.LoadLocation(strings.Trim(v, `"`))
			if err != nil {
				return time.Time{}, false
			}
			loc = l
		}
	}
	for _, layout := range []string{"20060102T150405Z", "20060102T150405", "20060102"} {
		layoutLoc := loc
		if strings.HasSuffix(layout, "Z") {
			layoutLoc = time.UTC
		}
		if t, err := time.ParseInLocation(layout, strings.TrimSpace(p.Value), layoutLoc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func compareRecurrence(a, b Component) int {
	pa, _ := a.Get("RECURRENCE-ID")
	pb, _ := b.Get("RECURRENCE-ID")
	ta, oka := recurrenceTime(pa)
	tb, okb := recurrenceTime(pb)
	switch {
	case oka && okb:
		return ta.Compare(tb)
	case oka:
		return -1
	case okb:
		return 1
	}
	return strings.Compare(pa.Value, pb.Value)
}

// Object renders the CalDAV object of one UID: a VCALENDAR with the master
// first (nil for a series whose master is gone), then the exceptions sorted by
// RECURRENCE-ID (ties keep the caller's order, which is row ID order). An
// exception never carries RRULE or EXDATE. The inputs are not modified.
func Object(master *Component, exceptions []Component) []byte {
	cal := Component{Name: "VCALENDAR"}
	cal.add(Property{Name: "VERSION", Value: "2.0"})
	cal.add(Property{Name: "PRODID", Value: prodID})
	cal.add(Property{Name: "CALSCALE", Value: "GREGORIAN"})
	if master != nil {
		cal.Items = append(cal.Items, Item{Comp: new(master.Clone())})
	}
	sorted := make([]Component, len(exceptions))
	for i, e := range exceptions {
		sorted[i] = e.Clone()
		sorted[i].removeProps(func(p Property) bool {
			return strings.EqualFold(p.Name, "RRULE") || strings.EqualFold(p.Name, "EXDATE")
		})
	}
	slices.SortStableFunc(sorted, compareRecurrence)
	for i := range sorted {
		cal.Items = append(cal.Items, Item{Comp: &sorted[i]})
	}
	return cal.Bytes()
}
