package ical

import (
	"testing"
	"time"
)

// Every reader of a TZID wall clock (the view, the edits, the CalDAV write
// path) resolves a DST overlap or gap by RFC 5545 §3.3.5, as Expand does:
// an ambiguous time is the first of the two, a time in a gap takes the
// offset before it.
func TestWallClocksResolveAsExpandDoes(t *testing.T) {
	// Spring forward, New York 2026-03-08 02:00 → 03:00: 02:30 is 07:30Z.
	gap := calendar("BEGIN:VEVENT", "UID:g@x", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=America/New_York:20260308T023000", "DTEND;TZID=America/New_York:20260308T040000", "END:VEVENT")
	d, err := DecodeEvent(gap)
	if err != nil {
		t.Fatal(err)
	}
	master, _ := xSeries(t, xEvent("DTSTART;TZID=America/New_York:20260308T023000", "DTEND;TZID=America/New_York:20260308T040000"))
	occ, _ := xExpand(t, master, nil, "2026-03-07T00:00:00Z", "2026-03-09T00:00:00Z", time.UTC, 10)
	if len(occ) != 1 {
		t.Fatalf("%d occurrences", len(occ))
	}
	if want := utc("2026-03-08T07:30:00Z"); !occ[0].Start.Equal(want) || !d.Start.At.Equal(want) {
		t.Errorf("gap 02:30: Expand %s, DecodeEvent %s, want both %s", occ[0].Start.UTC(), d.Start.At.UTC(), want)
	}

	// Fall back, Paris 2026-10-25 03:00 → 02:00: 02:30 is first 00:30Z.
	overlap := calendar("BEGIN:VEVENT", "UID:o@x", "DTSTAMP:20260101T000000Z",
		"DTSTART;TZID=Europe/Paris:20261025T023000", "DTEND;TZID=Europe/Paris:20261025T040000", "END:VEVENT")
	d, err = DecodeEvent(overlap)
	if err != nil {
		t.Fatal(err)
	}
	if want := utc("2026-10-25T00:30:00Z"); !d.Start.At.Equal(want) {
		t.Errorf("overlap 02:30: DecodeEvent %s, want %s", d.Start.At.UTC(), want)
	}

	// The write path keys an exception by the same instant.
	in, err := ParseWrite([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:w@x\r\nDTSTAMP:20260101T000000Z\r\n" +
		"RECURRENCE-ID;TZID=Europe/Paris:20261025T023000\r\nDTSTART;TZID=Europe/Paris:20261025T100000\r\n" +
		"DTEND;TZID=Europe/Paris:20261025T110000\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:w@x\r\nDTSTAMP:20260101T000000Z\r\n" +
		"RECURRENCE-ID;TZID=America/New_York:20260308T023000\r\nDTSTART;TZID=America/New_York:20260308T100000\r\n" +
		"DTEND;TZID=America/New_York:20260308T110000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2026-10-25T00:30:00Z", "2026-03-08T07:30:00Z"} {
		if _, ok := in.Exceptions[utc(want).Unix()]; !ok {
			keys := []time.Time{}
			for k := range in.Exceptions {
				keys = append(keys, time.Unix(k, 0).UTC())
			}
			t.Errorf("ParseWrite keys the exceptions at %v, want one at %s", keys, want)
		}
	}

	// A series moved by an hour moves its exception at 01:30 into the
	// overlap: the first 02:30.
	paris := xLoc(t, "Europe/Paris")
	ex := Time{At: time.Date(2026, 10, 25, 1, 30, 0, 0, paris), TZID: "Europe/Paris"}
	moved, ok := shiftException(ex, Time{TZID: "Europe/Paris"}, time.Hour)
	if want := utc("2026-10-25T00:30:00Z"); !ok || !moved.At.Equal(want) {
		t.Errorf("shifted exception %s (%t), want %s", moved.At.UTC(), ok, want)
	}
}
