package ical

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// wev is one VEVENT block with CRLF line ends.
func wev(lines ...string) string {
	return "BEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\n"
}

// wcal wraps VEVENT blocks the way Thunderbird sends them: VERSION, PRODID
// and a VTIMEZONE first.
func wcal(events ...string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Mozilla.org/NONSGML Mozilla Calendar V1.1//EN\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Europe/Paris\r\nBEGIN:STANDARD\r\nTZOFFSETFROM:+0200\r\nTZOFFSETTO:+0100\r\n" +
		"DTSTART:19701025T030000\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		strings.Join(events, "") + "END:VCALENDAR\r\n")
}

// head is a VEVENT's identity lines; secretSummary must never reach an error.
const secretSummary = "SUMMARY:secret-title-xyz"

var head = []string{"UID:u1", "DTSTAMP:20261101T080000Z", secretSummary}

// timed is head plus a one-hour Europe/Paris event, then extra.
func timed(extra ...string) string {
	return wev(slices.Concat(head, []string{"DTSTART;TZID=Europe/Paris:20261105T100000", "DTEND;TZID=Europe/Paris:20261105T110000"}, extra)...)
}

// exception is an exception VEVENT of u1 at the given Paris local time.
func exception(recID string, extra ...string) string {
	return wev(slices.Concat(head, []string{"RECURRENCE-ID;TZID=Europe/Paris:" + recID,
		"DTSTART;TZID=Europe/Paris:" + recID, "SEQUENCE:1"}, extra)...)
}

func mustParseWrite(t *testing.T, body []byte) Incoming {
	t.Helper()
	in, err := ParseWrite(body)
	if err != nil {
		t.Fatalf("ParseWrite: %v", err)
	}
	return in
}

func refusalOf(t *testing.T, body []byte) *Refusal {
	t.Helper()
	_, err := ParseWrite(body)
	var r *Refusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *Refusal", err)
	}
	if r.Reason == "" {
		t.Fatalf("refusal %s without a reason", r.Code)
	}
	if strings.Contains(err.Error(), "secret-title") {
		t.Fatalf("event content in the error: %v", err)
	}
	return r
}

