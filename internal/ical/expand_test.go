package ical

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// xEvent is one VEVENT of the series u-x with the given lines.
func xEvent(lines ...string) string {
	return "BEGIN:VEVENT\r\nUID:u-x\r\nDTSTAMP:20261101T080000Z\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\n"
}

// xSeries parses a VCALENDAR of events and splits it into the master (the
// VEVENT without RECURRENCE-ID; the zero Component when there is none) and
// the overrides.
func xSeries(t *testing.T, events ...string) (Component, []Component) {
	t.Helper()
	top, err := Parse([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" + strings.Join(events, "") + "END:VCALENDAR\r\n"))
	if err != nil || len(top) != 1 {
		t.Fatalf("Parse: %v (%d components)", err, len(top))
	}
	var master Component
	var overrides []Component
	for _, ev := range top[0].Subs("VEVENT") {
		if _, ok := ev.Get("RECURRENCE-ID"); ok {
			overrides = append(overrides, ev)
		} else {
			master = ev
		}
	}
	return master, overrides
}

func xLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// xInstant parses an RFC 3339 instant.
func xInstant(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// xExpand expands the series with a large cap and fails on error.
func xExpand(t *testing.T, master Component, overrides []Component, from, to string, loc *time.Location, max int) ([]Occurrence, bool) {
	t.Helper()
	occ, truncated, err := Expand(master, overrides, xInstant(t, from), xInstant(t, to), loc, max)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	return occ, truncated
}

// starts lists the occurrence starts as RFC 3339 UTC instants.
func starts(occ []Occurrence) []string {
	out := make([]string, len(occ))
	for i, o := range occ {
		out[i] = o.Start.UTC().Format(time.RFC3339)
	}
	return out
}

// walls lists the occurrence starts as wall clocks of loc.
func walls(occ []Occurrence, loc *time.Location) []string {
	out := make([]string, len(occ))
	for i, o := range occ {
		out[i] = o.Start.In(loc).Format("2006-01-02T15:04")
	}
	return out
}

func sameList(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s:\n got  %v\n want %v", what, got, want)
	}
}

func TestExpandSingleEvent(t *testing.T) {
	m, _ := xSeries(t, xEvent("DTSTART;TZID=Europe/Paris:20261114T150000", "DTEND;TZID=Europe/Paris:20261114T160000"))
	occ, truncated := xExpand(t, m, nil, "2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z", time.UTC, 1000)
	if len(occ) != 1 || truncated {
		t.Fatalf("got %d occurrences (truncated %v), want 1", len(occ), truncated)
	}
	o := occ[0]
	if !o.Start.Equal(xInstant(t, "2026-11-14T14:00:00Z")) || !o.End.Equal(xInstant(t, "2026-11-14T15:00:00Z")) {
		t.Errorf("times %v..%v, want 14:00Z..15:00Z", o.Start, o.End)
	}
	if o.TZID != "Europe/Paris" || o.AllDay || o.Floating || o.Override || o.RecurrenceID != nil {
		t.Errorf("occurrence %+v: want TZID Europe/Paris, timed, not floating, no override, no RecurrenceID", o)
	}
	// Outside the range: nothing. The range is half-open: an event ending at
	// from, or starting at to, does not overlap.
	for _, r := range [][2]string{{"2026-11-14T15:00:00Z", "2026-11-15T00:00:00Z"}, {"2026-11-14T00:00:00Z", "2026-11-14T14:00:00Z"}} {
		if occ, _ := xExpand(t, m, nil, r[0], r[1], time.UTC, 1000); len(occ) != 0 {
			t.Errorf("range %v: got %v, want none", r, starts(occ))
		}
	}
	// A range that starts inside the event still shows it.
	if occ, _ := xExpand(t, m, nil, "2026-11-14T14:30:00Z", "2026-11-14T14:45:00Z", time.UTC, 1000); len(occ) != 1 {
		t.Errorf("range inside the event: got %d occurrences, want 1", len(occ))
	}
}

func TestExpandZeroLengthEventAtFrom(t *testing.T) {
	m, _ := xSeries(t, xEvent("DTSTART:20261114T140000Z"))
	if occ, _ := xExpand(t, m, nil, "2026-11-14T14:00:00Z", "2026-11-14T15:00:00Z", time.UTC, 10); len(occ) != 1 {
		t.Errorf("an instant event at from: got %d, want 1", len(occ))
	}
	if occ, _ := xExpand(t, m, nil, "2026-11-14T13:00:00Z", "2026-11-14T14:00:00Z", time.UTC, 10); len(occ) != 0 {
		t.Errorf("an instant event at to: got %d, want 0", len(occ))
	}
}

func TestExpandWeeklyKeepsWallClockAcrossDST(t *testing.T) {
	m, _ := xSeries(t, xEvent("DTSTART;TZID=Europe/Paris:20260322T023000", "RRULE:FREQ=WEEKLY;COUNT=3"))
	occ, _ := xExpand(t, m, nil, "2026-03-01T00:00:00Z", "2026-05-01T00:00:00Z", time.UTC, 1000)
	sameList(t, "starts", starts(occ), []string{"2026-03-22T01:30:00Z", "2026-03-29T01:30:00Z", "2026-04-05T00:30:00Z"})
}

// TestExpandDSTTransitions: weekly series at the transition time, the second
// occurrence on the change day. A gap reads the wall clock with the offset
// before the gap; an overlap takes the first of the two instants (RFC 5545
// §3.3.5 via §3.3.10). Go's time.Date picks either; the rows marked E16 are
// those where it (and rrule-go) gives the other instant.
func TestExpandDSTTransitions(t *testing.T) {
	cases := []struct {
		name, dtstart string
		want          []string
	}{
		{"Paris gap", "Europe/Paris:20260322T023000", []string{"2026-03-22T01:30:00Z", "2026-03-29T01:30:00Z", "2026-04-05T00:30:00Z"}},
		{"Paris overlap (E16)", "Europe/Paris:20261018T023000", []string{"2026-10-18T00:30:00Z", "2026-10-25T00:30:00Z", "2026-11-01T01:30:00Z"}},
		{"New York gap (E16)", "America/New_York:20260301T023000", []string{"2026-03-01T07:30:00Z", "2026-03-08T07:30:00Z", "2026-03-15T06:30:00Z"}},
		{"New York overlap", "America/New_York:20261025T013000", []string{"2026-10-25T05:30:00Z", "2026-11-01T05:30:00Z", "2026-11-08T06:30:00Z"}},
		{"Sydney gap", "Australia/Sydney:20260927T023000", []string{"2026-09-26T16:30:00Z", "2026-10-03T16:30:00Z", "2026-10-10T15:30:00Z"}},
		{"Sydney overlap (E16)", "Australia/Sydney:20260329T023000", []string{"2026-03-28T15:30:00Z", "2026-04-04T15:30:00Z", "2026-04-11T16:30:00Z"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := xSeries(t, xEvent("DTSTART;TZID="+c.dtstart, "DURATION:PT1H", "RRULE:FREQ=WEEKLY;COUNT=3"))
			occ, _ := xExpand(t, m, nil, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z", time.UTC, 1000)
			sameList(t, "starts", starts(occ), c.want)
			for _, o := range occ {
				if o.End.Sub(o.Start) != time.Hour {
					t.Errorf("occurrence at %v lasts %v, want 1h", o.Start, o.End.Sub(o.Start))
				}
				if o.RecurrenceID == nil || !o.RecurrenceID.Equal(o.Start) {
					t.Errorf("occurrence at %v: RecurrenceID %v, want its start", o.Start, o.RecurrenceID)
				}
			}
		})
	}
}

func TestExpandExdateRemovesOccurrence(t *testing.T) {
	m, _ := xSeries(t, xEvent("DTSTART;TZID=Europe/Paris:20260105T100000", "RRULE:FREQ=DAILY;COUNT=4",
		"EXDATE;TZID=Europe/Paris:20260106T100000,20260108T100000"))
	occ, _ := xExpand(t, m, nil, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", time.UTC, 1000)
	// COUNT counts the rule's occurrences; the EXDATEs remove two of the four.
	sameList(t, "starts", starts(occ), []string{"2026-01-05T09:00:00Z", "2026-01-07T09:00:00Z"})
}

func TestExpandOverrideReplacesAndMoves(t *testing.T) {
	m, ov := xSeries(t,
		xEvent("DTSTART;TZID=Europe/Paris:20260105T100000", "DTEND;TZID=Europe/Paris:20260105T110000", "RRULE:FREQ=WEEKLY;COUNT=6"),
		// The 01-12 occurrence moved out of the range (to March).
		xEvent("RECURRENCE-ID;TZID=Europe/Paris:20260112T100000", "DTSTART;TZID=Europe/Paris:20260302T100000", "DTEND;TZID=Europe/Paris:20260302T110000"),
		// The 01-19 occurrence moved within the range, to 14:00.
		xEvent("RECURRENCE-ID;TZID=Europe/Paris:20260119T100000", "DTSTART;TZID=Europe/Paris:20260119T140000", "DTEND;TZID=Europe/Paris:20260119T150000"),
		// The 02-09 occurrence (out of the range) moved into it.
		xEvent("RECURRENCE-ID:20260209T090000Z", "DTSTART;TZID=America/New_York:20260115T080000", "DTEND;TZID=America/New_York:20260115T090000"),
	)
	occ, _ := xExpand(t, m, ov, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", time.UTC, 1000)
	sameList(t, "starts", starts(occ), []string{
		"2026-01-05T09:00:00Z", "2026-01-15T13:00:00Z", "2026-01-19T13:00:00Z", "2026-01-26T09:00:00Z",
	})
	wantOverride := []bool{false, true, true, false}
	wantRID := []string{"2026-01-05T09:00:00Z", "2026-02-09T09:00:00Z", "2026-01-19T09:00:00Z", "2026-01-26T09:00:00Z"}
	for i, o := range occ {
		if o.Override != wantOverride[i] {
			t.Errorf("occurrence %d: Override %v, want %v", i, o.Override, wantOverride[i])
		}
		if o.RecurrenceID == nil || o.RecurrenceID.UTC().Format(time.RFC3339) != wantRID[i] {
			t.Errorf("occurrence %d: RecurrenceID %v, want %s", i, o.RecurrenceID, wantRID[i])
		}
	}
	if occ[1].TZID != "America/New_York" || occ[1].End.Sub(occ[1].Start) != time.Hour {
		t.Errorf("the moved-in override: TZID %q, length %v; want its own zone and times", occ[1].TZID, occ[1].End.Sub(occ[1].Start))
	}
	// In March the override moved out shows instead.
	occ, _ = xExpand(t, m, ov, "2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z", time.UTC, 1000)
	if len(occ) != 1 || !occ[0].Override || occ[0].Start.UTC().Format(time.RFC3339) != "2026-03-02T09:00:00Z" {
		t.Errorf("March: got %v, want the override at 2026-03-02T09:00:00Z", starts(occ))
	}
}

// A series whose master is gone (the zero Component) still shows its
// overrides.
func TestExpandOverridesWithoutMaster(t *testing.T) {
	_, ov := xSeries(t, xEvent("RECURRENCE-ID:20260112T090000Z", "DTSTART:20260112T120000Z", "DTEND:20260112T130000Z"))
	occ, _, err := Expand(Component{}, ov, xInstant(t, "2026-01-01T00:00:00Z"), xInstant(t, "2026-02-01T00:00:00Z"), time.UTC, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(occ) != 1 || !occ[0].Override || occ[0].Start.UTC().Format(time.RFC3339) != "2026-01-12T12:00:00Z" {
		t.Errorf("got %v, want the override at 12:00Z", starts(occ))
	}
}

func TestExpandAllDayInAFarZone(t *testing.T) {
	auckland := xLoc(t, "Pacific/Auckland")
	m, _ := xSeries(t, xEvent("DTSTART;VALUE=DATE:20261114", "DTEND;VALUE=DATE:20261115"))
	occ, _ := xExpand(t, m, nil, "2026-11-13T00:00:00Z", "2026-11-15T00:00:00Z", auckland, 1000)
	if len(occ) != 1 {
		t.Fatalf("got %d occurrences, want 1", len(occ))
	}
	o := occ[0]
	if !o.AllDay || o.StartDate != "2026-11-14" || o.EndDate != "2026-11-15" || o.TZID != "" {
		t.Errorf("occurrence %+v: want all-day 2026-11-14..2026-11-15 without TZID", o)
	}
	// Midnight in Auckland (NZDT, +13).
	if !o.Start.Equal(xInstant(t, "2026-11-13T11:00:00Z")) || !o.End.Equal(xInstant(t, "2026-11-14T11:00:00Z")) {
		t.Errorf("instants %v..%v, want Auckland midnights", o.Start.UTC(), o.End.UTC())
	}
	// Seen from Auckland, 2026-11-14T12:00Z is already the 15th: no overlap.
	if occ, _ := xExpand(t, m, nil, "2026-11-14T12:00:00Z", "2026-11-16T00:00:00Z", auckland, 1000); len(occ) != 0 {
		t.Errorf("after the Auckland day: got %+v, want none", occ)
	}
}

func TestExpandAllDayRecurring(t *testing.T) {
	paris := xLoc(t, "Europe/Paris")
	m, ov := xSeries(t,
		xEvent("DTSTART;VALUE=DATE:20261023", "RRULE:FREQ=WEEKLY;UNTIL=20261113", "EXDATE;VALUE=DATE:20261106"),
		xEvent("RECURRENCE-ID;VALUE=DATE:20261030", "DTSTART;VALUE=DATE:20261031", "DTEND;VALUE=DATE:20261102"),
	)
	occ, _ := xExpand(t, m, ov, "2026-10-01T00:00:00Z", "2026-12-01T00:00:00Z", paris, 1000)
	var dates []string
	for _, o := range occ {
		if !o.AllDay {
			t.Errorf("occurrence %+v is not all-day", o)
		}
		dates = append(dates, o.StartDate+".."+o.EndDate)
	}
	// No DTEND: one day. UNTIL as a DATE is inclusive; 11-06 is an EXDATE;
	// the 10-30 occurrence is overridden by a two-day event from 10-31.
	sameList(t, "dates", dates, []string{"2026-10-23..2026-10-24", "2026-10-31..2026-11-02", "2026-11-13..2026-11-14"})
	// Paris midnights, CEST before 10-25 and CET after.
	sameList(t, "starts", starts(occ), []string{"2026-10-22T22:00:00Z", "2026-10-30T23:00:00Z", "2026-11-12T23:00:00Z"})
	if !occ[1].Override || occ[1].RecurrenceID == nil || !occ[1].RecurrenceID.Equal(xInstant(t, "2026-10-29T23:00:00Z")) {
		t.Errorf("override: %+v, want Override and RecurrenceID at the 10-30 Paris midnight", occ[1])
	}
}

func TestExpandFloatingUsesLoc(t *testing.T) {
	m, _ := xSeries(t, xEvent("DTSTART:20261114T090000", "DTEND:20261114T100000", "RRULE:FREQ=DAILY;COUNT=2"))
	for _, c := range []struct {
		zone string
		want []string
	}{
		{"America/New_York", []string{"2026-11-14T14:00:00Z", "2026-11-15T14:00:00Z"}},
		{"Asia/Tokyo", []string{"2026-11-14T00:00:00Z", "2026-11-15T00:00:00Z"}},
	} {
		occ, _ := xExpand(t, m, nil, "2026-11-13T00:00:00Z", "2026-11-17T00:00:00Z", xLoc(t, c.zone), 1000)
		sameList(t, c.zone, starts(occ), c.want)
		for _, o := range occ {
			if !o.Floating || o.TZID != "" || o.End.Sub(o.Start) != time.Hour {
				t.Errorf("%s: occurrence %+v, want floating, no TZID, 1h", c.zone, o)
			}
		}
	}
}

func TestExpandCapsPerSeries(t *testing.T) {
	m, ov := xSeries(t,
		xEvent("DTSTART:20260101T090000Z", "RRULE:FREQ=DAILY"),
		xEvent("RECURRENCE-ID:20260102T090000Z", "DTSTART:20260102T100000Z"),
	)
	occ, truncated := xExpand(t, m, ov, "2026-01-01T00:00:00Z", "2029-01-01T00:00:00Z", time.UTC, 1000)
	if len(occ) != 1000 || !truncated {
		t.Fatalf("got %d occurrences, truncated %v; want 1000, true", len(occ), truncated)
	}
	if got := occ[999].Start.UTC().Format(time.RFC3339); got != "2028-09-26T09:00:00Z" {
		t.Errorf("last occurrence %s, want 2028-09-26T09:00:00Z (the 1000th day)", got)
	}
	if !occ[1].Override {
		t.Errorf("the override is not the second occurrence: %+v", occ[1])
	}
	// Under the cap: every occurrence, not truncated.
	occ, truncated = xExpand(t, m, ov, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z", time.UTC, 1000)
	if len(occ) != 365 || truncated {
		t.Errorf("one year: got %d, truncated %v; want 365, false", len(occ), truncated)
	}
	// Exactly at the cap: not truncated.
	if occ, truncated = xExpand(t, m, ov, "2026-01-01T00:00:00Z", "2026-01-11T00:00:00Z", time.UTC, 10); len(occ) != 10 || truncated {
		t.Errorf("ten days, max 10: got %d, truncated %v; want 10, false", len(occ), truncated)
	}
	if _, _, err := Expand(m, ov, xInstant(t, "2026-01-01T00:00:00Z"), xInstant(t, "2026-01-11T00:00:00Z"), time.UTC, 0); err == nil {
		t.Error("max 0 is accepted")
	}
}

func TestExpandUntilInclusive(t *testing.T) {
	cases := []struct {
		name, dtstart, rule, zone string
		want                      []string // local wall clocks in zone
	}{
		{"UTC UNTIL at an occurrence", "Europe/Paris:20260105T100000", "FREQ=DAILY;UNTIL=20260107T090000Z", "Europe/Paris",
			[]string{"2026-01-05T10:00", "2026-01-06T10:00", "2026-01-07T10:00"}},
		{"UTC UNTIL a second before", "Europe/Paris:20260105T100000", "FREQ=DAILY;UNTIL=20260107T085959Z", "Europe/Paris",
			[]string{"2026-01-05T10:00", "2026-01-06T10:00"}},
		// A DATE UNTIL on a timed start keeps the whole local day (not
		// midnight, as rrule-go reads it).
		{"DATE UNTIL keeps the day", "Europe/Paris:20260105T100000", "FREQ=DAILY;UNTIL=20260107", "Europe/Paris",
			[]string{"2026-01-05T10:00", "2026-01-06T10:00", "2026-01-07T10:00"}},
		// The local day, not the UTC one: 01-08 08:00 NZDT is 01-07 19:00Z.
		{"DATE UNTIL in the start's zone", "Pacific/Auckland:20260105T080000", "FREQ=DAILY;UNTIL=20260107", "Pacific/Auckland",
			[]string{"2026-01-05T08:00", "2026-01-06T08:00", "2026-01-07T08:00"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := xSeries(t, xEvent("DTSTART;TZID="+c.dtstart, "RRULE:"+c.rule))
			occ, _ := xExpand(t, m, nil, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", time.UTC, 1000)
			sameList(t, "starts", walls(occ, xLoc(t, c.zone)), c.want)
		})
	}
}

// TestExpandFarRangeMatchesWalk: whatever shortcut long series take, a range
// far from DTSTART yields exactly the occurrences a walk from DTSTART yields
// in it.
func TestExpandFarRangeMatchesWalk(t *testing.T) {
	cases := []struct{ dtstart, rule string }{
		{"Europe/Paris:20200105T233000", "FREQ=DAILY"},
		{"Europe/Paris:20200105T233000", "FREQ=DAILY;INTERVAL=7"},
		{"America/New_York:20200103T013000", "FREQ=WEEKLY;INTERVAL=3;BYDAY=MO,FR,SU;WKST=SU"},
		{"Europe/Paris:20200130T100000", "FREQ=MONTHLY;INTERVAL=5"},
		{"Europe/Paris:20200131T100000", "FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1"},
		{"Europe/Paris:20200315T100000", "FREQ=YEARLY"},
		{"Europe/Paris:20200105T100000", "FREQ=DAILY;UNTIL=20261231T000000Z"},
	}
	from, to := xInstant(t, "2026-03-01T00:00:00Z"), xInstant(t, "2027-03-01T00:00:00Z")
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			// A multi-day event: occurrences that start before from still overlap.
			m, _ := xSeries(t, xEvent("DTSTART;TZID="+c.dtstart, "DURATION:P2DT3H", "RRULE:"+c.rule))
			all, truncated, err := Expand(m, nil, xInstant(t, "2019-01-01T00:00:00Z"), to, time.UTC, 100000)
			if err != nil || truncated {
				t.Fatalf("walk: %v, truncated %v", err, truncated)
			}
			var want []Occurrence
			for _, o := range all {
				if o.Start.Before(to) && o.End.After(from) {
					want = append(want, o)
				}
			}
			if len(want) == 0 {
				t.Fatal("the walk has nothing in the range")
			}
			got, _, err := Expand(m, nil, from, to, time.UTC, 100000)
			if err != nil {
				t.Fatal(err)
			}
			sameList(t, "starts", starts(got), starts(want))
		})
	}
	// And against counting, independently of the expander: every day of
	// 2026-06 of a daily series started 2020-01-01.
	m, _ := xSeries(t, xEvent("DTSTART;TZID=Europe/Paris:20200101T120000", "RRULE:FREQ=DAILY"))
	occ, _ := xExpand(t, m, nil, "2026-06-01T00:00:00Z", "2026-07-01T00:00:00Z", time.UTC, 1000)
	var want []string
	for d := 1; d <= 30; d++ {
		want = append(want, fmt.Sprintf("2026-06-%02dT12:00", d))
	}
	sameList(t, "June 2026", walls(occ, xLoc(t, "Europe/Paris")), want)
}

// TestExpandRFC5545Examples: the RFC 5545 §3.8.5.3 examples whose rules are
// in Proton's set (12 and 13 through ruling R5's rewrite of BYDAY=1FR), as
// E16 lists them. Starts are America/New_York wall clocks at 09:00.
func TestExpandRFC5545Examples(t *testing.T) {
	ny := xLoc(t, "America/New_York")
	days := func(from, to string) []string { // every day from..to inclusive
		var out []string
		d, _ := time.Parse("2006-01-02", from)
		end, _ := time.Parse("2006-01-02", to)
		for ; !d.After(end); d = d.AddDate(0, 0, 1) {
			out = append(out, d.Format("2006-01-02"))
		}
		return out
	}
	cases := []struct {
		name, dtstart, rule string
		from, to            string // range; empty means 1997-01-01..1999-01-01
		max                 int    // 0: 1000
		want                []string
	}{
		{"1 daily x10", "19970902", "FREQ=DAILY;COUNT=10", "", "", 0, days("1997-09-02", "1997-09-11")},
		{"2 daily until", "19970902", "FREQ=DAILY;UNTIL=19971224T000000Z", "", "", 0, days("1997-09-02", "1997-12-23")},
		{"3 every other day", "19970902", "FREQ=DAILY;INTERVAL=2", "1997-10-20T00:00:00Z", "1997-11-02T00:00:00Z", 0,
			[]string{"1997-10-20", "1997-10-22", "1997-10-24", "1997-10-26", "1997-10-28", "1997-10-30", "1997-11-01"}},
		{"4 every 10 days x5", "19970902", "FREQ=DAILY;INTERVAL=10;COUNT=5", "", "", 0,
			[]string{"1997-09-02", "1997-09-12", "1997-09-22", "1997-10-02", "1997-10-12"}},
		{"6 weekly x10", "19970902", "FREQ=WEEKLY;COUNT=10", "", "", 0,
			[]string{"1997-09-02", "1997-09-09", "1997-09-16", "1997-09-23", "1997-09-30", "1997-10-07", "1997-10-14", "1997-10-21", "1997-10-28", "1997-11-04"}},
		{"7 weekly until", "19970902", "FREQ=WEEKLY;UNTIL=19971224T000000Z", "", "", 0,
			[]string{"1997-09-02", "1997-09-09", "1997-09-16", "1997-09-23", "1997-09-30", "1997-10-07", "1997-10-14", "1997-10-21", "1997-10-28",
				"1997-11-04", "1997-11-11", "1997-11-18", "1997-11-25", "1997-12-02", "1997-12-09", "1997-12-16", "1997-12-23"}},
		{"8 every other week", "19970902", "FREQ=WEEKLY;INTERVAL=2;WKST=SU", "1997-09-01T00:00:00Z", "1998-03-01T00:00:00Z", 0,
			[]string{"1997-09-02", "1997-09-16", "1997-09-30", "1997-10-14", "1997-10-28", "1997-11-11", "1997-11-25", "1997-12-09", "1997-12-23",
				"1998-01-06", "1998-01-20", "1998-02-03", "1998-02-17"}},
		{"9 tu,th five weeks (until)", "19970902", "FREQ=WEEKLY;UNTIL=19971007T000000Z;WKST=SU;BYDAY=TU,TH", "", "", 0,
			[]string{"1997-09-02", "1997-09-04", "1997-09-09", "1997-09-11", "1997-09-16", "1997-09-18", "1997-09-23", "1997-09-25", "1997-09-30", "1997-10-02"}},
		{"9 tu,th five weeks (count)", "19970902", "FREQ=WEEKLY;COUNT=10;WKST=SU;BYDAY=TU,TH", "", "", 0,
			[]string{"1997-09-02", "1997-09-04", "1997-09-09", "1997-09-11", "1997-09-16", "1997-09-18", "1997-09-23", "1997-09-25", "1997-09-30", "1997-10-02"}},
		{"10 every other week mo,we,fr", "19970901", "FREQ=WEEKLY;INTERVAL=2;UNTIL=19971224T000000Z;WKST=SU;BYDAY=MO,WE,FR", "", "", 0,
			[]string{"1997-09-01", "1997-09-03", "1997-09-05", "1997-09-15", "1997-09-17", "1997-09-19", "1997-09-29",
				"1997-10-01", "1997-10-03", "1997-10-13", "1997-10-15", "1997-10-17", "1997-10-27", "1997-10-29", "1997-10-31",
				"1997-11-10", "1997-11-12", "1997-11-14", "1997-11-24", "1997-11-26", "1997-11-28",
				"1997-12-08", "1997-12-10", "1997-12-12", "1997-12-22"}},
		{"11 every other week tu,th x8", "19970902", "FREQ=WEEKLY;INTERVAL=2;COUNT=8;WKST=SU;BYDAY=TU,TH", "", "", 0,
			[]string{"1997-09-02", "1997-09-04", "1997-09-16", "1997-09-18", "1997-09-30", "1997-10-02", "1997-10-14", "1997-10-16"}},
		{"12 first friday x10", "19970905", "FREQ=MONTHLY;COUNT=10;BYDAY=1FR", "", "", 0,
			[]string{"1997-09-05", "1997-10-03", "1997-11-07", "1997-12-05", "1998-01-02", "1998-02-06", "1998-03-06", "1998-04-03", "1998-05-01", "1998-06-05"}},
		{"13 first friday until", "19970905", "FREQ=MONTHLY;UNTIL=19971224T000000Z;BYDAY=1FR", "", "", 0,
			[]string{"1997-09-05", "1997-10-03", "1997-11-07", "1997-12-05"}},
		{"16 third-to-last day", "19970928", "FREQ=MONTHLY;BYMONTHDAY=-3", "", "", 6,
			[]string{"1997-09-28", "1997-10-29", "1997-11-28", "1997-12-29", "1998-01-29", "1998-02-26"}},
		{"37 WKST=MO", "19970805", "FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO", "", "", 0,
			[]string{"1997-08-05", "1997-08-10", "1997-08-19", "1997-08-24"}},
		{"38 WKST=SU", "19970805", "FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=SU", "", "", 0,
			[]string{"1997-08-05", "1997-08-17", "1997-08-19", "1997-08-31"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := xSeries(t, xEvent("DTSTART;TZID=America/New_York:"+c.dtstart+"T090000", "RRULE:"+c.rule))
			from, to, max := c.from, c.to, c.max
			if from == "" {
				from, to = "1997-01-01T00:00:00Z", "1999-01-01T00:00:00Z"
			}
			if max == 0 {
				max = 1000
			}
			occ, _ := xExpand(t, m, nil, from, to, time.UTC, max)
			want := make([]string, len(c.want))
			for i, d := range c.want {
				want[i] = d + "T09:00"
			}
			sameList(t, "starts", walls(occ, ny), want)
		})
	}
}

// TestExpandProtonForms: E16's appendix, the rule shapes Proton accepts.
// DTSTART Europe/Paris 10:00 unless the case gives a time; "first n" cases
// are unbounded rules checked on their first n occurrences (max n).
func TestExpandProtonForms(t *testing.T) {
	paris := xLoc(t, "Europe/Paris")
	cases := []struct {
		id, dtstart, rule string
		extra             []string
		first             int
		want              []string // local dates, or local "date Thh:mm" when the time is not 10:00
	}{
		{"P01", "20260105", "FREQ=DAILY", nil, 5, []string{"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08", "2026-01-09"}},
		{"P03", "20260105", "FREQ=DAILY;INTERVAL=999;COUNT=3", nil, 0, []string{"2026-01-05", "2028-09-30", "2031-06-26"}},
		{"P04", "20260105", "FREQ=DAILY;UNTIL=20260108T090000Z", nil, 0, []string{"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08"}},
		{"P05", "20260105", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR", nil, 10, []string{"2026-01-05", "2026-01-06", "2026-01-07", "2026-01-08", "2026-01-09",
			"2026-01-12", "2026-01-13", "2026-01-14", "2026-01-15", "2026-01-16"}},
		{"P06", "20260105", "FREQ=WEEKLY;INTERVAL=2;BYDAY=MO,WE;WKST=SU;COUNT=4", nil, 0, []string{"2026-01-05", "2026-01-07", "2026-01-19", "2026-01-21"}},
		{"P07", "20260106", "FREQ=WEEKLY;INTERVAL=2;COUNT=4;BYDAY=TU,SU;WKST=MO", nil, 0, []string{"2026-01-06", "2026-01-11", "2026-01-20", "2026-01-25"}},
		{"P08", "20260105", "FREQ=WEEKLY;INTERVAL=4999;COUNT=2", nil, 0, []string{"2026-01-05", "2121-10-27"}},
		{"P09", "20260115", "FREQ=MONTHLY;COUNT=3", nil, 0, []string{"2026-01-15", "2026-02-15", "2026-03-15"}},
		{"P10", "20260131", "FREQ=MONTHLY;BYMONTHDAY=31;COUNT=4", nil, 0, []string{"2026-01-31", "2026-03-31", "2026-05-31", "2026-07-31"}},
		{"P11", "20260131", "FREQ=MONTHLY;COUNT=3", nil, 0, []string{"2026-01-31", "2026-03-31", "2026-05-31"}},
		{"P12", "20260131", "FREQ=MONTHLY;BYMONTHDAY=-1;COUNT=4", nil, 0, []string{"2026-01-31", "2026-02-28", "2026-03-31", "2026-04-30"}},
		{"P13", "20260113", "FREQ=MONTHLY;BYDAY=TU;BYSETPOS=2;COUNT=4", nil, 0, []string{"2026-01-13", "2026-02-10", "2026-03-10", "2026-04-14"}},
		{"P14", "20260130", "FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1;COUNT=4", nil, 0, []string{"2026-01-30", "2026-02-27", "2026-03-27", "2026-04-24"}},
		{"P15", "20260104", "FREQ=MONTHLY;BYDAY=SU;BYSETPOS=1;COUNT=3", nil, 0, []string{"2026-01-04", "2026-02-01", "2026-03-01"}},
		{"P16", "20260121", "FREQ=MONTHLY;BYDAY=WE;BYSETPOS=3;COUNT=3", nil, 0, []string{"2026-01-21", "2026-02-18", "2026-03-18"}},
		{"P17", "20260122", "FREQ=MONTHLY;BYDAY=TH;BYSETPOS=4;COUNT=3", nil, 0, []string{"2026-01-22", "2026-02-26", "2026-03-26"}},
		{"P18", "20260113", "FREQ=MONTHLY;INTERVAL=3;BYDAY=TU;BYSETPOS=2;COUNT=3", nil, 0, []string{"2026-01-13", "2026-04-14", "2026-07-14"}},
		{"P19", "20260313", "FREQ=YEARLY;COUNT=3", nil, 0, []string{"2026-03-13", "2027-03-13", "2028-03-13"}},
		{"P20", "20280229", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=29;COUNT=3", nil, 0, []string{"2028-02-29", "2032-02-29", "2036-02-29"}},
		{"P21", "20280229", "FREQ=YEARLY;COUNT=2", nil, 0, []string{"2028-02-29", "2032-02-29"}},
		{"P22", "20260228", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=-1;COUNT=3", nil, 0, []string{"2026-02-28", "2027-02-28", "2028-02-29"}},
		{"P23", "20260313", "FREQ=YEARLY;INTERVAL=99;COUNT=2", nil, 0, []string{"2026-03-13", "2125-03-13"}},
		{"P24", "20371229", "FREQ=DAILY;UNTIL=20371231T225959Z", nil, 0, []string{"2037-12-29", "2037-12-30", "2037-12-31"}},
		{"P25", "20260105", "FREQ=DAILY;COUNT=5", []string{"EXDATE;TZID=Europe/Paris:20260106T100000"}, 0,
			[]string{"2026-01-05", "2026-01-07", "2026-01-08", "2026-01-09"}},
		{"P26", "20260105", "FREQ=DAILY;COUNT=3", []string{"EXDATE:20260106T090000Z"}, 0, []string{"2026-01-05", "2026-01-07"}},
		{"P27", "20260327", "FREQ=DAILY;COUNT=4", nil, 0, []string{"2026-03-27", "2026-03-28", "2026-03-29", "2026-03-30"}},
		{"P28", "20261018", "FREQ=WEEKLY;COUNT=3", nil, 0, []string{"2026-10-18", "2026-10-25", "2026-11-01"}},
		// DTSTART off the rule (a Wednesday, Mondays only) is the first
		// occurrence and counts towards COUNT (RFC 5545 §3.8.5.3; E16 call).
		{"P29", "20260107", "FREQ=WEEKLY;BYDAY=MO;COUNT=2", nil, 0, []string{"2026-01-07", "2026-01-12"}},
		{"P30", "20250131T170000", "FREQ=MONTHLY;BYSETPOS=-1;BYDAY=FR", nil, 4, []string{"2025-01-31T17:00", "2025-02-28T17:00", "2025-03-28T17:00", "2025-04-25T17:00"}},
	}
	// P02: 49 days from 01-05.
	var p02 []string
	for d := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC); len(p02) < 49; d = d.AddDate(0, 0, 1) {
		p02 = append(p02, d.Format("2006-01-02"))
	}
	cases = append(cases, struct {
		id, dtstart, rule string
		extra             []string
		first             int
		want              []string
	}{"P02", "20260105", "FREQ=DAILY;COUNT=49", nil, 0, p02})
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			dtstart := c.dtstart
			if !strings.Contains(dtstart, "T") {
				dtstart += "T100000"
			}
			lines := append([]string{"DTSTART;TZID=Europe/Paris:" + dtstart, "RRULE:" + c.rule}, c.extra...)
			m, _ := xSeries(t, xEvent(lines...))
			max := 1000
			if c.first > 0 {
				max = c.first
			}
			occ, truncated := xExpand(t, m, nil, "2025-01-01T00:00:00Z", "2126-01-01T00:00:00Z", time.UTC, max)
			if truncated != (c.first > 0) {
				t.Errorf("truncated %v, want %v", truncated, c.first > 0)
			}
			want := make([]string, len(c.want))
			for i, w := range c.want {
				if !strings.Contains(w, "T") {
					w += "T10:00"
				}
				want[i] = w
			}
			sameList(t, "starts", walls(occ, paris), want)
		})
	}
}

// Rules another client stored that Proton's set does not hold are refused,
// not guessed; RDATE and EXRULE too. The two rewrites ParseWrite applies
// (R5, R12) are applied first.
func TestExpandRefusesShapesOutsideProtonsSet(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		code  RefusalCode
	}{
		{"hourly", []string{"RRULE:FREQ=HOURLY;COUNT=3"}, UnsupportedRecurrence},
		{"monthly two ordinals", []string{"RRULE:FREQ=MONTHLY;BYDAY=1MO,3MO"}, UnsupportedRecurrence},
		{"monthly -2", []string{"RRULE:FREQ=MONTHLY;BYDAY=-2MO"}, UnsupportedRecurrence},
		{"yearly byday", []string{"RRULE:FREQ=YEARLY;BYMONTH=1;BYDAY=SU"}, UnsupportedRecurrence},
		{"weekly ordinal", []string{"RRULE:FREQ=WEEKLY;BYDAY=1MO"}, UnsupportedRecurrence},
		{"monthday list", []string{"RRULE:FREQ=MONTHLY;BYMONTHDAY=15,30"}, UnsupportedRecurrence},
		{"two rules", []string{"RRULE:FREQ=DAILY", "RRULE:FREQ=WEEKLY"}, UnsupportedRecurrence},
		{"rdate", []string{"RRULE:FREQ=DAILY;COUNT=2", "RDATE;TZID=Europe/Paris:20260110T100000"}, RDate},
		{"exrule", []string{"RRULE:FREQ=DAILY;COUNT=2", "EXRULE:FREQ=WEEKLY"}, ExRule},
		{"malformed", []string{"RRULE:FREQ=DAILY;COUNT"}, BadData},
		{"unknown zone", []string{"RRULE:FREQ=DAILY;COUNT=2", "EXDATE;TZID=Mars/Olympus:20260106T100000"}, UnknownTZID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := xSeries(t, xEvent(append([]string{"DTSTART;TZID=Europe/Paris:20260105T100000"}, c.lines...)...))
			_, _, err := Expand(m, nil, xInstant(t, "2026-01-01T00:00:00Z"), xInstant(t, "2026-02-01T00:00:00Z"), time.UTC, 100)
			var r *Refusal
			if !errors.As(err, &r) || r.Code != c.code {
				t.Fatalf("err = %v, want a %s refusal", err, c.code)
			}
		})
	}
	// An override with RANGE.
	m, ov := xSeries(t, xEvent("DTSTART:20260105T100000Z", "RRULE:FREQ=DAILY;COUNT=5"),
		xEvent("RECURRENCE-ID;RANGE=THISANDFUTURE:20260106T100000Z", "DTSTART:20260106T120000Z"))
	_, _, err := Expand(m, ov, xInstant(t, "2026-01-01T00:00:00Z"), xInstant(t, "2026-02-01T00:00:00Z"), time.UTC, 100)
	if r := (*Refusal)(nil); !errors.As(err, &r) || r.Code != Range {
		t.Errorf("RANGE override: err = %v, want a %s refusal", err, Range)
	}
	// R12: DAILY with plain BYDAY is the WEEKLY rule it means.
	m, _ = xSeries(t, xEvent("DTSTART;TZID=Europe/Paris:20260105T100000", "RRULE:FREQ=DAILY;BYDAY=MO,WE;COUNT=3"))
	occ, _ := xExpand(t, m, nil, "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z", time.UTC, 100)
	sameList(t, "R12", walls(occ, xLoc(t, "Europe/Paris")), []string{"2026-01-05T10:00", "2026-01-07T10:00", "2026-01-12T10:00"})
}

