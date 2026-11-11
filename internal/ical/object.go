package ical

import (
	"cmp"
	"slices"
	"strings"
)

const prodID = "-//pcb//proton-calendar-bridge//EN"

// Exception is one recurrence exception with its sort key: the row's
// plaintext RecurrenceID (Unix seconds).
type Exception struct {
	Comp         Component
	RecurrenceID int64
}

// Object renders the CalDAV object of one UID: a VCALENDAR with the master
// first (nil for a series whose master is gone), then the exceptions sorted by
// RecurrenceID (ties keep the caller's order, which is row ID order). An
// exception never carries RRULE or EXDATE. The inputs are not modified.
func Object(master *Component, exceptions []Exception) []byte {
	cal := Component{Name: "VCALENDAR"}
	cal.add(Property{Name: "VERSION", Value: "2.0"})
	cal.add(Property{Name: "PRODID", Value: prodID})
	cal.add(Property{Name: "CALSCALE", Value: "GREGORIAN"})
	if master != nil {
		cal.Items = append(cal.Items, Item{Comp: new(master.Clone())})
	}
	sorted := make([]Exception, len(exceptions))
	for i, e := range exceptions {
		sorted[i] = Exception{Comp: e.Comp.Clone(), RecurrenceID: e.RecurrenceID}
		sorted[i].Comp.removeProps(func(p Property) bool {
			return strings.EqualFold(p.Name, "RRULE") || strings.EqualFold(p.Name, "EXDATE")
		})
	}
	slices.SortStableFunc(sorted, func(a, b Exception) int { return cmp.Compare(a.RecurrenceID, b.RecurrenceID) })
	for i := range sorted {
		cal.Items = append(cal.Items, Item{Comp: &sorted[i].Comp})
	}
	return cal.Bytes()
}
