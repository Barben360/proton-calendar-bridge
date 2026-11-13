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
		{"a DEL in a value", wcal(timed("X-A:a\x7fb")), BadData},
		{"invalid UTF-8", wcal(timed("X-A:caf\xe9")), BadData},
		{"a DATE RECURRENCE-ID on a timed series", wcal(timed("RRULE:FREQ=DAILY"),
			wev(slices.Concat(head, []string{"RECURRENCE-ID;VALUE=DATE:20261106", "DTSTART;VALUE=DATE:20261106"})...)), BadData},
		{"a timed RECURRENCE-ID on an all-day series", wcal(wev(slices.Concat(head, []string{"DTSTART;VALUE=DATE:20261105", "RRULE:FREQ=DAILY"})...),
			exception("20261106T100000")), BadData},
		{"a faked master that is not Thunderbird's", wcal(wev(slices.Concat(head, []string{"DTSTART:20261106T090000Z", "RDATE:20261106T090000Z", "X-MOZ-FAKED-MASTER:0"})...)), RDate},
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
		{"FREQ=MONTHLY;BYDAY=5TH", "", false},  // M4-R5: ordinals 1..4 and -1 only
		{"FREQ=MONTHLY;BYDAY=-2TH", "", false}, // M4-R5
		{"FREQ=MONTHLY;BYDAY=0TH", "", false},
		{"FREQ=MONTHLY;BYDAY=1TH,2FR", "", false},        // M4-R5: one day only
		{"FREQ=MONTHLY;BYDAY=1TH,FR", "", false},         // M4-R5: one day only
		{"FREQ=MONTHLY;BYDAY=1TH;BYSETPOS=1", "", false}, // an ordinal with BYSETPOS is not rewritten
		{"FREQ=WEEKLY;BYDAY=1TH", "", false},             // M4-R10: WEEKLY takes plain days only
		{"FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;WKST=MO", "", true},
		{"FREQ=WEEKLY;BYSETPOS=1;BYDAY=MO", "", false},
		{"FREQ=DAILY;WKST=SU;INTERVAL=2", "", true},
		{"FREQ=DAILY;WKST=XX", "", false},
		{"FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR", "", true}, // M4-R12: written as WEEKLY
		{"FREQ=DAILY;INTERVAL=1;BYDAY=MO", "", true},  // M4-R12
		{"FREQ=DAILY;INTERVAL=2;BYDAY=MO", "", false}, // M4-R12: not the same meaning as WEEKLY
		{"FREQ=DAILY;BYDAY=1MO", "", false},           // M4-R12: plain days only
		{"FREQ=DAILY;BYDAY=MO;BYHOUR=3", "", false},
		{"FREQ=DAILY;BYHOUR=3,4", "", false},
		{"FREQ=WEEKLY;BYHOUR=3", "", false},
		{"FREQ=DAILY;BYMINUTE=5", "", false},
		{"FREQ=DAILY;BYSECOND=5", "", false},
		{"FREQ=DAILY;BYMONTH=3", "", false},
		{"FREQ=YEARLY;BYYEARDAY=100", "", false},
		{"FREQ=YEARLY;BYWEEKNO=10", "", false},
		{"FREQ=YEARLY;BYMONTH=3;BYDAY=2TU", "", false},
		{"FREQ=YEARLY;BYDAY=MO", "", false},
		{"FREQ=YEARLY;BYMONTH=3", "", false},
		{"FREQ=YEARLY;BYMONTH=3,4;BYMONTHDAY=5", "", false},
		{"FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=5,6", "", false},
		{"FREQ=YEARLY;BYMONTH=13;BYMONTHDAY=5", "", false},
		{"FREQ=YEARLY;BYMONTH=3;BYMONTHDAY=32", "", false},
		{"FREQ=YEARLY;BYMONTH=12;BYMONTHDAY=-1", "", true},
		{"FREQ=MONTHLY;BYMONTHDAY=0", "", false},
		{"FREQ=MONTHLY;BYMONTHDAY=-31", "", true},
		{"FREQ=MONTHLY;BYDAY=MO;BYSETPOS=1;WKST=MO", "", true},
		{"FREQ=MONTHLY;BYDAY=MO;BYSETPOS=1;BYHOUR=3", "", false},
		{"FREQ=MONTHLY;X-NAME=1", "", false},
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