// Proton's numeric limits bound writes, not how a stored rule expands; a
// huge MONTHLY interval stops past year 9999 instead of overflowing.
func TestExpandIgnoresWriteLimits(t *testing.T) {
	m, _ := xSeries(t, xEvent("DTSTART:20260105T100000Z", "RRULE:FREQ=DAILY;COUNT=60"))
	if occ, _ := xExpand(t, m, nil, "2026-01-01T00:00:00Z", "2027-01-01T00:00:00Z", time.UTC, 1000); len(occ) != 60 {
		t.Errorf("COUNT=60: got %d occurrences, want 60", len(occ))
	}
	for _, rule := range []string{"FREQ=MONTHLY;INTERVAL=999999999;COUNT=3", "FREQ=YEARLY;BYMONTH=2;BYMONTHDAY=30;COUNT=3", "FREQ=MONTHLY;INTERVAL=999999999"} {
		m, _ := xSeries(t, xEvent("DTSTART:20260105T100000Z", "RRULE:"+rule))
		occ, _ := xExpand(t, m, nil, "2026-01-01T00:00:00Z", "9999-12-31T00:00:00Z", time.UTC, 1000)
		sameList(t, rule, starts(occ), []string{"2026-01-05T10:00:00Z"})
	}
}

// A DURATION is nominal in days (the wall clock is kept across a DST change)
// and exact in hours; a DTEND is an exact duration.
func TestExpandDurations(t *testing.T) {
	m, _ := xSeries(t, xEvent("DTSTART;TZID=Europe/Paris:20260328T100000", "DURATION:P1DT1H", "RRULE:FREQ=DAILY;COUNT=1"))
	occ, _ := xExpand(t, m, nil, "2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z", time.UTC, 10)
	if len(occ) != 1 || occ[0].End.UTC().Format(time.RFC3339) != "2026-03-29T09:00:00Z" {
		t.Errorf("P1DT1H from 03-28 10:00 CET: got %v, want end 2026-03-29T09:00:00Z (11:00 CEST)", occ)
	}
	m, _ = xSeries(t, xEvent("DTSTART;TZID=Europe/Paris:20260328T100000", "DTEND;TZID=Europe/Paris:20260329T110000", "RRULE:FREQ=DAILY;COUNT=2"))
	occ, _ = xExpand(t, m, nil, "2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z", time.UTC, 10)
	if len(occ) != 2 || occ[1].End.Sub(occ[1].Start) != 24*time.Hour {
		t.Errorf("DTEND 24 exact hours: got %v", occ)
	}
}
