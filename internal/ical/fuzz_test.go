package ical

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func FuzzParse(f *testing.F) {
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nSUMMARY:é\r\n x\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"))
	f.Add([]byte("BEGIN:VEVENT\nX;A=\"b:c\":d\n\te\nEND:VEVENT"))
	f.Add(append([]byte("BEGIN:VEVENT\nSUMMARY:x"), bytes.Repeat([]byte("\n a"), 2000)...))
	f.Add([]byte("BEGIN:VEVENT\nA:b\n\n c\nEND:VEVENT"))
	f.Add([]byte("garbage\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		comps, err := Parse(in)
		if err != nil {
			return
		}
		var out []byte
		for _, c := range comps {
			out = append(out, c.Bytes()...)
		}
		again, err := Parse(out)
		if err != nil {
			t.Fatalf("reparse of own output failed: %v", err)
		}
		if !reflect.DeepEqual(comps, again) {
			t.Fatalf("structures differ after round trip")
		}
		var out2 []byte
		for _, c := range again {
			out2 = append(out2, c.Bytes()...)
		}
		if !bytes.Equal(out, out2) {
			t.Fatalf("unstable:\n%q\n%q", out, out2)
		}
	})
}

func FuzzMergeEvent(f *testing.F) {
	f.Add([]byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:a\r\nEXDATE:1\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"), []byte("BEGIN:VEVENT\nSUMMARY:x\nX-PM-SESSION-KEY:k\nEND:VEVENT"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		ev, err := MergeEvent([][]byte{a, b})
		if err != nil {
			return
		}
		ApplyRow(&ev, "#abc", map[string]int{"t": 1})
		if _, err := Parse(ev.Bytes()); err != nil {
			t.Fatalf("merged output does not parse: %v", err)
		}
	})
}

func FuzzParseWrite(f *testing.F) {
	f.Add(wcal(timed("RRULE:FREQ=WEEKLY;BYDAY=TH", "EXDATE;TZID=Europe/Paris:20261112T100000", "ATTENDEE;X-PM-TOKEN=t:mailto:a@x",
		"COLOR:red", "STATUS:TENTATIVE", "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT15M", "END:VALARM"), exception("20261119T100000")))
	f.Add(wcal(wev("UID:u", "DTSTART;TZID=/mozilla.org/20070129_1/Europe/Paris:20261105T100000", "DURATION:P1DT2H", "SUMMARY:"+strings.Repeat("é", 50))))
	f.Add(wcal(wev("UID:u", "DTSTART;VALUE=DATE:20261105", "RRULE:FREQ=MONTHLY;BYDAY=MO;BYSETPOS=-1;UNTIL=20371231")))
	f.Add([]byte("BEGIN:VCALENDAR\nBEGIN:VEVENT\nUID:u\nDTSTART:1\nEND:VEVENT\nEND:VCALENDAR"))
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, body []byte) {
		in, err := ParseWrite(body)
		if err != nil {
			var r *Refusal
			if !errors.As(err, &r) || r.Reason == "" {
				t.Fatalf("not a refusal with a reason: %v", err)
			}
			return
		}
		var events []WriteEvent
		if in.Master != nil {
			events = append(events, *in.Master)
		}
		for _, e := range in.Exceptions {
			events = append(events, e)
		}
		if len(events) == 0 || len(events) > MaxEvents {
			t.Fatalf("%d events accepted", len(events))
		}
		for _, e := range events {
			if e.Comp.firstSub() != len(e.Comp.Items) {
				t.Fatalf("a nested component left in a normalised VEVENT")
			}
			parts, _, err := Patch(nil, e.Comp)
			if err != nil {
				t.Fatalf("Patch on create: %v", err)
			}
			checkFragments(t, parts)
			again, changed, err := Patch(parts, e.Comp)
			if err != nil {
				t.Fatalf("Patch of its own output: %v", err)
			}
			for k := range parts {
				if !bytes.Equal(again[k], parts[k]) || changed[k] {
					t.Fatalf("%s not stable: %q -> %q", k, parts[k], again[k])
				}
			}
			notes, dropped := NotificationsFromAlarms(e.Alarms, e.AllDay)
			if len(notes) > 10 || len(notes)+dropped != len(e.Alarms) {
				t.Fatalf("%d notifications, %d dropped, from %d alarms", len(notes), dropped, len(e.Alarms))
			}
			for _, n := range notes {
				if _, ok := parseDuration(n.Trigger); !ok {
					t.Fatalf("trigger %q is not a duration", n.Trigger)
				}
			}
		}
	})
}

// checkFragments asserts Proton's fragment form: CRLF, no line over 75
// octets, no CRLF after the last line, one VEVENT alone in a VCALENDAR.
func checkFragments(t *testing.T, parts map[PartKind][]byte) {
	t.Helper()
	for k, b := range parts {
		s := string(b)
		if !strings.HasPrefix(s, "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n") || !strings.HasSuffix(s, "\r\nEND:VEVENT\r\nEND:VCALENDAR") {
			t.Fatalf("%s: not one VEVENT alone in a VCALENDAR: %q", k, s)
		}
		for l := range strings.SplitSeq(s, "\r\n") {
			if len(l) > 75 || l == "" || strings.ContainsAny(l, "\r\n") {
				t.Fatalf("%s: bad line %q", k, l)
			}
		}
		if _, err := Parse(b); err != nil {
			t.Fatalf("%s does not parse: %v", k, err)
		}
	}
}

