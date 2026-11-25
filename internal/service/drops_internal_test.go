package service

import (
	"strings"
	"testing"
	"time"
)

// A series that cannot be expanded, before or after the change, is taken
// as dropping occurrences (fail closed); a valid change of it is not.
func TestDroppedOccurrencesFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	object := func(rule string) []byte {
		lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN", "BEGIN:VEVENT", "UID:s@x", "DTSTAMP:20261101T080000Z",
			"DTSTART:20261201T080000Z", "DTEND:20261201T090000Z", "SUMMARY:Series"}
		if rule != "" {
			lines = append(lines, "RRULE:"+rule)
		}
		return []byte(strings.Join(append(lines, "END:VEVENT", "END:VCALENDAR", ""), "\r\n"))
	}
	weekly := object("FREQ=WEEKLY")
	for _, tc := range []struct {
		name          string
		before, after []byte
		want          int
	}{
		{"valid", weekly, object("FREQ=DAILY"), 0},
		{"rule outside the set after", weekly, object("FREQ=HOURLY"), DroppedUnknown},
		{"rule outside the set before", object("FREQ=HOURLY"), weekly, DroppedUnknown},
		{"after does not parse", weekly, []byte("BEGIN:VCALENDAR\r\nnot an object"), DroppedUnknown},
		{"before does not parse", []byte("BEGIN:VCALENDAR\r\nnot an object"), weekly, DroppedUnknown},
	} {
		if got := droppedOccurrences(tc.before, tc.after, ScopeSeries, now); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

// An expansion the series cap cuts cannot be counted: more occurrences
// than MaxSeriesOccurrences in the window (overrides at times of their own,
// besides a daily rule) make even a new title DroppedUnknown.
func TestDroppedOccurrencesCutByTheCapFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	lines := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//test//EN",
		"BEGIN:VEVENT", "UID:s@x", "DTSTAMP:20261101T080000Z", "DTSTART:20261201T080000Z", "DTEND:20261201T090000Z", "SUMMARY:Series", "RRULE:FREQ=DAILY", "END:VEVENT"}
	first := time.Date(2026, 12, 1, 8, 0, 0, 0, time.UTC)
	for i := range MaxSeriesOccurrences {
		day := first.AddDate(0, 0, i%700)
		rid, at := day.Format("20060102T150405Z"), day.Add(time.Duration(4+i/700)*time.Hour).Format("20060102T150405Z")
		lines = append(lines, "BEGIN:VEVENT", "UID:s@x", "DTSTAMP:20261101T080000Z", "RECURRENCE-ID:"+rid,
			"DTSTART:"+at, "DTEND:"+at, "SUMMARY:Moved", "END:VEVENT")
	}
	obj := []byte(strings.Join(append(lines, "END:VCALENDAR", ""), "\r\n"))
	if got := droppedOccurrences(obj, obj, ScopeSeries, now); got != DroppedUnknown {
		t.Errorf("%d, want DroppedUnknown", got)
	}
}
