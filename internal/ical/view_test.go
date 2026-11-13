package ical

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// calendar wraps VEVENT lines in the VCALENDAR head ical.Object writes.
func calendar(lines ...string) []byte {
	head := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:" + prodID, "CALSCALE:GREGORIAN"}
	return []byte(strings.Join(append(append(head, lines...), "END:VCALENDAR"), "\r\n") + "\r\n")
}

// inUTC is d with every instant moved to UTC, so that DeepEqual compares
// instants and not *time.Location pointers.
func inUTC(d EventData) EventData {
	fix := func(t Time) Time {
		if !t.At.IsZero() {
			t.At = t.At.UTC()
		}
		return t
	}
	d.Start, d.End = fix(d.Start), fix(d.End)
	if d.RecurrenceID != nil {
		d.RecurrenceID = new(fix(*d.RecurrenceID))
	}
	ex := make([]Time, len(d.ExDates))
	for i, t := range d.ExDates {
		ex[i] = fix(t)
	}
	if d.ExDates != nil {
		d.ExDates = ex
	}
	for i := range d.Overrides {
		d.Overrides[i] = inUTC(d.Overrides[i])
	}
	return d
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestDecodeEventFields(t *testing.T) {
	// The shape service rendering gives: the merged parts (shared signed,
	// shared encrypted, calendar signed, attendees), then the row's alarms.
	vcal := calendar(
		"BEGIN:VEVENT",
		"UID:abc\\,def@proton.me",
		"DTSTAMP:20261101T090000Z",
		"DTSTART;TZID=Europe/Paris:20261116T100000",
		"DTEND;TZID=Europe/Paris:20261116T113000",
		"RRULE:FREQ=WEEKLY;BYDAY=MO;COUNT=10",
		"EXDATE;TZID=Europe/Paris:20261123T100000,20261130T100000",
		"EXDATE;TZID=Europe/Paris:20261207T100000",
		"ORGANIZER;CN=Alice Martin:mailto:alice@example.org",
		"SEQUENCE:2",
		"SUMMARY:Weekly sync\\, team A",
		"DESCRIPTION:Agenda:\\n1. status\\; 2. plans \\\\ next",
		"LOCATION:Room 4\\, floor 2",
		"URL:https://example.org/meet?a=1,b=2",
		"STATUS:CONFIRMED",
		"TRANSP:OPAQUE",
		`ATTENDEE;CN="Bob, Jr.";ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;RSVP=TRUE:mailto:bob@example.org`,
		"ATTENDEE;ROLE=OPT-PARTICIPANT;PARTSTAT=TENTATIVE:MAILTO:carol@example.org",
		"BEGIN:VALARM",
		"ACTION:DISPLAY",
		"TRIGGER:-PT15M",
		"DESCRIPTION:Reminder",
		"END:VALARM",
		"BEGIN:VALARM",
		"ACTION:EMAIL",
		"TRIGGER:-P1D",
		"DESCRIPTION:Reminder",
		"SUMMARY:Reminder",
		"END:VALARM",
		"END:VEVENT",
	)
	got, err := DecodeEvent(vcal)
	if err != nil {
		t.Fatal(err)
	}
	paris := func(day, hm string) Time {
		return Time{At: utc("2026-" + day + "T" + hm + ":00+01:00"), TZID: "Europe/Paris"}
	}
	want := EventData{
		UID:         "abc,def@proton.me",
		Summary:     "Weekly sync, team A",
		Description: "Agenda:\n1. status; 2. plans \\ next",
		Location:    "Room 4, floor 2",
		URL:         "https://example.org/meet?a=1,b=2",
		Status:      "CONFIRMED",
		Transp:      "OPAQUE",
		Start:       paris("11-16", "10:00"),
		End:         paris("11-16", "11:30"),
		RRule:       "FREQ=WEEKLY;BYDAY=MO;COUNT=10",
		ExDates:     []Time{paris("11-23", "10:00"), paris("11-30", "10:00"), paris("12-07", "10:00")},
		Alarms:      []Alarm{{MinutesBefore: 15, Action: "DISPLAY"}, {MinutesBefore: 1440, Action: "EMAIL"}},
		Organizer:   &Person{Name: "Alice Martin", Email: "alice@example.org"},
		Attendees: []Person{
			{Name: "Bob, Jr.", Email: "bob@example.org", Role: "REQ-PARTICIPANT", Status: "ACCEPTED"},
			{Email: "carol@example.org", Role: "OPT-PARTICIPANT", Status: "TENTATIVE"},
		},
	}
	if g := inUTC(got); !reflect.DeepEqual(g, inUTC(want)) {
		t.Fatalf("decoded:\n%+v\nwant:\n%+v", g, inUTC(want))
	}
	// The instant is kept in its zone: JSON shows the local offset.
	if loc := got.Start.At.Location().String(); loc != "Europe/Paris" {
		t.Fatalf("Start.At is in %q, want Europe/Paris", loc)
	}
	if s := got.Start.At.Format(time.RFC3339); s != "2026-11-16T10:00:00+01:00" {
		t.Fatalf("Start.At = %s", s)
	}
}

func TestDecodeEventAllDayAndFloating(t *testing.T) {
	cases := []struct {
		name       string
		lines      []string
		start, end Time
		exdates    []Time
	}{
		{
			name:    "all-day",
			lines:   []string{"DTSTART;VALUE=DATE:20261224", "DTEND;VALUE=DATE:20261226", "RRULE:FREQ=YEARLY", "EXDATE;VALUE=DATE:20271224"},
			start:   Time{Date: "2026-12-24"},
			end:     Time{Date: "2026-12-26"},
			exdates: []Time{{Date: "2027-12-24"}},
		},
		{
			name:  "floating",
			lines: []string{"DTSTART:20261116T100000", "DTEND:20261116T110000"},
			start: Time{At: utc("2026-11-16T10:00:00Z"), Floating: true},
			end:   Time{At: utc("2026-11-16T11:00:00Z"), Floating: true},
		},
		{
			name:  "utc",
			lines: []string{"DTSTART:20261116T090000Z", "DTEND:20261116T100000Z"},
			start: Time{At: utc("2026-11-16T09:00:00Z")},
			end:   Time{At: utc("2026-11-16T10:00:00Z")},
		},
		{
			name:  "mozilla zone",
			lines: []string{"DTSTART;TZID=/mozilla.org/20070129_1/America/New_York:20261116T100000", "DTEND;TZID=America/New_York:20261116T110000"},
			start: Time{At: utc("2026-11-16T15:00:00Z"), TZID: "America/New_York"},
			end:   Time{At: utc("2026-11-16T16:00:00Z"), TZID: "America/New_York"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := append([]string{"BEGIN:VEVENT", "UID:u", "DTSTAMP:20261101T090000Z"}, c.lines...)
			got, err := DecodeEvent(calendar(append(lines, "END:VEVENT")...))
			if err != nil {
				t.Fatal(err)
			}
			got = inUTC(got)
			if got.Start != c.start || got.End != c.end {
				t.Fatalf("start, end = %+v, %+v; want %+v, %+v", got.Start, got.End, c.start, c.end)
			}
			if !reflect.DeepEqual(got.ExDates, c.exdates) {
				t.Fatalf("exdates = %+v, want %+v", got.ExDates, c.exdates)
			}
		})
	}
}

// TestDecodeEventEndWithoutDTEND: DURATION gives the end (days nominal, in
// the start's zone); with neither, a date lasts one day and a time none
// (RFC 5545 §3.6.1).
func TestDecodeEventEndWithoutDTEND(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		end   Time
	}{
		// 2026-03-29 is the spring-forward day in Paris: one nominal day
		// after 10:00 CET is 10:00 CEST, 23 hours later.
		{"duration across DST", []string{"DTSTART;TZID=Europe/Paris:20260328T100000", "DURATION:P1DT30M"},
			Time{At: utc("2026-03-29T08:30:00Z"), TZID: "Europe/Paris"}},
		{"duration on a date", []string{"DTSTART;VALUE=DATE:20261224", "DURATION:P2D"}, Time{Date: "2026-12-26"}},
		{"duration floating", []string{"DTSTART:20261116T100000", "DURATION:PT45M"}, Time{At: utc("2026-11-16T10:45:00Z"), Floating: true}},
		{"date alone", []string{"DTSTART;VALUE=DATE:20261231"}, Time{Date: "2027-01-01"}},
		{"time alone", []string{"DTSTART:20261116T090000Z"}, Time{At: utc("2026-11-16T09:00:00Z")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lines := append([]string{"BEGIN:VEVENT", "UID:u"}, c.lines...)
			got, err := DecodeEvent(calendar(append(lines, "END:VEVENT")...))
			if err != nil {
				t.Fatal(err)
			}
			if got = inUTC(got); got.End != c.end {
				t.Fatalf("end = %+v, want %+v", got.End, c.end)
			}
			if got.Unknown != 0 {
				t.Fatalf("DURATION counted as unknown: %d", got.Unknown)
			}
		})
	}
}

