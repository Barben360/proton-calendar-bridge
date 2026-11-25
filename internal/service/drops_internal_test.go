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
