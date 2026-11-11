package ical

import (
	"bytes"
	"strings"
	"testing"
)

func vevent(t *testing.T, lines ...string) Component {
	t.Helper()
	comps, err := Parse([]byte("BEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	return comps[0]
}

func TestObjectOrderAndStripping(t *testing.T) {
	master := vevent(t, "UID:u", "DTSTART:20261101T100000Z", "RRULE:FREQ=DAILY", "EXDATE:20261103T100000Z")
	e3 := vevent(t, "UID:u", "RECURRENCE-ID:20261105T100000Z", "SUMMARY:third", "RRULE:FREQ=DAILY", "EXDATE:20261106T100000Z", "RDATE:20261107T100000Z")
	e1 := vevent(t, "UID:u", "RECURRENCE-ID:20261102T100000Z", "SUMMARY:first", "EXDATE;TZID=Europe/Paris:20261106T100000")
	e2a := vevent(t, "UID:u", "RECURRENCE-ID:20261104T100000Z", "SUMMARY:tie-a")
	e2b := vevent(t, "UID:u", "RECURRENCE-ID:20261104T100000Z", "SUMMARY:tie-b")
	in := []Component{e3, e2a, e1, e2b}

	out := string(Object(&master, in))
	order := []string{"SUMMARY:first", "SUMMARY:tie-a", "SUMMARY:tie-b", "SUMMARY:third"}
	last := strings.Index(out, "RRULE:FREQ=DAILY")
	if last < 0 || strings.Index(out, "RECURRENCE-ID") < last {
		t.Fatalf("master must come first:\n%s", out)
	}
	for _, s := range order {
		i := strings.Index(out, s)
		if i < last {
			t.Fatalf("%s out of order:\n%s", s, out)
		}
		last = i
	}
	if strings.Count(out, "RRULE") != 1 || strings.Count(out, "EXDATE") != 1 {
		t.Fatalf("exceptions must not carry RRULE/EXDATE; master keeps its own:\n%s", out)
	}
	if !strings.Contains(out, "RDATE:20261107T100000Z") {
		t.Fatal("only RRULE and EXDATE are stripped")
	}
	// Inputs untouched.
	if len(values(in[0], "RRULE")) != 1 || len(values(in[0], "EXDATE")) != 1 || in[0].Name != "VEVENT" || len(in) != 4 || values(in[0], "SUMMARY")[0] != ":third" {
		t.Fatal("Object mutated its inputs")
	}
	if len(values(master, "RRULE")) != 1 {
		t.Fatal("master mutated")
	}
}

func TestObjectSortsByInstantAcrossZones(t *testing.T) {
	// 10:00 Paris in December is 09:00Z, earlier than 09:30Z, although the
	// raw strings sort the other way round.
	first := vevent(t, "UID:u", "RECURRENCE-ID;TZID=Europe/Paris:20261201T100000", "SUMMARY:first")
	second := vevent(t, "UID:u", "RECURRENCE-ID:20261201T093000Z", "SUMMARY:second")
	out := string(Object(nil, []Component{second, first}))
	if strings.Index(out, "SUMMARY:first") > strings.Index(out, "SUMMARY:second") {
		t.Fatalf("expected chronological order:\n%s", out)
	}
}

func TestObjectOrphansWithoutMaster(t *testing.T) {
	e := vevent(t, "UID:u", "RECURRENCE-ID:20261102T100000Z", "RRULE:FREQ=DAILY")
	out := string(Object(nil, []Component{e}))
	if !strings.Contains(out, "RECURRENCE-ID:20261102T100000Z") || strings.Contains(out, "RRULE") {
		t.Fatalf("%s", out)
	}
}

func TestObjectShapeAndCRLF(t *testing.T) {
	master := vevent(t, "UID:u", "SUMMARY:"+strings.Repeat("é", 80))
	out := Object(&master, nil)
	if !bytes.HasSuffix(out, []byte("END:VEVENT\r\nEND:VCALENDAR\r\n")) {
		t.Fatalf("must end with CRLF: %q", out[len(out)-30:])
	}
	if !strings.HasPrefix(string(out), "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//pcb//proton-calendar-bridge//EN\r\nCALSCALE:GREGORIAN\r\nBEGIN:VEVENT\r\n") {
		t.Fatalf("header: %q", out[:120])
	}
	if bytes.Contains(bytes.ReplaceAll(out, []byte("\r\n"), nil), []byte("\n")) || bytes.Contains(out, []byte("VTIMEZONE")) {
		t.Fatal("bare LF or VTIMEZONE")
	}
	for _, l := range bytes.Split(bytes.TrimSuffix(out, []byte("\r\n")), []byte("\r\n")) {
		if len(l) > 75 {
			t.Fatalf("line of %d octets", len(l))
		}
	}
	if _, err := Parse(out); err != nil {
		t.Fatal(err)
	}
}