func TestDecodeEventOverrides(t *testing.T) {
	master := vevent(t, "UID:u", "DTSTAMP:20261101T090000Z", "DTSTART;TZID=Europe/Paris:20261102T100000",
		"DTEND;TZID=Europe/Paris:20261102T110000", "RRULE:FREQ=DAILY;COUNT=5", "SUMMARY:Daily")
	moved := vevent(t, "UID:u", "DTSTAMP:20261101T090000Z", "RECURRENCE-ID;TZID=Europe/Paris:20261104T100000",
		"DTSTART;TZID=Europe/Paris:20261104T150000", "DTEND;TZID=Europe/Paris:20261104T160000", "SUMMARY:Daily (moved)", "X-FOO:bar")
	renamed := vevent(t, "UID:u", "DTSTAMP:20261101T090000Z", "RECURRENCE-ID;TZID=Europe/Paris:20261103T100000",
		"DTSTART;TZID=Europe/Paris:20261103T100000", "DTEND;TZID=Europe/Paris:20261103T110000", "SUMMARY:Daily (renamed)")
	// Rendered as service rendering does: the master, then the exceptions
	// by RecurrenceID.
	vcal := Object(&master, []Exception{{moved, 1793782800}, {renamed, 1793696400}})

	got, err := DecodeEvent(vcal)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Daily" || got.RRule != "FREQ=DAILY;COUNT=5" || got.RecurrenceID != nil {
		t.Fatalf("master = %+v", got)
	}
	if len(got.Overrides) != 2 {
		t.Fatalf("%d overrides, want 2: %+v", len(got.Overrides), got.Overrides)
	}
	wants := []struct {
		summary  string
		rid, at  string
		overUnkn int
	}{
		{"Daily (renamed)", "2026-11-03T09:00:00Z", "2026-11-03T09:00:00Z", 0},
		{"Daily (moved)", "2026-11-04T09:00:00Z", "2026-11-04T14:00:00Z", 1},
	}
	for i, w := range wants {
		o := got.Overrides[i]
		if o.Summary != w.summary || o.RecurrenceID == nil || !o.RecurrenceID.At.Equal(utc(w.rid)) ||
			o.RecurrenceID.TZID != "Europe/Paris" || !o.Start.At.Equal(utc(w.at)) || o.Unknown != w.overUnkn {
			t.Fatalf("override %d = %+v (RecurrenceID %+v), want %+v", i, o, o.RecurrenceID, w)
		}
		if o.Overrides != nil || o.UID != "u" {
			t.Fatalf("override %d: UID %q, overrides %+v", i, o.UID, o.Overrides)
		}
	}
	if got.Unknown != 0 {
		t.Fatalf("master Unknown = %d: an override's property was counted on it", got.Unknown)
	}
}