// Ruling M4-R5: Thunderbird's MONTHLY BYDAY=<n><DAY> (n in 1..4 or -1, one
// day) is written in Proton's form BYDAY=<DAY>;BYSETPOS=<n>, in place.
func TestMonthlyOrdinalBydayIsRewritten(t *testing.T) {
	cases := []struct{ rule, want string }{
		{"FREQ=MONTHLY;BYDAY=2TU", "FREQ=MONTHLY;BYDAY=TU;BYSETPOS=2"},
		{"FREQ=MONTHLY;BYDAY=1MO;COUNT=10", "FREQ=MONTHLY;BYDAY=MO;BYSETPOS=1;COUNT=10"},
		{"FREQ=MONTHLY;BYDAY=4SU", "FREQ=MONTHLY;BYDAY=SU;BYSETPOS=4"},
		{"FREQ=MONTHLY;BYDAY=-1FR;INTERVAL=2", "FREQ=MONTHLY;BYDAY=FR;BYSETPOS=-1;INTERVAL=2"},
		{"FREQ=MONTHLY;BYDAY=+3we", "FREQ=MONTHLY;BYDAY=WE;BYSETPOS=3"},
		{"freq=monthly;byday=2tu", "freq=monthly;BYDAY=TU;BYSETPOS=2"},
		// Already Proton's form, or not MONTHLY: as sent.
		{"FREQ=MONTHLY;BYDAY=TU;BYSETPOS=2", "FREQ=MONTHLY;BYDAY=TU;BYSETPOS=2"},
	}
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			in := mustParseWrite(t, wcal(timed("RRULE:"+c.rule)))
			if got := values(in.Master.Comp, "RRULE"); len(got) != 1 || got[0] != ":"+c.want {
				t.Fatalf("RRULE = %v, want %q", got, c.want)
			}
			if rewritten := c.want != c.rule; slices.Contains(in.Notes, noteMonthly) != rewritten {
				t.Fatalf("notes %q, rewritten %v", in.Notes, rewritten)
			}
		})
	}
}

// Ruling M4-R11: Thunderbird's faked master of an orphan exception
// (E08 §6.4) is dropped; the body is then exceptions only.
func TestThunderbirdFakedMasterIsDropped(t *testing.T) {
	fake := wev(slices.Concat(head, []string{"DTSTART;TZID=Europe/Paris:20261106T100000",
		"RDATE;TZID=Europe/Paris:20261106T100000", "X-MOZ-FAKED-MASTER:1"})...)
	in := mustParseWrite(t, wcal(fake, exception("20261106T100000", "SUMMARY2:moved")))
	if in.Master != nil {
		t.Fatalf("the faked master was kept as a master")
	}
	if got := keys(in.Exceptions); !slices.Equal(got, []int64{1793955600}) {
		t.Fatalf("exceptions %v", got)
	}
	if !slices.Contains(in.Notes, noteFaked) {
		t.Errorf("notes %q lack %q", in.Notes, noteFaked)
	}
	if r := refusalOf(t, wcal(fake)); r.Code != BadData {
		t.Fatalf("a faked master alone: code %q, want %q (no VEVENT left)", r.Code, BadData)
	}
}

// Ruling M4-R12: DAILY with plain BYDAY days and INTERVAL absent or 1 (the
// form Thunderbird is believed to write for "every weekday") is written
// as the WEEKLY rule it means, in place; COUNT and UNTIL stay as sent.
func TestDailyBydayIsRewritten(t *testing.T) {
	cases := []struct{ rule, want string }{
		{"FREQ=DAILY;BYDAY=MO,TU,WE,TH,FR", "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"},
		{"FREQ=DAILY;INTERVAL=1;BYDAY=MO,WE;COUNT=10", "FREQ=WEEKLY;INTERVAL=1;BYDAY=MO,WE;COUNT=10"},
		{"BYDAY=SA,SU;FREQ=DAILY;UNTIL=20271231T225959Z", "BYDAY=SA,SU;FREQ=WEEKLY;UNTIL=20271231T225959Z"},
		{"freq=daily;byday=mo", "freq=WEEKLY;byday=mo"},
		{"FREQ=DAILY;COUNT=3", "FREQ=DAILY;COUNT=3"},
		{"FREQ=WEEKLY;BYDAY=MO", "FREQ=WEEKLY;BYDAY=MO"},
	}
	for _, c := range cases {
		t.Run(c.rule, func(t *testing.T) {
			in := mustParseWrite(t, wcal(timed("RRULE:"+c.rule)))
			if got := values(in.Master.Comp, "RRULE"); len(got) != 1 || got[0] != ":"+c.want {
				t.Fatalf("RRULE = %v, want %q", got, c.want)
			}
			if rewritten := c.want != c.rule; slices.Contains(in.Notes, noteDaily) != rewritten {
				t.Fatalf("notes %q, rewritten %v", in.Notes, rewritten)
			}
		})
	}
}

