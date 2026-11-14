package ical

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var editNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

// editFixture reads testdata/edit/name with CRLF line ends, as Bytes writes.
func editFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "edit", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), []byte("\n"), []byte("\r\n"))
}

// paris is a wall clock (YYYYMMDDTHHMMSS) in Europe/Paris.
func paris(t *testing.T, wall string) Time {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.ParseInLocation(localLayout, wall, loc)
	if err != nil {
		t.Fatal(err)
	}
	return Time{At: at, TZID: "Europe/Paris"}
}

func decoded(t *testing.T, vcal []byte) EventData {
	t.Helper()
	d, err := DecodeEvent(vcal)
	if err != nil {
		t.Fatalf("DecodeEvent: %v\n%s", err, vcal)
	}
	return d
}

// veventProps are the properties of the n-th VEVENT of vcal.
func veventProps(t *testing.T, vcal []byte, n int) []Property {
	t.Helper()
	comps, err := Parse(vcal)
	if err != nil {
		t.Fatal(err)
	}
	evs := comps[0].Subs("VEVENT")
	if n >= len(evs) {
		t.Fatalf("VEVENT %d of %d", n, len(evs))
	}
	return evs[n].Props()
}

func propValue(props []Property, name string) (string, bool) {
	for _, p := range props {
		if strings.EqualFold(p.Name, name) {
			return p.Value, true
		}
	}
	return "", false
}

func sameInstant(a, b Time) bool {
	return a.Date == b.Date && a.TZID == b.TZID && a.At.Equal(b.At)
}

// refusalReason fails unless err is a *Refusal with code, and returns its Reason.
func refusalReason(t *testing.T, err error, code RefusalCode) string {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *Refusal %s", err, code)
	}
	if r.Code != code {
		t.Fatalf("code = %s (%s), want %s", r.Code, r.Reason, code)
	}
	return r.Reason
}

// The edit cases whose outputs TestApplyEditResultPassesParseWrite checks.
// Each test below runs its case and checks what it is about.
type editCase struct {
	name string
	run  func(t *testing.T) []byte
}