func TestDecodeEventCountsUnknown(t *testing.T) {
	got, err := DecodeEvent(calendar("BEGIN:VEVENT", "UID:u", "DTSTAMP:20261101T090000Z", "DTSTART:20261116T090000Z",
		"SEQUENCE:0", "SUMMARY:s", "X-FOO:bar", "CATEGORIES:work,home", "END:VEVENT"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Unknown != 2 {
		t.Fatalf("Unknown = %d, want 2", got.Unknown)
	}

	// A repeated single-valued property (the first one is shown) and a
	// nested component other than VALARM are not shown either.
	got, err = DecodeEvent(calendar("BEGIN:VEVENT", "UID:u", "DTSTART:20261116T090000Z", "SUMMARY:first", "SUMMARY:second",
		"BEGIN:X-THING", "A:b", "END:X-THING", "END:VEVENT"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != "first" || got.Unknown != 2 {
		t.Fatalf("Summary %q, Unknown %d; want first, 2", got.Summary, got.Unknown)
	}
}

// TestDecodeEventAlarms: only a trigger relative to the start is an Alarm;
// an absolute one or one relative to the end is counted in Unknown.
func TestDecodeEventAlarms(t *testing.T) {
	got, err := DecodeEvent(calendar("BEGIN:VEVENT", "UID:u", "DTSTART;VALUE=DATE:20261224",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:PT9H", "DESCRIPTION:Reminder (all-day event)", "END:VALARM",
		"BEGIN:VALARM", "ACTION:display", "TRIGGER;RELATED=START:-PT1H", "END:VALARM",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER;VALUE=DATE-TIME:20261223T090000Z", "END:VALARM",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER;RELATED=END:-PT5M", "END:VALARM",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT30S", "END:VALARM",
		"END:VEVENT"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Alarm{{MinutesBefore: -540, Action: "DISPLAY"}, {MinutesBefore: 60, Action: "DISPLAY"}}
	if !reflect.DeepEqual(got.Alarms, want) {
		t.Fatalf("alarms = %+v, want %+v", got.Alarms, want)
	}
	if got.Unknown != 3 {
		t.Fatalf("Unknown = %d, want 3 (absolute, end-relative, sub-minute)", got.Unknown)
	}
}

func TestDecodeEventRejectsTwoMasters(t *testing.T) {
	ev := []string{"BEGIN:VEVENT", "UID:u", "DTSTART:20261116T090000Z", "SUMMARY:secret plans", "END:VEVENT"}
	_, err := DecodeEvent(calendar(append(append([]string{}, ev...), ev...)...))
	if err == nil {
		t.Fatal("two masters decoded")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("the error carries event content: %v", err)
	}
}

func TestDecodeEventMasterless(t *testing.T) {
	orphan := func(day string) []string {
		return []string{"BEGIN:VEVENT", "UID:u", "RECURRENCE-ID:202611" + day + "T090000Z", "DTSTART:202611" + day + "T090000Z", "END:VEVENT"}
	}
	// One occurrence of a series whose master is gone (an invitation to one
	// occurrence) stands for the event.
	got, err := DecodeEvent(calendar(orphan("16")...))
	if err != nil {
		t.Fatal(err)
	}
	if got.RecurrenceID == nil || !got.RecurrenceID.At.Equal(utc("2026-11-16T09:00:00Z")) || got.Overrides != nil {
		t.Fatalf("orphan = %+v", got)
	}
	if _, err := DecodeEvent(calendar(append(orphan("16"), orphan("17")...)...)); err == nil {
		t.Fatal("two occurrences without a master decoded")
	}
}

func TestDecodeEventRejects(t *testing.T) {
	cases := map[string][]byte{
		"no event":        calendar(),
		"bare VEVENT":     []byte("BEGIN:VEVENT\r\nUID:u\r\nDTSTART:20261116T090000Z\r\nEND:VEVENT\r\n"),
		"two calendars":   append(calendar("BEGIN:VEVENT", "UID:u", "DTSTART:20261116T090000Z", "END:VEVENT"), calendar()...),
		"malformed":       []byte("BEGIN:VCALENDAR\r\nnot a line\r\nEND:VCALENDAR\r\n"),
		"no DTSTART":      calendar("BEGIN:VEVENT", "UID:u", "END:VEVENT"),
		"unknown zone":    calendar("BEGIN:VEVENT", "UID:u", "DTSTART;TZID=Mars/Olympus:20261116T090000", "END:VEVENT"),
		"bad date":        calendar("BEGIN:VEVENT", "UID:u", "DTSTART;VALUE=DATE:2026-11-16", "END:VEVENT"),
		"bad exdate":      calendar("BEGIN:VEVENT", "UID:u", "DTSTART:20261116T090000Z", "EXDATE:20261117T09", "END:VEVENT"),
		"bad duration":    calendar("BEGIN:VEVENT", "UID:u", "DTSTART:20261116T090000Z", "DURATION:1h", "END:VEVENT"),
		"bad override id": calendar("BEGIN:VEVENT", "UID:u", "DTSTART:20261116T090000Z", "END:VEVENT", "BEGIN:VEVENT", "UID:u", "RECURRENCE-ID:x", "DTSTART:20261116T090000Z", "END:VEVENT"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if d, err := DecodeEvent(in); err == nil {
				t.Fatalf("decoded: %+v", d)
			}
		})
	}
}

// FuzzDecodeEvent: any input decodes or fails without panicking, and a
// decoded event keeps the view's shape.
func FuzzDecodeEvent(f *testing.F) {
	f.Add(calendar("BEGIN:VEVENT", "UID:u", "DTSTART;TZID=Europe/Paris:20261102T100000", "DURATION:P1W", "RRULE:FREQ=DAILY",
		"EXDATE;VALUE=DATE:20261103", `ATTENDEE;CN="a;b":mailto:a@x`, "BEGIN:VALARM", "TRIGGER:-PT1H", "END:VALARM", "END:VEVENT",
		"BEGIN:VEVENT", "UID:u", "RECURRENCE-ID:20261104T090000Z", "DTSTART:20261104T090000", "END:VEVENT"))
	f.Add(calendar("BEGIN:VEVENT", "UID:u", "DTSTART;VALUE=DATE:20261224", "DURATION:P99999999W", "END:VEVENT"))
	f.Fuzz(func(t *testing.T, in []byte) {
		d, err := DecodeEvent(in)
		if err != nil {
			return
		}
		for _, tm := range append([]Time{d.Start, d.End}, d.ExDates...) {
			if tm.Date != "" && (!tm.At.IsZero() || tm.TZID != "" || tm.Floating) {
				t.Fatalf("a date with a time: %+v", tm)
			}
		}
		for _, o := range d.Overrides {
			if o.RecurrenceID == nil || o.Overrides != nil {
				t.Fatalf("override without RECURRENCE-ID or with overrides: %+v", o)
			}
		}
		if len(d.Overrides) > 0 && d.RecurrenceID != nil {
			t.Fatal("overrides on an occurrence")
		}
	})
}

func ExampleDecodeEvent() {
	d, err := DecodeEvent(calendar("BEGIN:VEVENT", "UID:u", "DTSTART;VALUE=DATE:20261224", "SUMMARY:Christmas Eve", "END:VEVENT"))
	fmt.Println(d.Summary, d.Start.Date, d.End.Date, err)
	// Output: Christmas Eve 2026-12-24 2026-12-25 <nil>
}
