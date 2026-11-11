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
	in := []Exception{{e3, 500}, {e2a, 400}, {e1, 200}, {e2b, 400}}

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
	if len(values(in[0].Comp, "RRULE")) != 1 || len(values(in[0].Comp, "EXDATE")) != 1 || len(in) != 4 || values(in[0].Comp, "SUMMARY")[0] != ":third" {
		t.Fatal("Object mutated its inputs")
	}
	if len(values(master, "RRULE")) != 1 {
		t.Fatal("master mutated")
	}
}

func TestObjectSortsByCallerKeyNotByContent(t *testing.T) {
	// The RECURRENCE-ID text says the opposite order: the key decides.
	a := vevent(t, "UID:u", "RECURRENCE-ID:20260101T000000Z", "SUMMARY:key-high")
	b := vevent(t, "UID:u", "RECURRENCE-ID:20300101T000000Z", "SUMMARY:key-low")
	out := string(Object(nil, []Exception{{a, 1000}, {b, -5}}))
	if strings.Index(out, "SUMMARY:key-low") > strings.Index(out, "SUMMARY:key-high") {
		t.Fatalf("expected key order:\n%s", out)
	}
}

func TestObjectEqualKeysKeepCallerOrder(t *testing.T) {
	var in []Exception
	for _, n := range []string{"z", "a", "m", "b"} {
		in = append(in, Exception{vevent(t, "UID:u", "SUMMARY:"+n), 7})
	}
	out := string(Object(nil, in))
	last := -1
	for _, n := range []string{"z", "a", "m", "b"} {
		i := strings.Index(out, "SUMMARY:"+n)
		if i < last {
			t.Fatalf("row order lost for equal keys:\n%s", out)
		}
		last = i
	}
}

func TestObjectOrphansWithoutMaster(t *testing.T) {
	e := vevent(t, "UID:u", "RECURRENCE-ID:20261102T100000Z", "RRULE:FREQ=DAILY")
	out := string(Object(nil, []Exception{{e, 1}}))
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