func manyExceptions(n int) []string {
	out := []string{timed("RRULE:FREQ=DAILY")}
	for i := range n {
		day := 6 + i/24 // 2026-11-06 onwards, one exception per hour slot
		out = append(out, exception(fmt.Sprintf("202611%02dT%02d0000", day, i%24)))
	}
	return out
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		code RefusalCode
	}{
		{"two UIDs", wcal(timed(), wev("UID:u2", "DTSTAMP:20261101T080000Z", "DTSTART:20261105T100000Z")), NotOneUID},
		{"101 VEVENTs", wcal(manyExceptions(100)...), TooMany},
		{"floating DTSTART", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105T100000"})...)), Floating},
		{"floating DTEND", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105T100000Z", "DTEND:20261105T110000"})...)), Floating},
		{"floating EXDATE", wcal(timed("RRULE:FREQ=DAILY", "EXDATE:20261106T100000")), Floating},
		{"floating RECURRENCE-ID", wcal(timed("RRULE:FREQ=DAILY"), wev(slices.Concat(head, []string{"RECURRENCE-ID:20261106T100000", "DTSTART:20261106T100000Z"})...)), Floating},
		{"unknown TZID", wcal(wev(slices.Concat(head, []string{"DTSTART;TZID=Mars/Olympus_Mons:20261105T100000"})...)), UnknownTZID},
		{"Windows TZID", wcal(wev(slices.Concat(head, []string{"DTSTART;TZID=W. Europe Standard Time:20261105T100000"})...)), UnknownTZID},
		{"empty TZID", wcal(wev(slices.Concat(head, []string{"DTSTART;TZID=:20261105T100000"})...)), UnknownTZID},
		{"Local TZID", wcal(wev(slices.Concat(head, []string{"DTSTART;TZID=Local:20261105T100000"})...)), UnknownTZID},
		{"unknown TZID on DTEND", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105T100000Z", "DTEND;TZID=Nowhere/Land:20261105T110000"})...)), UnknownTZID},
		{"FREQ=HOURLY", wcal(timed("RRULE:FREQ=HOURLY")), UnsupportedRecurrence},
		{"RANGE=THISANDFUTURE", wcal(timed("RRULE:FREQ=DAILY"), wev(slices.Concat(head, []string{"RECURRENCE-ID;RANGE=THISANDFUTURE;TZID=Europe/Paris:20261106T100000", "DTSTART;TZID=Europe/Paris:20261106T120000"})...)), Range},
		{"RDATE", wcal(timed("RDATE:20261110T100000Z")), RDate},
		{"EXRULE", wcal(timed("EXRULE:FREQ=WEEKLY")), ExRule},
		{"not iCalendar", []byte("hello\r\n"), BadData},
		{"two VCALENDARs", append(wcal(timed()), wcal(timed())...), BadData},
		{"a bare VEVENT", []byte(timed()), BadData},
		{"no VEVENT", wcal(), BadData},
		{"a VTODO", wcal(timed(), "BEGIN:VTODO\r\nUID:u1\r\nEND:VTODO\r\n"), BadData},
		{"a VEVENT child other than VALARM", wcal(timed("BEGIN:X-THING", "A:b", "END:X-THING")), BadData},
		{"no UID", wcal(wev("DTSTAMP:20261101T080000Z", "DTSTART:20261105T100000Z")), BadData},
		{"two UIDs in one VEVENT", wcal(timed("UID:u1")), BadData},
		{"an empty UID", wcal(wev("UID:", "DTSTART:20261105T100000Z")), BadData},
		{"no DTSTART", wcal(wev(head...)), BadData},
		{"two DTSTART", wcal(timed("DTSTART:20261105T100000Z")), BadData},
		{"two SUMMARY", wcal(timed("SUMMARY:other")), BadData},
		{"DTEND before DTSTART", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105T100000Z", "DTEND:20261105T090000Z"})...)), BadData},
		{"DTEND and DURATION", wcal(timed("DURATION:PT1H")), BadData},
		{"DATE DTEND on a timed start", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105T100000Z", "DTEND;VALUE=DATE:20261106"})...)), BadData},
		{"a date without VALUE=DATE", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105"})...)), BadData},
		{"a DATE with a TZID", wcal(wev(slices.Concat(head, []string{"DTSTART;VALUE=DATE;TZID=Europe/Paris:20261105"})...)), BadData},
		{"a UTC time with a TZID", wcal(wev(slices.Concat(head, []string{"DTSTART;TZID=Europe/Paris:20261105T100000Z"})...)), BadData},
		{"an impossible date", wcal(wev(slices.Concat(head, []string{"DTSTART:20261305T100000Z"})...)), BadData},
		{"two masters", wcal(timed(), timed()), BadData},
		{"a duplicate RECURRENCE-ID", wcal(timed("RRULE:FREQ=DAILY"), exception("20261106T100000"), exception("20261106T100000")), BadData},
		{"RRULE on an exception", wcal(timed("RRULE:FREQ=DAILY"), exception("20261106T100000", "RRULE:FREQ=DAILY")), UnsupportedRecurrence},
		{"EXDATE on an exception", wcal(timed("RRULE:FREQ=DAILY"), exception("20261106T100000", "EXDATE:20261107T090000Z")), UnsupportedRecurrence},
		{"a SEQUENCE that is not a number", wcal(timed("SEQUENCE:two")), BadData},
		{"a negative DURATION", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105T100000Z", "DURATION:-PT1H"})...)), BadData},
		{"a timed DURATION on a DATE", wcal(wev(slices.Concat(head, []string{"DTSTART;VALUE=DATE:20261105", "DURATION:PT1H"})...)), BadData},
		{"a bare CR in a value", wcal(timed("X-A:one\rtwo")), BadData},
		{"a NUL in a parameter", wcal(timed("X-A;P=a\x00b:v")), BadData},
		{"a malformed DURATION", wcal(wev(slices.Concat(head, []string{"DTSTART:20261105T100000Z", "DURATION:1H"})...)), BadData},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if r := refusalOf(t, c.body); r.Code != c.code {
				t.Fatalf("code %q (%s), want %q", r.Code, r.Reason, c.code)
			}
		})
	}
}

func TestHundredVEventsAreAccepted(t *testing.T) {
	in := mustParseWrite(t, wcal(manyExceptions(99)...))
	if in.Master == nil || len(in.Exceptions) != 99 {
		t.Fatalf("master %v, %d exceptions", in.Master != nil, len(in.Exceptions))
	}
}

func TestRRuleLimits(t *testing.T) {
	cases := []struct {
		rule  string
		start string // DTSTART line, Paris 10:00 by default
		ok    bool
	}{
		{"FREQ=DAILY", "", true},
		{"FREQ=WEEKLY;BYDAY=MO,WE", "", true},
		{"FREQ=MONTHLY", "", true},
		{"FREQ=YEARLY", "", true},
		{"FREQ=HOURLY", "", false},
		{"FREQ=MINUTELY", "", false},
		{"FREQ=SECONDLY", "", false},
		{"FREQ=DAILY;COUNT=49", "", true},
		{"FREQ=DAILY;COUNT=50", "", false},
		{"FREQ=DAILY;UNTIL=20371231T225959Z", "", true},
		{"FREQ=DAILY;UNTIL=20371231T230000Z", "", false}, // 2038-01-01 00:00 in Paris
		{"FREQ=DAILY;UNTIL=20380101T075959Z", "DTSTART;TZID=America/Los_Angeles:20261105T100000", true},
		{"FREQ=DAILY;UNTIL=20380101T080000Z", "DTSTART;TZID=America/Los_Angeles:20261105T100000", false},
		{"FREQ=DAILY;UNTIL=20371231", "DTSTART;VALUE=DATE:20261105", true},
		{"FREQ=DAILY;UNTIL=20380101", "DTSTART;VALUE=DATE:20261105", false},
		{"FREQ=DAILY;COUNT=3;UNTIL=20271231T000000Z", "", false},
		{"FREQ=MONTHLY;BYMONTHDAY=5", "", true},
		{"FREQ=MONTHLY;BYMONTHDAY=5,6", "", false},
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=1", "", true},
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=4", "", true},
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=-1", "", true},
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=5", "", false},
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=-2", "", false},
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=0", "", false},
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=1,2", "", false},
		{"FREQ=MONTHLY;BYDAY=TH,FR;BYSETPOS=1", "", false},
		{"FREQ=MONTHLY;BYDAY=TH", "", false},
		{"FREQ=MONTHLY;BYDAY=1TH", "", false}, // the constraints' shape is BYDAY+BYSETPOS
		{"FREQ=MONTHLY;BYDAY=TH;BYSETPOS=1;BYMONTHDAY=5", "", false},
		{"FREQ=MONTHLY;BYMONTH=3", "", false},
		{"FREQ=YEARLY;BYMONTHDAY=5", "", false},
		{"FREQ=YEARLY;BYMONTH=11;BYMONTHDAY=5", "", true},
		{"FREQ=DAILY;INTERVAL=999", "", true},
		{"FREQ=DAILY;INTERVAL=1000", "", false},
		{"FREQ=WEEKLY;INTERVAL=4999", "", true},
		{"FREQ=WEEKLY;INTERVAL=5000", "", false},
		{"FREQ=YEARLY;INTERVAL=99", "", true},
		{"FREQ=YEARLY;INTERVAL=100", "", false},
		{"FREQ=MONTHLY;INTERVAL=1000", "", true}, // no MONTHLY bound in the constraints
	}
	for _, c := range cases {
		t.Run(c.rule+" "+c.start, func(t *testing.T) {
			start := c.start
			if start == "" {
				start = "DTSTART;TZID=Europe/Paris:20261105T100000"
			}
			body := wcal(wev(slices.Concat(head, []string{start, "RRULE:" + c.rule})...))
			_, err := ParseWrite(body)
			if c.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if r := refusalOf(t, body); r.Code != UnsupportedRecurrence {
				t.Fatalf("code %q (%s), want %q", r.Code, r.Reason, UnsupportedRecurrence)
			}
		})
	}
	for _, rule := range []string{"COUNT=3", "FREQ=DAILY;COUNT=x", "FREQ=DAILY;FREQ=WEEKLY", "FREQ=DAILY;;", "FREQ=DAILY;INTERVAL=0", "FREQ=DAILY;UNTIL=2027"} {
		t.Run("malformed "+rule, func(t *testing.T) {
			r := refusalOf(t, wcal(timed("RRULE:"+rule)))
			if r.Code != BadData {
				t.Fatalf("code %q (%s), want %q", r.Code, r.Reason, BadData)
			}
		})
	}
}

func TestNormalisationStripsOurAndRenderedLines(t *testing.T) {
	body := []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nCALSCALE:GREGORIAN\r\nMETHOD:PUBLISH\r\nX-WR-CALNAME:c\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Europe/Paris\r\nEND:VTIMEZONE\r\n" +
		timed(
			"COLOR:tomato",
			"X-PCB-INTEGRITY:unverified",
			"X-PCB-PLACEHOLDER:1",
			"X-PCB-ANYTHING:x",
			"X-PM-SESSION-KEY:c2Vzc2lvbg==",
			"ORGANIZER;CN=Org:mailto:org@example.com",
			"ATTENDEE;CN=Ann;X-PM-TOKEN=tok1;PARTSTAT=ACCEPTED:mailto:ann@example.com",
			"X-MOZ-GENERATION:3",
			"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15M", "X-MOZ-LASTACK:20261101T000000Z", "END:VALARM",
			"BEGIN:VALARM", "ACTION:EMAIL", "TRIGGER:-P1D", "END:VALARM",
		) + "END:VCALENDAR\r\n")
	in := mustParseWrite(t, body)
	if in.UID != "u1" || in.Master == nil || len(in.Exceptions) != 0 {
		t.Fatalf("UID %q master %v exceptions %d", in.UID, in.Master != nil, len(in.Exceptions))
	}
	ev := in.Master.Comp
	for _, name := range []string{"COLOR", "X-PCB-INTEGRITY", "X-PCB-PLACEHOLDER", "X-PCB-ANYTHING", "X-PM-SESSION-KEY",
		"VERSION", "PRODID", "CALSCALE", "METHOD", "X-WR-CALNAME", "TZID"} {
		if got := values(ev, name); len(got) != 0 {
			t.Errorf("%s kept: %v", name, got)
		}
	}
	if subs := ev.firstSub(); subs != len(ev.Items) {
		t.Errorf("nested component kept in the VEVENT: %+v", ev.Items[subs])
	}
	if got := values(ev, "ATTENDEE"); len(got) != 1 || got[0] != ";CN=Ann;PARTSTAT=ACCEPTED:mailto:ann@example.com" {
		t.Errorf("ATTENDEE = %v, want X-PM-TOKEN gone and the rest kept", got)
	}
	for name, want := range map[string]string{"X-MOZ-GENERATION": ":3", "ORGANIZER": ";CN=Org:mailto:org@example.com", "SUMMARY": ":secret-title-xyz"} {
		if got := values(ev, name); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %q", name, got, want)
		}
	}
	if len(in.Master.Alarms) != 2 || in.Master.Alarms[0].Name != "VALARM" {
		t.Fatalf("alarms = %+v", in.Master.Alarms)
	}
	if _, ok := in.Master.Alarms[0].Get("X-MOZ-LASTACK"); !ok {
		t.Errorf("the alarm handed over is not the one sent")
	}
	for _, n := range []string{noteColor, noteOurs, noteToken} {
		if !slices.Contains(in.Notes, n) {
			t.Errorf("notes %q lack %q", in.Notes, n)
		}
	}
	if in.Master.AllDay {
		t.Errorf("a timed event read as all-day")
	}
}

func TestTentativeIsDropped(t *testing.T) {
	for _, line := range []string{"STATUS:TENTATIVE", "STATUS:tentative"} {
		in := mustParseWrite(t, wcal(timed(line, "TRANSP:OPAQUE")))
		if got := values(in.Master.Comp, "STATUS"); len(got) != 0 {
			t.Errorf("%s kept: %v", line, got)
		}
		if got := values(in.Master.Comp, "TRANSP"); len(got) != 1 {
			t.Errorf("TRANSP lost: %v", got)
		}
		if !slices.Contains(in.Notes, noteTentative) {
			t.Errorf("notes %q lack %q", in.Notes, noteTentative)
		}
	}
	in := mustParseWrite(t, wcal(timed("STATUS:CONFIRMED")))
	if got := values(in.Master.Comp, "STATUS"); len(got) != 1 || got[0] != ":CONFIRMED" {
		t.Errorf("STATUS:CONFIRMED = %v", got)
	}
	if slices.Contains(in.Notes, noteTentative) {
		t.Errorf("tentative note without a tentative status")
	}
}

func TestMozillaTZIDIsMapped(t *testing.T) {
	const moz = "/mozilla.org/20070129_1/Europe/Paris"
	body := wcal(
		wev(slices.Concat(head, []string{
			"DTSTART;TZID=" + moz + ":20261105T100000",
			"DTEND;TZID=/mozilla.org/20050126_1/America/Argentina/Buenos_Aires:20261105T070000",
			"RRULE:FREQ=DAILY",
			"EXDATE;TZID=" + moz + ":20261106T100000,20261107T100000",
		})...),
		wev(slices.Concat(head, []string{
			"RECURRENCE-ID;TZID=" + moz + ":20261108T100000",
			"DTSTART;TZID=\"Europe/Paris\":20261108T120000",
		})...),
	)
	in := mustParseWrite(t, body)
	m := in.Master.Comp
	for name, want := range map[string]string{
		"DTSTART": ";TZID=Europe/Paris:20261105T100000",
		"DTEND":   ";TZID=America/Argentina/Buenos_Aires:20261105T070000",
		"EXDATE":  ";TZID=Europe/Paris:20261106T100000,20261107T100000",
	} {
		if got := values(m, name); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %q", name, got, want)
		}
	}
	// 2026-11-08 10:00 in Paris (UTC+1) is 09:00 UTC.
	const recID = 1794128400
	ex, ok := in.Exceptions[recID]
	if !ok {
		t.Fatalf("exceptions keyed %v, want %d", keys(in.Exceptions), recID)
	}
	if got := values(ex.Comp, "RECURRENCE-ID"); len(got) != 1 || got[0] != ";TZID=Europe/Paris:20261108T100000" {
		t.Errorf("RECURRENCE-ID = %v", got)
	}
	if got := values(ex.Comp, "DTSTART"); len(got) != 1 || got[0] != ";TZID=\"Europe/Paris\":20261108T120000" {
		t.Errorf("an IANA TZID was rewritten: %v", got)
	}
	if !slices.Contains(in.Notes, noteMozilla) {
		t.Errorf("notes %q lack %q", in.Notes, noteMozilla)
	}
	r := refusalOf(t, wcal(wev(slices.Concat(head, []string{"DTSTART;TZID=/mozilla.org/20070129_1/Nowhere/Land:20261105T100000"})...)))
	if r.Code != UnknownTZID {
		t.Fatalf("unknown Mozilla zone: code %q", r.Code)
	}
}

func keys[V any](m map[int64]V) []int64 {
	var out []int64
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestParseWriteShape(t *testing.T) {
	body := wcal(
		exception("20261107T100000", "SUMMARY2:x"),
		timed("RRULE:FREQ=DAILY"),
		exception("20261106T100000"),
	)
	in := mustParseWrite(t, body)
	if in.UID != "u1" || in.Master == nil {
		t.Fatalf("UID %q, master %v", in.UID, in.Master != nil)
	}
	// Paris is UTC+1 in November: 10:00 local is 09:00 UTC.
	want := []int64{1793955600, 1794042000}
	if got := keys(in.Exceptions); !slices.Equal(got, want) {
		t.Fatalf("exception keys %v, want %v", got, want)
	}
	if _, ok := in.Master.Comp.Get("RECURRENCE-ID"); ok {
		t.Errorf("the master is an exception")
	}
	orphans := mustParseWrite(t, wcal(exception("20261106T100000")))
	if orphans.Master != nil || len(orphans.Exceptions) != 1 {
		t.Fatalf("exceptions alone: master %v, %d exceptions", orphans.Master != nil, len(orphans.Exceptions))
	}
	allDay := mustParseWrite(t, wcal(wev(slices.Concat(head, []string{"DTSTART;VALUE=DATE:20261105", "DTEND;VALUE=DATE:20261106"})...)))
	if !allDay.Master.AllDay {
		t.Errorf("an all-day event read as timed")
	}
	allDayEx := mustParseWrite(t, wcal(wev(slices.Concat(head, []string{"RECURRENCE-ID;VALUE=DATE:20261106", "DTSTART;VALUE=DATE:20261106"})...)))
	if got := keys(allDayEx.Exceptions); !slices.Equal(got, []int64{1793923200}) { // midnight UTC
		t.Errorf("all-day RECURRENCE-ID keyed %v", got)
	}
}

func TestDurationBecomesDTEnd(t *testing.T) {
	cases := []struct{ start, dur, want string }{
		{"DTSTART;TZID=Europe/Paris:20261105T100000", "PT1H30M", ";TZID=Europe/Paris:20261105T113000"},
		// Paris leaves summer time on 2026-10-25: a day is nominal, 24 hours are exact.
		{"DTSTART;TZID=Europe/Paris:20261024T100000", "P1D", ";TZID=Europe/Paris:20261025T100000"},
		{"DTSTART;TZID=Europe/Paris:20261024T100000", "PT24H", ";TZID=Europe/Paris:20261025T090000"},
		{"DTSTART;TZID=/mozilla.org/20070129_1/Europe/Paris:20261105T100000", "P1W", ";TZID=Europe/Paris:20261112T100000"},
		{"DTSTART;VALUE=DATE:20261105", "P2D", ";VALUE=DATE:20261107"},
		{"DTSTART:20261105T100000Z", "PT45M", ":20261105T104500Z"},
		{"DTSTART:20261105T100000Z", "+P1DT2H3M4S", ":20261106T120304Z"},
	}
	for _, c := range cases {
		t.Run(c.start+" "+c.dur, func(t *testing.T) {
			in := mustParseWrite(t, wcal(wev("UID:u1", c.start, "DURATION:"+c.dur, "SUMMARY:after")))
			ev := in.Master.Comp
			if got := values(ev, "DTEND"); len(got) != 1 || got[0] != c.want {
				t.Fatalf("DTEND = %v, want %q", got, c.want)
			}
			if got := values(ev, "DURATION"); len(got) != 0 {
				t.Fatalf("DURATION kept: %v", got)
			}
			var names []string
			for _, p := range ev.Props() {
				names = append(names, p.Name)
			}
			if !slices.Equal(names, []string{"UID", "DTSTART", "DTEND", "SUMMARY"}) {
				t.Fatalf("DTEND not where DURATION was: %v", names)
			}
			if !slices.Contains(in.Notes, noteDuration) {
				t.Errorf("notes %q lack %q", in.Notes, noteDuration)
			}
		})
	}
}