// genEvent builds a VEVENT from fuzz bytes: each pool entry in or out (a
// variant of it for the single-valued ones), then shuffled. One spelling
// per variant and per attendee address, so a line identical by meaning is
// identical in bytes.
func genEvent(data []byte) Component {
	next := func() int {
		if len(data) == 0 {
			return 0
		}
		b := data[0]
		data = data[1:]
		return int(b)
	}
	long := strings.Repeat("abcdefghij", 12)
	singles := [][]string{
		{"DTSTAMP:20261101T080000Z", "DTSTAMP:20261111T111111Z"},
		{"DTSTART;TZID=Europe/Paris:20261105T100000", "DTSTART:20261105T100000Z", "DTSTART;VALUE=DATE:20261105"},
		{"DTEND;TZID=Europe/Paris:20261105T110000", "DTEND:20261105T120000Z"},
		{"RRULE:FREQ=WEEKLY", "RRULE:FREQ=DAILY;COUNT=5"},
		{"SEQUENCE:0", "SEQUENCE:3"},
		{"ORGANIZER;CN=Org:mailto:o@x", "ORGANIZER:mailto:p@x"},
		{"SUMMARY:short", "SUMMARY:" + long, "SUMMARY:" + strings.Repeat("é", 50)},
		{"DESCRIPTION:" + long + long, "DESCRIPTION:d"},
		{"LOCATION:here", "LOCATION:there"},
		{"STATUS:CONFIRMED", "STATUS:CANCELLED"},
		{"TRANSP:OPAQUE", "TRANSP:TRANSPARENT"},
		{"CLASS:PUBLIC"},
		{"X-PM-CONFERENCE-ID:1", "X-PM-CONFERENCE-ID:2"},
	}
	multis := []string{
		"EXDATE;TZID=Europe/Paris:20261112T100000", "EXDATE;TZID=Europe/Paris:20261119T100000", "EXDATE:20261126T090000Z",
		"COMMENT:one", "COMMENT:two", "CATEGORIES:A", "CATEGORIES:B",
		"ATTENDEE;CN=Ann:mailto:ann@x", "ATTENDEE:mailto:bob@x", "ATTENDEE;ROLE=CHAIR:mailto:cy@x",
		"X-MOZ-SNOOZE-TIME:20261105T090000Z", "X-FOO;A=1;B=2:" + long,
	}
	lines := []string{"UID:u"}
	for _, variants := range singles {
		if pick := next() % (len(variants) + 1); pick > 0 {
			lines = append(lines, variants[pick-1])
		}
	}
	for _, l := range multis {
		if next()%2 == 1 {
			lines = append(lines, l)
		}
	}
	for i := len(lines) - 1; i > 0; i-- {
		j := next() % (i + 1)
		lines[i], lines[j] = lines[j], lines[i]
	}
	c := Component{Name: "VEVENT"}
	for _, l := range lines {
		p, _ := splitLine(l)
		c.add(p)
	}
	return c
}

// mergedLines is the multiset of lines MergeEvent makes of parts.
func mergedLines(t *testing.T, parts map[PartKind][]byte) []string {
	t.Helper()
	var ordered [][]byte
	for _, k := range partKinds {
		if b, ok := parts[k]; ok {
			ordered = append(ordered, b)
		}
	}
	ev, err := MergeEvent(ordered)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	return sortedLines(ev)
}

func sortedLines(c Component) []string {
	var out []string
	for _, p := range c.Props() {
		out = append(out, p.Line())
	}
	slices.Sort(out)
	return out
}

// FuzzPatchRoundTrip: Patch(Route(x)) is x again under MergeEvent; patching
// it with y gives y; patching again with the same event changes nothing.
func FuzzPatchRoundTrip(f *testing.F) {
	f.Add([]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 0, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 0, 1, 0, 1, 0, 1, 0, 1})
	f.Add([]byte("a fairly random seed of bytes for two events, x then y"))
	f.Fuzz(func(t *testing.T, data []byte) {
		x, y := genEvent(data[:len(data)/2]), genEvent(data[len(data)/2:])
		created, _, err := Patch(nil, x)
		if err != nil {
			t.Fatal(err)
		}
		checkFragments(t, created)
		if got, want := mergedLines(t, created), sortedLines(x); !slices.Equal(got, want) {
			t.Fatalf("created event merges to\n%q\nwant\n%q", got, want)
		}
		same, changed, err := Patch(created, x)
		if err != nil {
			t.Fatal(err)
		}
		for k := range created {
			if !bytes.Equal(same[k], created[k]) || changed[k] {
				t.Fatalf("%s changed by an identical event", k)
			}
		}
		updated, _, err := Patch(created, y)
		if err != nil {
			t.Fatal(err)
		}
		checkFragments(t, updated)
		if got, want := mergedLines(t, updated), sortedLines(y); !slices.Equal(got, want) {
			t.Fatalf("x patched with y merges to\n%q\nwant\n%q", got, want)
		}
		again, changed, err := Patch(updated, y)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(updated) {
			t.Fatalf("parts %d then %d", len(updated), len(again))
		}
		for k := range updated {
			if !bytes.Equal(again[k], updated[k]) || changed[k] {
				t.Fatalf("%s not stable", k)
			}
		}
	})
}