func editCases() []editCase {
	return []editCase{
		{"new-event-minimal", func(t *testing.T) []byte {
			out, err := NewEvent("new-1@pcb", Fields{
				Summary: new("Dentist"),
				Start:   new(paris(t, "20261120T090000")),
				End:     new(paris(t, "20261120T100000")),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"new-event-full", func(t *testing.T) []byte {
			out, err := NewEvent("new-2@pcb", Fields{
				Summary:     new("Yoga; then lunch, maybe"),
				Description: new("Bring the mat\nand water"),
				Location:    new("Studio 2"),
				Start:       &Time{Date: "2026-11-14"}, // the second Saturday
				End:         &Time{Date: "2026-11-15"},
				RRule:       new("FREQ=MONTHLY;BYDAY=2SA"),
				Alarms:      &[]int{-540, 1440},
				Busy:        new(false),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"keep-untouched", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "keep.before.ics"), ScopeSeries, nil, Fields{Summary: new("Planning (Q1, room 5)")}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"clear-location", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "series.ics"), ScopeSeries, nil, Fields{Location: new("")}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"occurrence-creates-override", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "series.ics"), ScopeOccurrence, new(paris(t, "20261207T100000")), Fields{
				Summary: new("Team sync (offsite)"),
				Start:   new(paris(t, "20261207T113000")),
				End:     new(paris(t, "20261207T123000")),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"occurrence-edits-its-override", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "series.ics"), ScopeOccurrence, new(paris(t, "20261130T100000")), Fields{
				Location: new("Room 9"),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"series-shift", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "series.ics"), ScopeSeries, nil, Fields{
				Start: new(paris(t, "20261116T110000")),
				End:   new(paris(t, "20261116T120000")),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"series-longer", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "series.ics"), ScopeSeries, nil, Fields{
				End: new(paris(t, "20261116T113000")),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"rrule-change-without-exceptions", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "plain-series.ics"), ScopeSeries, nil, Fields{
				RRule: new("FREQ=WEEKLY;BYDAY=MO,WE;UNTIL=20270131T225959Z"),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"alarms-and-busy", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "keep.before.ics"), ScopeSeries, nil, Fields{
				Alarms: &[]int{10, 60},
				Busy:   new(false),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"until-utc-shift", func(t *testing.T) []byte {
			out, err := ApplyEdit(untilSeries("UNTIL=20261207T090000Z"), ScopeSeries, nil, Fields{
				Start: new(paris(t, "20261116T110000")),
				End:   new(paris(t, "20261116T120000")),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"until-date-shift", func(t *testing.T) []byte {
			out, err := ApplyEdit(untilSeries("UNTIL=20261207"), ScopeSeries, nil, Fields{
				Start: new(paris(t, "20261117T100000")),
				End:   new(paris(t, "20261117T110000")),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"shift-with-its-rule", func(t *testing.T) []byte {
			out, err := ApplyEdit(editFixture(t, "plain-series.ics"), ScopeSeries, nil, Fields{
				Start: new(paris(t, "20261117T100000")),
				End:   new(paris(t, "20261117T110000")),
				RRule: new("FREQ=WEEKLY;BYDAY=TU;COUNT=10"),
			}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"delete-adds-exdate", func(t *testing.T) []byte {
			out, err := DeleteOccurrence(editFixture(t, "series.ics"), paris(t, "20261207T100000"), editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"delete-drops-override", func(t *testing.T) []byte {
			out, err := DeleteOccurrence(editFixture(t, "series.ics"), paris(t, "20261130T100000"), editNow)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
	}
}

func runEditCase(t *testing.T, name string) []byte {
	t.Helper()
	for _, c := range editCases() {
		if c.name == name {
			return c.run(t)
		}
	}
	t.Fatalf("no edit case %q", name)
	return nil
}

func TestNewEventMinimal(t *testing.T) {
	out := runEditCase(t, "new-event-minimal")
	d := decoded(t, out)
	if d.UID != "new-1@pcb" || d.Summary != "Dentist" {
		t.Errorf("UID, SUMMARY = %q, %q", d.UID, d.Summary)
	}
	if !sameInstant(d.Start, paris(t, "20261120T090000")) || !sameInstant(d.End, paris(t, "20261120T100000")) {
		t.Errorf("start, end = %+v, %+v", d.Start, d.End)
	}
	if d.RRule != "" || len(d.Alarms) != 0 || d.Location != "" || d.Transp != "" || d.Unknown != 0 {
		t.Errorf("unasked content: %+v", d)
	}
	props := veventProps(t, out, 0)
	if v, _ := propValue(props, "DTSTAMP"); v != "20261113T120000Z" {
		t.Errorf("DTSTAMP = %q", v)
	}
	if v, ok := propValue(props, "SEQUENCE"); v != "0" || !ok {
		t.Errorf("SEQUENCE = %q, %v", v, ok)
	}
}

func TestNewEventFull(t *testing.T) {
	d := decoded(t, runEditCase(t, "new-event-full"))
	if d.Summary != "Yoga; then lunch, maybe" || d.Description != "Bring the mat\nand water" || d.Location != "Studio 2" {
		t.Errorf("text = %q, %q, %q", d.Summary, d.Description, d.Location)
	}
	if d.Start.Date != "2026-11-14" || d.End.Date != "2026-11-15" {
		t.Errorf("dates = %+v, %+v", d.Start, d.End)
	}
	// Proton's form of the monthly ordinal, as ParseWrite would write it.
	if d.RRule != "FREQ=MONTHLY;BYDAY=SA;BYSETPOS=2" {
		t.Errorf("RRULE = %q", d.RRule)
	}
	want := []Alarm{{MinutesBefore: -540, Action: "DISPLAY"}, {MinutesBefore: 1440, Action: "DISPLAY"}}
	if !slices.Equal(d.Alarms, want) {
		t.Errorf("alarms = %+v", d.Alarms)
	}
	if d.Transp != "TRANSPARENT" {
		t.Errorf("TRANSP = %q", d.Transp)
	}
}

func TestNewEventRefusesWhatProtonCannotStore(t *testing.T) {
	start, end := paris(t, "20261120T090000"), paris(t, "20261120T100000")
	cases := []struct {
		name string
		uid  string
		f    Fields
		code RefusalCode
	}{
		{"no summary", "u", Fields{Start: &start, End: &end}, BadData},
		{"empty summary", "u", Fields{Summary: new(""), Start: &start, End: &end}, BadData},
		{"no start", "u", Fields{Summary: new("x"), End: &end}, BadData},
		{"no end", "u", Fields{Summary: new("x"), Start: &start}, BadData},
		{"no uid", "", Fields{Summary: new("x"), Start: &start, End: &end}, BadData},
		{"end before start", "u", Fields{Summary: new("x"), Start: &end, End: &start}, BadData},
		{"date and time", "u", Fields{Summary: new("x"), Start: &Time{Date: "2026-11-20"}, End: &end}, BadData},
		{"floating", "u", Fields{Summary: new("x"), Start: &Time{At: start.At, Floating: true}, End: &end}, Floating},
		{"unknown zone", "u", Fields{Summary: new("x"), Start: &Time{At: start.At, TZID: "Mars/Olympus"}, End: &end}, UnknownTZID},
		{"hourly", "u", Fields{Summary: new("x"), Start: &start, End: &end, RRule: new("FREQ=HOURLY")}, UnsupportedRecurrence},
		{"control character", "u", Fields{Summary: new("a\x07b"), Start: &start, End: &end}, BadData},
		{"alarm after a timed start", "u", Fields{Summary: new("x"), Start: &start, End: &end, Alarms: &[]int{-5}}, BadData},
		{"eleven alarms", "u", Fields{Summary: new("x"), Start: &start, End: &end, Alarms: &[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}, BadData},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := NewEvent(c.uid, c.f, editNow)
			if out != nil {
				t.Errorf("output with a refusal: %q", out)
			}
			refusalReason(t, err, c.code)
		})
	}
}

func TestApplyEditKeepsUntouchedLines(t *testing.T) {
	got := runEditCase(t, "keep-untouched")
	want := editFixture(t, "keep.after.ics")
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from keep.after.ics:\n%s", got)
	}
}

func TestApplyEditClearsLocation(t *testing.T) {
	out := runEditCase(t, "clear-location")
	if _, ok := propValue(veventProps(t, out, 0), "LOCATION"); ok {
		t.Error("the master still has a LOCATION")
	}
	before, after := decoded(t, editFixture(t, "series.ics")), decoded(t, out)
	if before.Location == "" {
		t.Fatal("the fixture has no LOCATION to clear")
	}
	before.Location = ""
	if before.Summary != after.Summary || before.RRule != after.RRule || len(after.Overrides) != 1 ||
		!slices.EqualFunc(before.ExDates, after.ExDates, sameInstant) || !slices.Equal(before.Alarms, after.Alarms) {
		t.Errorf("more than LOCATION changed: %+v", after)
	}
}

func TestApplyEditOccurrenceCreatesOverride(t *testing.T) {
	out := runEditCase(t, "occurrence-creates-override")
	d := decoded(t, out)
	before := decoded(t, editFixture(t, "series.ics"))
	if d.Summary != before.Summary || !sameInstant(d.Start, before.Start) || d.RRule != before.RRule {
		t.Errorf("the master changed: %+v", d)
	}
	if v, _ := propValue(veventProps(t, out, 0), "DTSTAMP"); v != "20261101T090000Z" {
		t.Errorf("master DTSTAMP = %q, want it untouched", v)
	}
	if len(d.Overrides) != 2 {
		t.Fatalf("%d overrides, want 2 (the stored one and the new one)", len(d.Overrides))
	}
	if d.Overrides[0].Summary != "Team sync (moved)" {
		t.Errorf("the stored override changed: %+v", d.Overrides[0])
	}
	o := d.Overrides[1]
	if o.RecurrenceID == nil || !sameInstant(*o.RecurrenceID, paris(t, "20261207T100000")) {
		t.Errorf("RECURRENCE-ID = %+v", o.RecurrenceID)
	}
	if o.Summary != "Team sync (offsite)" || o.Location != "Room 4" || o.UID != "series-1@proton.me" {
		t.Errorf("override text = %q, %q, %q", o.Summary, o.Location, o.UID)
	}
	if !sameInstant(o.Start, paris(t, "20261207T113000")) || !sameInstant(o.End, paris(t, "20261207T123000")) {
		t.Errorf("override times = %+v, %+v", o.Start, o.End)
	}
	if o.RRule != "" || len(o.ExDates) != 0 {
		t.Errorf("the override recurs: %q, %v", o.RRule, o.ExDates)
	}
	if !slices.Equal(o.Alarms, before.Alarms) {
		t.Errorf("override alarms = %+v, want the master's %+v", o.Alarms, before.Alarms)
	}
	props := veventProps(t, out, 2)
	if v, _ := propValue(props, "DTSTAMP"); v != "20261113T120000Z" {
		t.Errorf("override DTSTAMP = %q", v)
	}
	if v, _ := propValue(props, "SEQUENCE"); v != "2" {
		t.Errorf("override SEQUENCE = %q, want the master's 1 bumped", v)
	}
}

func TestApplyEditOccurrenceWithoutEndKeepsTheDuration(t *testing.T) {
	out, err := ApplyEdit(editFixture(t, "series.ics"), ScopeOccurrence, new(paris(t, "20261214T100000")), Fields{Busy: new(false)}, editNow)
	if err != nil {
		t.Fatal(err)
	}
	o := decoded(t, out).Overrides[1]
	if !sameInstant(o.Start, paris(t, "20261214T100000")) || !sameInstant(o.End, paris(t, "20261214T110000")) || o.Transp != "TRANSPARENT" {
		t.Errorf("override = %+v", o)
	}
}

func TestApplyEditOccurrenceEditsItsOverride(t *testing.T) {
	out := runEditCase(t, "occurrence-edits-its-override")
	d := decoded(t, out)
	if len(d.Overrides) != 1 {
		t.Fatalf("%d overrides, want the stored one edited", len(d.Overrides))
	}
	o := d.Overrides[0]
	if o.Location != "Room 9" || o.Summary != "Team sync (moved)" || !sameInstant(o.Start, paris(t, "20261130T140000")) {
		t.Errorf("override = %+v", o)
	}
	if v, _ := propValue(veventProps(t, out, 1), "SEQUENCE"); v != "2" {
		t.Errorf("override SEQUENCE = %q", v)
	}
	if d.Location != "Room 4" {
		t.Errorf("master LOCATION = %q", d.Location)
	}
}

func TestApplyEditOccurrenceRefusals(t *testing.T) {
	cases := []struct {
		name string
		file string
		rid  *Time
		f    Fields
		code RefusalCode
	}{
		{"no recurrence id", "series.ics", nil, Fields{Summary: new("x")}, BadData},
		{"an rrule", "series.ics", new(paris(t, "20261207T100000")), Fields{RRule: new("FREQ=DAILY")}, UnsupportedRecurrence},
		{"a deleted occurrence", "series.ics", new(paris(t, "20261123T100000")), Fields{Summary: new("x")}, BadData},
		{"a date for a timed series", "series.ics", &Time{Date: "2026-12-07"}, Fields{Summary: new("x")}, BadData},
		{"a day off the rule", "series.ics", new(paris(t, "20261208T100000")), Fields{Summary: new("x")}, BadData},
		{"a time off the rule", "series.ics", new(paris(t, "20261207T103000")), Fields{Summary: new("x")}, BadData},
		{"after COUNT", "series.ics", new(paris(t, "20270125T100000")), Fields{Summary: new("x")}, BadData},
		{"before the series", "series.ics", new(paris(t, "20261109T100000")), Fields{Summary: new("x")}, BadData},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ApplyEdit(editFixture(t, c.file), ScopeOccurrence, c.rid, c.f, editNow)
			refusalReason(t, err, c.code)
		})
	}
}

func TestApplyEditSeriesShiftMovesExceptions(t *testing.T) {
	out := runEditCase(t, "series-shift")
	d := decoded(t, out)
	if !sameInstant(d.Start, paris(t, "20261116T110000")) || !sameInstant(d.End, paris(t, "20261116T120000")) {
		t.Errorf("master = %+v, %+v", d.Start, d.End)
	}
	if len(d.ExDates) != 1 || !sameInstant(d.ExDates[0], paris(t, "20261123T110000")) {
		t.Errorf("EXDATE = %+v, want 2026-11-23 11:00 Paris", d.ExDates)
	}
	if len(d.Overrides) != 1 {
		t.Fatalf("%d overrides", len(d.Overrides))
	}
	o := d.Overrides[0]
	if o.RecurrenceID == nil || !sameInstant(*o.RecurrenceID, paris(t, "20261130T110000")) {
		t.Errorf("RECURRENCE-ID = %+v, want 2026-11-30 11:00 Paris", o.RecurrenceID)
	}
	if !sameInstant(o.Start, paris(t, "20261130T140000")) || o.Summary != "Team sync (moved)" {
		t.Errorf("the override's own times or text moved: %+v", o)
	}
}

// A shift is a wall-clock shift: a Saturday 10:00 Paris series moved to
// Sunday 10:00 across the end of summer time (2026-10-25) is 24 h of wall
// clock but 25 h of instant. Its EXDATE (Saturday 2026-10-31 10:00 Paris,
// written in UTC) moves to Sunday 2026-11-01 10:00 Paris, where the series'
// occurrence now is, and stays written in UTC.
func TestApplyEditSeriesShiftAcrossDST(t *testing.T) {
	vcal := calendar(
		"BEGIN:VEVENT", "UID:dst@pcb", "DTSTAMP:20261001T090000Z",
		"DTSTART;TZID=Europe/Paris:20261024T100000", "DTEND;TZID=Europe/Paris:20261024T110000",
		"RRULE:FREQ=WEEKLY;COUNT=10", "EXDATE:20261031T090000Z", "SUMMARY:x", "END:VEVENT")
	out, err := ApplyEdit(vcal, ScopeSeries, nil, Fields{
		Start: new(paris(t, "20261025T100000")), End: new(paris(t, "20261025T110000")),
	}, editNow)
	if err != nil {
		t.Fatal(err)
	}
	d := decoded(t, out)
	if len(d.ExDates) != 1 || !d.ExDates[0].At.Equal(paris(t, "20261101T100000").At) {
		t.Fatalf("EXDATE = %+v, want 2026-11-01 10:00 Paris", d.ExDates)
	}
	if v, _ := propValue(veventProps(t, out, 0), "EXDATE"); v != "20261101T090000Z" {
		t.Errorf("EXDATE value = %q, want it kept in UTC", v)
	}
}

func TestApplyEditSeriesLongerKeepsExceptions(t *testing.T) {
	d := decoded(t, runEditCase(t, "series-longer"))
	if !sameInstant(d.End, paris(t, "20261116T113000")) {
		t.Errorf("end = %+v", d.End)
	}
	if !sameInstant(d.ExDates[0], paris(t, "20261123T100000")) || !sameInstant(*d.Overrides[0].RecurrenceID, paris(t, "20261130T100000")) {
		t.Errorf("exceptions moved: %+v, %+v", d.ExDates, d.Overrides[0].RecurrenceID)
	}
}

func TestApplyEditRRuleChangeWithExceptionsIsRefused(t *testing.T) {
	cases := []struct {
		name string
		f    Fields
	}{
		{"rrule change", Fields{RRule: new("FREQ=WEEKLY;BYDAY=TU;COUNT=10")}},
		{"rrule removed", Fields{RRule: new("")}},
		{"start and end shifted differently", Fields{Start: new(paris(t, "20261116T110000")), End: new(paris(t, "20261116T130000"))}},
		{"start moved alone", Fields{Start: new(paris(t, "20261116T103000"))}},
		{"zone changed", Fields{
			Start: &Time{At: paris(t, "20261116T100000").At},
			End:   &Time{At: paris(t, "20261116T110000").At},
		}},
		{"made all-day", Fields{Start: &Time{Date: "2026-11-16"}, End: &Time{Date: "2026-11-17"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := editFixture(t, "series.ics")
			keep := bytes.Clone(in)
			out, err := ApplyEdit(in, ScopeSeries, nil, c.f, editNow)
			reason := refusalReason(t, err, SeriesHasExceptions)
			if !strings.Contains(reason, "2") {
				t.Errorf("reason %q does not name the 2 exceptions", reason)
			}
			if out != nil {
				t.Errorf("output with a refusal")
			}
			if !bytes.Equal(in, keep) {
				t.Error("the input was modified")
			}
		})
	}
}

func TestApplyEditSameRRuleWithExceptionsIsAccepted(t *testing.T) {
	out, err := ApplyEdit(editFixture(t, "series.ics"), ScopeSeries, nil, Fields{RRule: new("FREQ=WEEKLY;COUNT=10;BYDAY=MO;INTERVAL=1")}, editNow)
	if err != nil {
		t.Fatalf("the same rule, written in another order: %v", err)
	}
	if d := decoded(t, out); len(d.ExDates) != 1 || len(d.Overrides) != 1 {
		t.Errorf("exceptions lost: %+v", d)
	}
}

func TestApplyEditRRuleChangeWithoutExceptions(t *testing.T) {
	out := runEditCase(t, "rrule-change-without-exceptions")
	d := decoded(t, out)
	if d.RRule != "FREQ=WEEKLY;BYDAY=MO,WE;UNTIL=20270131T225959Z" {
		t.Errorf("RRULE = %q", d.RRule)
	}
	props := veventProps(t, out, 0)
	if v, _ := propValue(props, "SEQUENCE"); v != "2" {
		t.Errorf("SEQUENCE = %q", v)
	}
	if v, _ := propValue(props, "DTSTAMP"); v != "20261113T120000Z" {
		t.Errorf("DTSTAMP = %q", v)
	}

	out, err := ApplyEdit(editFixture(t, "plain-series.ics"), ScopeSeries, nil, Fields{RRule: new("")}, editNow)
	if err != nil {
		t.Fatal(err)
	}
	if d := decoded(t, out); d.RRule != "" {
		t.Errorf("RRULE not removed: %q", d.RRule)
	}
}

func TestApplyEditReplacesRelativeAlarmsOnly(t *testing.T) {
	out := runEditCase(t, "alarms-and-busy")
	d := decoded(t, out)
	want := []Alarm{{MinutesBefore: 10, Action: "DISPLAY"}, {MinutesBefore: 60, Action: "DISPLAY"}}
	if !slices.Equal(d.Alarms, want) {
		t.Errorf("alarms = %+v", d.Alarms)
	}
	comps, _ := Parse(out)
	master := comps[0].Subs("VEVENT")[0]
	var absolute int
	for _, a := range master.Subs("VALARM") {
		if tr, _ := a.Get("TRIGGER"); tr.Value == "20261116T080000Z" {
			absolute++
		}
	}
	if absolute != 1 {
		t.Errorf("%d absolute alarms, want the stored one kept", absolute)
	}
	if d.Transp != "TRANSPARENT" {
		t.Errorf("TRANSP = %q", d.Transp)
	}
}

func TestApplyEditRefusals(t *testing.T) {
	cases := []struct {
		name  string
		scope Scope
		f     Fields
		code  RefusalCode
	}{
		{"end before the stored start", ScopeSeries, Fields{End: new(paris(t, "20261116T090000"))}, BadData},
		{"unknown scope", Scope("following"), Fields{Summary: new("x")}, BadData},
		{"floating start", ScopeSeries, Fields{Start: &Time{At: paris(t, "20261116T100000").At, Floating: true}}, Floating},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ApplyEdit(editFixture(t, "plain-series.ics"), c.scope, nil, c.f, editNow)
			refusalReason(t, err, c.code)
		})
	}
}

func TestDeleteOccurrenceAddsExdate(t *testing.T) {
	out := runEditCase(t, "delete-adds-exdate")
	d := decoded(t, out)
	want := []Time{paris(t, "20261123T100000"), paris(t, "20261207T100000")}
	if !slices.EqualFunc(d.ExDates, want, sameInstant) {
		t.Errorf("EXDATE = %+v", d.ExDates)
	}
	if len(d.Overrides) != 1 {
		t.Errorf("%d overrides, want the stored one kept", len(d.Overrides))
	}
	props := veventProps(t, out, 0)
	if v, _ := propValue(props, "SEQUENCE"); v != "2" {
		t.Errorf("SEQUENCE = %q", v)
	}
	if v, _ := propValue(props, "DTSTAMP"); v != "20261113T120000Z" {
		t.Errorf("DTSTAMP = %q", v)
	}
}

func TestDeleteOccurrenceDropsItsOverride(t *testing.T) {
	d := decoded(t, runEditCase(t, "delete-drops-override"))
	if len(d.Overrides) != 0 {
		t.Errorf("%d overrides, want none", len(d.Overrides))
	}
	want := []Time{paris(t, "20261123T100000"), paris(t, "20261130T100000")}
	if !slices.EqualFunc(d.ExDates, want, sameInstant) {
		t.Errorf("EXDATE = %+v", d.ExDates)
	}
}

func TestDeleteOccurrenceRefusals(t *testing.T) {
	single := calendar("BEGIN:VEVENT", "UID:one@pcb", "DTSTAMP:20261101T090000Z",
		"DTSTART;TZID=Europe/Paris:20261116T100000", "DTEND;TZID=Europe/Paris:20261116T110000", "END:VEVENT")
	cases := []struct {
		name string
		vcal []byte
		rid  Time
	}{
		{"not recurring", single, paris(t, "20261116T100000")},
		{"already deleted", editFixture(t, "series.ics"), paris(t, "20261123T100000")},
		{"a date for a timed series", editFixture(t, "series.ics"), Time{Date: "2026-12-07"}},
		{"a day off the rule", editFixture(t, "series.ics"), paris(t, "20261208T100000")},
		{"a time off the rule", editFixture(t, "series.ics"), paris(t, "20261207T103000")},
		{"after COUNT", editFixture(t, "series.ics"), paris(t, "20270125T100000")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := DeleteOccurrence(c.vcal, c.rid, editNow)
			refusalReason(t, err, BadData)
		})
	}
}

func TestApplyEditResultPassesParseWrite(t *testing.T) {
	for _, c := range editCases() {
		t.Run(c.name, func(t *testing.T) {
			out := c.run(t)
			in, err := ParseWrite(out)
			if err != nil {
				t.Fatalf("ParseWrite: %v\n%s", err, out)
			}
			if in.Master == nil {
				t.Error("no master")
			}
		})
	}
}

// untilSeries is a weekly Paris series from Monday 2026-11-16 10:00 to the
// UNTIL part given: four occurrences, the last on 2026-12-07. A DATE UNTIL
// comes without BYDAY (the start's weekday), so a +1 day shift stays on
// the rule.
func untilSeries(until string) []byte {
	byday := ";BYDAY=MO"
	if len(until) == len("UNTIL=20261207") {
		byday = ""
	}
	return calendar("BEGIN:VEVENT", "UID:until@pcb", "DTSTAMP:20261101T090000Z",
		"DTSTART;TZID=Europe/Paris:20261116T100000", "DTEND;TZID=Europe/Paris:20261116T110000",
		"RRULE:FREQ=WEEKLY"+byday+";"+until, "SEQUENCE:0", "SUMMARY:x", "END:VEVENT")
}

// occurrenceCount is how many occurrences vcal's series has in 2026-2027.
func occurrenceCount(t *testing.T, vcal []byte) int {
	t.Helper()
	cal, err := parseStored(vcal)
	if err != nil {
		t.Fatal(err)
	}
	occ, _, err := Expand(*storedMaster(cal), nil, utc("2026-01-01T00:00:00Z"), utc("2028-01-01T00:00:00Z"), time.UTC, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return len(occ)
}

func TestApplyEditShiftMovesUntil(t *testing.T) {
	cases := []struct {
		name, before, until string
	}{
		{"until-utc-shift", "UNTIL=20261207T090000Z", "UNTIL=20261207T100000Z"},
		{"until-date-shift", "UNTIL=20261207", "UNTIL=20261208"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if n := occurrenceCount(t, untilSeries(c.before)); n != 4 {
				t.Fatalf("the fixture has %d occurrences, want 4", n)
			}
			out := runEditCase(t, c.name)
			d := decoded(t, out)
			if !strings.HasSuffix(d.RRule, ";"+c.until) {
				t.Errorf("RRULE = %q, want it to end with %s", d.RRule, c.until)
			}
			if n := occurrenceCount(t, out); n != 4 {
				t.Errorf("%d occurrences after the shift, want the 4 kept", n)
			}
		})
	}
}

// A client that sends the whole view back echoes the stored RRULE with the
// shift: the rule is the same, so its UNTIL still moves.
func TestApplyEditShiftWithTheRuleEchoedMovesUntil(t *testing.T) {
	out, err := ApplyEdit(untilSeries("UNTIL=20261207T090000Z"), ScopeSeries, nil, Fields{
		Start: new(paris(t, "20261116T110000")),
		End:   new(paris(t, "20261116T120000")),
		RRule: new("FREQ=WEEKLY;BYDAY=MO;UNTIL=20261207T090000Z"),
	}, editNow)
	if err != nil {
		t.Fatal(err)
	}
	if d := decoded(t, out); d.RRule != "FREQ=WEEKLY;BYDAY=MO;UNTIL=20261207T100000Z" {
		t.Errorf("RRULE = %q, want UNTIL moved by the shift", d.RRule)
	}
	if n := occurrenceCount(t, out); n != 4 {
		t.Errorf("%d occurrences after the shift, want the 4 kept", n)
	}
}

// A UTC UNTIL on an all-day series (its UTC date is the last day) moves by
// the days the start moved.
func TestApplyEditAllDayShiftMovesUTCUntil(t *testing.T) {
	vcal := calendar("BEGIN:VEVENT", "UID:allday@pcb", "DTSTAMP:20261101T090000Z",
		"DTSTART;VALUE=DATE:20261116", "DTEND;VALUE=DATE:20261117",
		"RRULE:FREQ=WEEKLY;UNTIL=20261207T235959Z", "SUMMARY:x", "END:VEVENT")
	if n := occurrenceCount(t, vcal); n != 4 {
		t.Fatalf("the fixture has %d occurrences, want 4", n)
	}
	out, err := ApplyEdit(vcal, ScopeSeries, nil, Fields{
		Start: &Time{Date: "2026-11-17"}, End: &Time{Date: "2026-11-18"},
	}, editNow)
	if err != nil {
		t.Fatal(err)
	}
	if d := decoded(t, out); d.RRule != "FREQ=WEEKLY;UNTIL=20261208T235959Z" {
		t.Errorf("RRULE = %q", d.RRule)
	}
	if n := occurrenceCount(t, out); n != 4 {
		t.Errorf("%d occurrences after the shift, want the 4 kept", n)
	}
	if _, err := ParseWrite(out); err != nil {
		t.Errorf("ParseWrite: %v", err)
	}
}

func TestApplyEditRuleChangedWithTheStart(t *testing.T) {
	d := decoded(t, runEditCase(t, "shift-with-its-rule"))
	if d.RRule != "FREQ=WEEKLY;BYDAY=TU;COUNT=10" || !sameInstant(d.Start, paris(t, "20261117T100000")) {
		t.Errorf("series = %q from %+v", d.RRule, d.Start)
	}
}

// A shift that changes the date under a rule with BYDAY, BYMONTHDAY or
// BYMONTH leaves the moved exceptions, or the start itself, off the rule:
// refused, the exceptions first.
func TestApplyEditShiftOffTheRuleIsRefused(t *testing.T) {
	monthly := calendar("BEGIN:VEVENT", "UID:m@pcb", "DTSTAMP:20261101T090000Z",
		"DTSTART;TZID=Europe/Paris:20261116T100000", "DTEND;TZID=Europe/Paris:20261116T110000",
		"RRULE:FREQ=MONTHLY;BYMONTHDAY=16;COUNT=6", "EXDATE;TZID=Europe/Paris:20261216T100000", "SUMMARY:x", "END:VEVENT")
	yearly := calendar("BEGIN:VEVENT", "UID:y@pcb", "DTSTAMP:20261101T090000Z",
		"DTSTART;VALUE=DATE:20261116", "DTEND;VALUE=DATE:20261117",
		"RRULE:FREQ=YEARLY;BYMONTH=11;BYMONTHDAY=16", "SUMMARY:x", "END:VEVENT")
	shiftDay := Fields{Start: new(paris(t, "20261117T100000")), End: new(paris(t, "20261117T110000"))}
	cases := []struct {
		name string
		vcal []byte
		f    Fields
		code RefusalCode
	}{
		{"weekly BYDAY with exceptions", editFixture(t, "series.ics"), shiftDay, SeriesHasExceptions},
		{"monthly BYMONTHDAY with an EXDATE", monthly, shiftDay, SeriesHasExceptions},
		{"weekly BYDAY, no exception", editFixture(t, "plain-series.ics"), shiftDay, BadData},
		{"yearly BYMONTH BYMONTHDAY, no exception", yearly,
			Fields{Start: &Time{Date: "2026-11-17"}, End: &Time{Date: "2026-11-18"}}, BadData},
		{"rule changed off the start", editFixture(t, "plain-series.ics"), Fields{RRule: new("FREQ=WEEKLY;BYDAY=WE")}, BadData},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			keep := bytes.Clone(c.vcal)
			out, err := ApplyEdit(c.vcal, ScopeSeries, nil, c.f, editNow)
			refusalReason(t, err, c.code)
			if out != nil || !bytes.Equal(c.vcal, keep) {
				t.Error("output with a refusal, or the input modified")
			}
		})
	}
}

func TestNewEventStartOffItsRuleIsRefused(t *testing.T) {
	_, err := NewEvent("u", Fields{
		Summary: new("x"),
		Start:   new(paris(t, "20261117T100000")), // a Tuesday
		End:     new(paris(t, "20261117T110000")),
		RRule:   new("FREQ=WEEKLY;BYDAY=MO"),
	}, editNow)
	refusalReason(t, err, BadData)
}

func TestOccurrenceAfterUntilIsRefused(t *testing.T) {
	vcal := untilSeries("UNTIL=20261207T090000Z")
	rid := paris(t, "20261214T100000")
	_, err := DeleteOccurrence(vcal, rid, editNow)
	refusalReason(t, err, BadData)
	_, err = ApplyEdit(vcal, ScopeOccurrence, &rid, Fields{Summary: new("x")}, editNow)
	refusalReason(t, err, BadData)
	if _, err := DeleteOccurrence(vcal, paris(t, "20261207T100000"), editNow); err != nil {
		t.Errorf("the last occurrence: %v", err)
	}
}

// Review A finding 2: setting alarms by minutes keeps an existing alarm at
// those minutes as it is (an e-mail alarm keeps its lines); only a new
// minute value gets a DISPLAY alarm. An e-mail alarm is Proton-side (#16,
// Fix 4): it is not refused as one Proton cannot keep.
func TestReplaceAlarmsKeepsTheActionOfAnAlarmSentAgain(t *testing.T) {
	obj := calendar("BEGIN:VEVENT", "UID:a@x", "DTSTAMP:20260101T000000Z", "SUMMARY:Review",
		"DTSTART;TZID=Europe/Paris:20261116T100000", "DTEND;TZID=Europe/Paris:20261116T110000",
		"BEGIN:VALARM", "ACTION:EMAIL", "TRIGGER:-PT30M", "SUMMARY:Review soon", "DESCRIPTION:Mail me", "ATTENDEE:mailto:me@example.com", "END:VALARM",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT60M", "DESCRIPTION:Gone", "END:VALARM",
		"END:VEVENT")
	for _, tc := range []struct {
		name    string
		minutes []int
		want    []Alarm
	}{
		{"sent again unchanged", []int{30}, []Alarm{{30, "EMAIL"}}},
		{"one added", []int{30, 10}, []Alarm{{30, "EMAIL"}, {10, "DISPLAY"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ApplyEdit(obj, ScopeSeries, nil, Fields{Alarms: &tc.minutes}, editNow)
			if err != nil {
				t.Fatal(err)
			}
			d, err := DecodeEvent(out)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(d.Alarms, tc.want) {
				t.Errorf("alarms %+v, want %+v", d.Alarms, tc.want)
			}
			s := string(out)
			for _, line := range []string{"ACTION:EMAIL\r\nTRIGGER:-PT30M\r\nSUMMARY:Review soon\r\nDESCRIPTION:Mail me\r\nATTENDEE:mailto:me@example.com\r\n"} {
				if !strings.Contains(s, line) {
					t.Errorf("the e-mail alarm lost its lines:\n%s", s)
				}
			}
			if strings.Contains(s, "Gone") {
				t.Errorf("the alarm at 60 minutes, not asked for, is still there:\n%s", s)
			}
		})
	}
}