// The body's UID is its TEXT value, unescaped (what Proton stores, SPEC
// §5.3); the VEVENT keeps the line as written, and VEVENTs whose UIDs are
// the same value in other spellings are one event.
func TestParseWriteUnescapesTheUID(t *testing.T) {
	t.Parallel()
	esc := func(uid string, extra ...string) string {
		return wev(slices.Concat([]string{"UID:" + uid, "DTSTAMP:20261101T080000Z"}, extra)...)
	}
	in := mustParseWrite(t, wcal(esc(`pcb-m4-a\,b`, "DTSTART;TZID=Europe/Paris:20261105T100000", "RRULE:FREQ=DAILY"),
		esc("pcb-m4-a,b", "RECURRENCE-ID;TZID=Europe/Paris:20261106T100000", "DTSTART;TZID=Europe/Paris:20261106T100000")))
	if in.UID != "pcb-m4-a,b" {
		t.Errorf("UID %q, want the value pcb-m4-a,b", in.UID)
	}
	if p, _ := in.Master.Comp.Get("UID"); p.Value != `pcb-m4-a\,b` {
		t.Errorf("the master's UID line holds %q, want it as written", p.Value)
	}
	if len(in.Exceptions) != 1 {
		t.Errorf("%d exceptions, want the one of the same UID value", len(in.Exceptions))
	}
	// A series as Thunderbird sends it: every VEVENT spells the UID alike.
	series := mustParseWrite(t, wcal(esc(`s\,t`, "DTSTART:20261105T100000Z", "RRULE:FREQ=DAILY"),
		esc(`s\,t`, "RECURRENCE-ID:20261106T100000Z", "DTSTART:20261106T100000Z")))
	if series.UID != "s,t" || series.Master == nil || len(series.Exceptions) != 1 {
		t.Errorf("a series with an escaped UID: UID %q, master %v, %d exceptions", series.UID, series.Master != nil, len(series.Exceptions))
	}
	if _, err := ParseWrite(wcal(esc(`a\,b`, "DTSTART:20261105T100000Z"), esc(`a\;b`, "RECURRENCE-ID:20261106T100000Z", "DTSTART:20261106T100000Z"))); err == nil {
		t.Error("VEVENTs of two UID values parsed as one event")
	}
}

// A UID is refused (bad-data) when its text holds an escape RFC 5545 does
// not define (pcb would have to guess what Proton stores) or its value holds
// a control character, a newline included.
func TestParseWriteRefusesAUIDItCannotName(t *testing.T) {
	t.Parallel()
	for _, uid := range []string{`a\nb`, `a\Nb`, `a\xb`, `a\:b`, `trailing\`, `a\\\x`} {
		body := wcal(wev("UID:"+uid, "DTSTAMP:20261101T080000Z", "DTSTART:20261105T100000Z"))
		r := refusalOf(t, body)
		if r.Code != BadData || !strings.Contains(r.Reason, "UID") {
			t.Errorf("UID:%s: code %q (%s), want bad-data naming the UID", uid, r.Code, r.Reason)
		}
	}
	for _, uid := range []string{`a\\x`, `a\,b`, `a\;b`, "{0f5c-uuid}@thunderbird", "040000008200E00074C5B7101A82E008"} {
		if _, err := ParseWrite(wcal(wev("UID:"+uid, "DTSTAMP:20261101T080000Z", "DTSTART:20261105T100000Z"))); err != nil {
			t.Errorf("UID:%s refused: %v", uid, err)
		}
	}
}
