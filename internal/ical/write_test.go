package ical

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

// frag is a fragment in Proton's canonical form (constraints: CRLF, no
// VERSION/PRODID, no CRLF after the last line). Lines are taken as given, so
// a test can pass an oddly folded one.
func frag(lines ...string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n" + strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\nEND:VCALENDAR")
}

// comp parses one VEVENT from content lines.
func comp(t *testing.T, lines ...string) Component {
	t.Helper()
	comps, err := Parse([]byte(wev(lines...)))
	if err != nil || len(comps) != 1 {
		t.Fatalf("test VEVENT: %v", err)
	}
	return comps[0]
}

func names(props []Property) []string {
	out := make([]string, len(props))
	for i, p := range props {
		out[i] = p.Name
	}
	return out
}

const (
	uidLine   = "UID:u1"
	stampLine = "DTSTAMP:20261101T080000Z"
	exdate1   = "EXDATE;TZID=Europe/Paris:20261112T100000"
	annStored = "ATTENDEE;CN=Ann;X-PM-TOKEN=tok1;PARTSTAT=NEEDS-ACTION:mailto:ann@example.com"
)

// oddDescription is a DESCRIPTION folded at 40 octets, as no serialiser of
// ours would fold it: kept, its bytes prove the line was not rewritten.
var oddDescription = "DESCRIPTION:A long description that Prot\r\n on folded at forty octets, not at seve\r\n nty-five."

// stored is a canonical event: every part, an oddly folded line, a weird
// parameter order, two EXDATE copies, two COMMENTs and an attendee token.
func stored() map[PartKind][]byte {
	return map[PartKind][]byte{
		SharedSigned: frag(uidLine, stampLine, "DTSTART;TZID=Europe/Paris:20261105T100000", "DTEND;TZID=Europe/Paris:20261105T110000",
			"RRULE:FREQ=WEEKLY", exdate1, "SEQUENCE:2", "ORGANIZER;CN=Org:mailto:org@example.com"),
		SharedEncrypted: frag(uidLine, stampLine, "CREATED:20261101T070000Z", "SUMMARY:Title", oddDescription, "LOCATION:Room 1",
			"X-MOZ-GENERATION:3", `X-FOO;B="two";A=1:v`),
		CalendarSigned:    frag(uidLine, stampLine, exdate1, "STATUS:CONFIRMED", "TRANSP:OPAQUE"),
		CalendarEncrypted: frag(uidLine, stampLine, "COMMENT:note one", "COMMENT:note two"),
		Attendees:         frag(uidLine, stampLine, annStored),
	}
}

// served is what Thunderbird sends back for parts: the event as our render
// serves it (merged, row colour, PARTSTAT from the token, the alarms from
// Notifications), through ParseWrite, as the write path will see it.
func served(t *testing.T, parts map[PartKind][]byte) Component {
	t.Helper()
	var ordered [][]byte
	for _, k := range partKinds {
		if b, ok := parts[k]; ok {
			ordered = append(ordered, b)
		}
	}
	ev := mustMerge(t, ordered...)
	ApplyRow(&ev, "#3CBB82", map[string]int{"tok1": 3})
	if err := ApplyNotifications(&ev, json.RawMessage(`[{"Type":1,"Trigger":"-PT15M"}]`), nil, false); err != nil {
		t.Fatal(err)
	}
	MarkIntegrity(&ev, false)
	in := mustParseWrite(t, Object(&ev, nil))
	return in.Master.Comp
}

// set replaces the first property called name with line, or appends line.
func set(t *testing.T, c Component, name, line string) Component {
	t.Helper()
	p, ok := splitLine(line)
	if !ok {
		t.Fatalf("bad line %q", line)
	}
	c = c.Clone()
	for i, it := range c.Items {
		if it.Comp == nil && strings.EqualFold(it.Prop.Name, name) {
			c.Items[i].Prop = p
			return c
		}
	}
	c.addProp(p)
	return c
}

// without drops every property for which drop is true.
func without(c Component, drop func(Property) bool) Component {
	c = c.Clone()
	c.removeProps(drop)
	return c
}

func named(name string) func(Property) bool {
	return func(p Property) bool { return strings.EqualFold(p.Name, name) }
}

func mustPatch(t *testing.T, current map[PartKind][]byte, in Component) (map[PartKind][]byte, map[PartKind]bool) {
	t.Helper()
	out, changed, err := Patch(current, in)
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	return out, changed
}

// expectParts compares every part's bytes and changed flag.
func expectParts(t *testing.T, got map[PartKind][]byte, changed map[PartKind]bool, want map[PartKind][]byte, wantChanged ...PartKind) {
	t.Helper()
	if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want))) {
		t.Fatalf("parts %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	for k, w := range want {
		if !bytes.Equal(got[k], w) {
			t.Errorf("%s:\n got %q\nwant %q", k, got[k], w)
		}
	}
	for _, k := range partKinds {
		if changed[k] != slices.Contains(wantChanged, k) {
			t.Errorf("changed[%s] = %v, want %v", k, changed[k], !changed[k])
		}
	}
}

func TestRoutingTable(t *testing.T) {
	ev := comp(t, uidLine, stampLine, "DTSTART;TZID=Europe/Paris:20261105T100000", "DTEND;TZID=Europe/Paris:20261105T110000",
		"RECURRENCE-ID;TZID=Europe/Paris:20261105T100000", "RRULE:FREQ=DAILY", exdate1, "SEQUENCE:1",
		"ORGANIZER:mailto:o@x", "X-PM-CONFERENCE-ID:42", "CREATED:20261101T000000Z", "SUMMARY:s", "DESCRIPTION:d",
		"LOCATION:l", "X-MOZ-GENERATION:1", "CATEGORIES:c", "CLASS:PUBLIC", "URL:https://x", "STATUS:CONFIRMED",
		"TRANSP:OPAQUE", "COMMENT:c", "ATTENDEE:mailto:a@x", "LAST-MODIFIED:20261101T000000Z",
		"COLOR:red", "DURATION:PT1H", "X-PCB-INTEGRITY:unverified", "X-PCB-PLACEHOLDER:1", "X-PM-SESSION-KEY:k",
		"VERSION:2.0", "PRODID:x", "CALSCALE:GREGORIAN", "METHOD:PUBLISH",
		"BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "END:VALARM")
	want := map[PartKind][]string{
		SharedSigned:      {"UID", "DTSTAMP", "DTSTART", "DTEND", "RECURRENCE-ID", "RRULE", "EXDATE", "SEQUENCE", "ORGANIZER", "X-PM-CONFERENCE-ID"},
		SharedEncrypted:   {"UID", "DTSTAMP", "CREATED", "SUMMARY", "DESCRIPTION", "LOCATION", "X-MOZ-GENERATION", "CATEGORIES", "CLASS", "URL", "LAST-MODIFIED"},
		CalendarSigned:    {"UID", "DTSTAMP", "EXDATE", "STATUS", "TRANSP"},
		CalendarEncrypted: {"UID", "DTSTAMP", "COMMENT"},
		Attendees:         {"UID", "DTSTAMP", "ATTENDEE"},
	}
	got := Route(ev)
	if !slices.Equal(slices.Sorted(maps.Keys(got)), partKinds) {
		t.Fatalf("parts %v", slices.Sorted(maps.Keys(got)))
	}
	for k, w := range want {
		if !slices.Equal(names(got[k]), w) {
			t.Errorf("%s: %v, want %v", k, names(got[k]), w)
		}
	}
	if got[SharedSigned][6] != got[CalendarSigned][2] || got[SharedSigned][6].Line() != exdate1 {
		t.Errorf("EXDATE not the same line in both signed parts")
	}
	minimal := Route(comp(t, uidLine, stampLine, "DTSTART:20261105T100000Z", "SUMMARY:s"))
	if !slices.Equal(slices.Sorted(maps.Keys(minimal)), []PartKind{SharedSigned, SharedEncrypted, CalendarSigned}) {
		t.Fatalf("minimal event parts %v: the calendar encrypted and attendees parts come only with content", slices.Sorted(maps.Keys(minimal)))
	}
	if !slices.Equal(names(minimal[CalendarSigned]), []string{"UID", "DTSTAMP"}) {
		t.Errorf("minimal calendar signed part %v", names(minimal[CalendarSigned]))
	}
}

func TestPatchKeepsUntouchedBytes(t *testing.T) {
	cur := stored()
	in := set(t, served(t, cur), "SUMMARY", "SUMMARY:New title")
	got, changed := mustPatch(t, cur, in)
	want := stored()
	want[SharedEncrypted] = frag(uidLine, stampLine, "CREATED:20261101T070000Z", "SUMMARY:New title", oddDescription, "LOCATION:Room 1",
		"X-MOZ-GENERATION:3", `X-FOO;B="two";A=1:v`)
	expectParts(t, got, changed, want, SharedEncrypted)

	// Nothing edited: every part comes back byte for byte, nothing changed.
	same, changed := mustPatch(t, cur, served(t, cur))
	expectParts(t, same, changed, stored())
}

func TestPatchLineIdentity(t *testing.T) {
	cur := stored()
	base := served(t, cur)
	// Same meaning: parameter order, parameter name case, a quoted value
	// unquoted, an enumerated value's case; the stored bytes stay.
	in := set(t, base, "X-FOO", "X-FOO;a=1;b=two:v")
	in = set(t, in, "ORGANIZER", `ORGANIZER;cn="Org":mailto:org@example.com`)
	in = set(t, in, "LOCATION", "LOCATION;VALUE=TEXT:Room 1")
	cur[SharedEncrypted] = frag(uidLine, stampLine, "CREATED:20261101T070000Z", "SUMMARY:Title", oddDescription, "LOCATION;value=text:Room 1",
		"X-MOZ-GENERATION:3", `X-FOO;B="two";A=1:v`)
	got, changed := mustPatch(t, cur, in)
	expectParts(t, got, changed, cur)

	// Another value is another line, even if only its case differs: a
	// multi-valued property's old line goes and the new one is appended.
	in = set(t, base, "X-FOO", "X-FOO;A=1;B=two:V")
	got, changed = mustPatch(t, stored(), in)
	want := stored()
	want[SharedEncrypted] = frag(uidLine, stampLine, "CREATED:20261101T070000Z", "SUMMARY:Title", oddDescription, "LOCATION:Room 1",
		"X-MOZ-GENERATION:3", "X-FOO;A=1;B=two:V")
	expectParts(t, got, changed, want, SharedEncrypted)
}

func TestPatchMovesNothingAcrossParts(t *testing.T) {
	// Lines where the routing table would not put them: LOCATION in the
	// calendar encrypted part, EXDATE in the calendar signed part only, an
	// X- property in the shared signed part.
	cur := map[PartKind][]byte{
		SharedSigned:      frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "RRULE:FREQ=DAILY", "X-ODD:1"),
		SharedEncrypted:   frag(uidLine, stampLine, "SUMMARY:Title"),
		CalendarSigned:    frag(uidLine, stampLine, "EXDATE:20261106T100000Z"),
		CalendarEncrypted: frag(uidLine, stampLine, "LOCATION:Room 1"),
	}
	in := served(t, cur)
	got, changed := mustPatch(t, cur, in)
	expectParts(t, got, changed, cur)

	in = set(t, in, "LOCATION", "LOCATION:Room 2")
	in = set(t, in, "CATEGORIES", "CATEGORIES:new")
	in = without(in, named("X-ODD"))
	got, changed = mustPatch(t, cur, in)
	want := map[PartKind][]byte{
		SharedSigned:      frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "RRULE:FREQ=DAILY"),
		SharedEncrypted:   frag(uidLine, stampLine, "SUMMARY:Title", "CATEGORIES:new"),
		CalendarSigned:    cur[CalendarSigned],
		CalendarEncrypted: frag(uidLine, stampLine, "LOCATION:Room 2"),
	}
	expectParts(t, got, changed, want, SharedSigned, SharedEncrypted, CalendarEncrypted)
}

func TestPatchMultiValuedAsMultiset(t *testing.T) {
	cur := stored()
	base := served(t, cur)
	comments := func(c Component, values ...string) Component {
		c = without(c, named("COMMENT"))
		for _, v := range values {
			c.addProp(Property{Name: "COMMENT", Value: v})
		}
		return c
	}
	got, changed := mustPatch(t, cur, comments(base, "note two", "note one"))
	expectParts(t, got, changed, stored())

	got, changed = mustPatch(t, cur, comments(base, "note two", "note three"))
	want := stored()
	want[CalendarEncrypted] = frag(uidLine, stampLine, "COMMENT:note two", "COMMENT:note three")
	expectParts(t, got, changed, want, CalendarEncrypted)

	got, changed = mustPatch(t, cur, comments(base, "note one", "note two", "note one"))
	want = stored()
	want[CalendarEncrypted] = frag(uidLine, stampLine, "COMMENT:note one", "COMMENT:note two", "COMMENT:note one")
	expectParts(t, got, changed, want, CalendarEncrypted)
}

func TestPatchExdateInBothSignedParts(t *testing.T) {
	cur := stored()
	base := served(t, cur)
	const exdate2 = "EXDATE;TZID=Europe/Paris:20261119T100000"
	p, _ := splitLine(exdate2)
	in := base.Clone()
	in.addProp(p)
	got, changed := mustPatch(t, cur, in)
	want := stored()
	want[SharedSigned] = frag(uidLine, stampLine, "DTSTART;TZID=Europe/Paris:20261105T100000", "DTEND;TZID=Europe/Paris:20261105T110000",
		"RRULE:FREQ=WEEKLY", exdate1, "SEQUENCE:2", "ORGANIZER;CN=Org:mailto:org@example.com", exdate2)
	want[CalendarSigned] = frag(uidLine, stampLine, exdate1, "STATUS:CONFIRMED", "TRANSP:OPAQUE", exdate2)
	expectParts(t, got, changed, want, SharedSigned, CalendarSigned)

	got, changed = mustPatch(t, cur, without(base, named("EXDATE")))
	want = stored()
	want[SharedSigned] = frag(uidLine, stampLine, "DTSTART;TZID=Europe/Paris:20261105T100000", "DTEND;TZID=Europe/Paris:20261105T110000",
		"RRULE:FREQ=WEEKLY", "SEQUENCE:2", "ORGANIZER;CN=Org:mailto:org@example.com")
	want[CalendarSigned] = frag(uidLine, stampLine, "STATUS:CONFIRMED", "TRANSP:OPAQUE")
	expectParts(t, got, changed, want, SharedSigned, CalendarSigned)
}

func TestPatchDTStampReplacedInEveryPart(t *testing.T) {
	cur := stored()
	got, _ := mustPatch(t, cur, set(t, served(t, cur), "DTSTAMP", "DTSTAMP:20261111T111111Z"))
	for _, k := range partKinds {
		want := bytes.Replace(cur[k], []byte(stampLine), []byte("DTSTAMP:20261111T111111Z"), 1)
		if !bytes.Equal(got[k], want) {
			t.Errorf("%s:\n got %q\nwant %q", k, got[k], want)
		}
	}
}

func TestPatchAttendeeKeepsStoredLine(t *testing.T) {
	cur := stored()
	base := served(t, cur)
	if got := values(base, "ATTENDEE"); len(got) != 1 || !strings.Contains(got[0], "PARTSTAT=ACCEPTED") || strings.Contains(got[0], "X-PM-TOKEN") {
		t.Fatalf("the render does not serve what the test assumes: %v", got)
	}
	// Thunderbird re-sends the attendee without the token, with the
	// injected PARTSTAT, another CN and the address in another case.
	in := set(t, base, "ATTENDEE", "ATTENDEE;PARTSTAT=ACCEPTED;CN=Ann B:MAILTO:Ann@Example.com")
	got, changed := mustPatch(t, cur, in)
	expectParts(t, got, changed, stored())
	added, removed, err := AttendeeChanges(cur, in)
	if err != nil || len(added) != 0 || len(removed) != 0 {
		t.Fatalf("same attendee: added %v removed %v err %v", added, removed, err)
	}

	in = set(t, base, "ATTENDEE", "ATTENDEE;CN=Bob:mailto:bob@example.com")
	in.addProp(Property{Name: "ATTENDEE", Params: ";X-PM-TOKEN=forged", Value: "mailto:cy@example.com"})
	added, removed, err = AttendeeChanges(cur, in)
	if err != nil || !slices.Equal(added, []string{"mailto:bob@example.com", "mailto:cy@example.com"}) || !slices.Equal(removed, []string{"mailto:ann@example.com"}) {
		t.Fatalf("added %v removed %v err %v", added, removed, err)
	}
	added, removed, err = AttendeeChanges(nil, in)
	if err != nil || len(added) != 2 || len(removed) != 0 {
		t.Fatalf("on create: added %v removed %v err %v", added, removed, err)
	}
}

func TestPatchKeepsServerOwnedLines(t *testing.T) {
	cur := map[PartKind][]byte{
		SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z"),
		SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:Title", "COLOR:tomato", "X-PM-SESSION-KEY:a2V5", "X-PCB-INTEGRITY:unverified", "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "END:VALARM"),
		CalendarSigned:  frag(uidLine, stampLine),
	}
	// Patch is handed an event that was not normalised: the lines only the
	// server or the render may write are not taken from it.
	in := comp(t, uidLine, stampLine, "DTSTART:20261105T100000Z", "SUMMARY:Title", "LOCATION:new", "COLOR:blue",
		"X-PCB-PLACEHOLDER:1", "X-PM-SESSION-KEY:Zm9yZ2Vk", "DURATION:PT1H", "BEGIN:VALARM", "ACTION:EMAIL", "TRIGGER:-PT1M", "END:VALARM")
	got, changed := mustPatch(t, cur, in)
	want := map[PartKind][]byte{
		SharedSigned:    cur[SharedSigned],
		SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:Title", "COLOR:tomato", "X-PM-SESSION-KEY:a2V5", "X-PCB-INTEGRITY:unverified", "LOCATION:new", "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "END:VALARM"),
		CalendarSigned:  cur[CalendarSigned],
	}
	expectParts(t, got, changed, want, SharedEncrypted)
}

func TestPatchCanonicalFragments(t *testing.T) {
	cur := map[PartKind][]byte{
		// Proton-stored shapes: VERSION/PRODID, a VTIMEZONE, a final CRLF, LF line ends.
		SharedSigned: []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Proton AG//web-calendar//EN\r\nBEGIN:VTIMEZONE\r\nTZID:Europe/Paris\r\nEND:VTIMEZONE\r\n" +
			"BEGIN:VEVENT\r\nUID:u1\r\nDTSTAMP:20261101T080000Z\r\nDTSTART:20261105T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"),
		SharedEncrypted: []byte("BEGIN:VCALENDAR\nVERSION:2.0\nBEGIN:VEVENT\nUID:u1\nDTSTAMP:20261101T080000Z\nSUMMARY:a summary folded\n  with LF\nEND:VEVENT\nEND:VCALENDAR\n"),
		CalendarSigned:  []byte("BEGIN:VEVENT\r\nUID:u1\r\nDTSTAMP:20261101T080000Z\r\nEND:VEVENT"),
	}
	in := comp(t, uidLine, stampLine, "DTSTART:20261105T100000Z", "SUMMARY:a summary folded with LF")
	got, changed := mustPatch(t, cur, in)
	want := map[PartKind][]byte{
		SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z"),
		SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:a summary folded\r\n  with LF"),
		CalendarSigned:  frag(uidLine, stampLine),
	}
	expectParts(t, got, changed, want, SharedSigned, SharedEncrypted, CalendarSigned)

	again, changed := mustPatch(t, got, in)
	expectParts(t, again, changed, want)
}

func TestPatchCreate(t *testing.T) {
	long := "SUMMARY:" + strings.Repeat("é", 60)
	in := comp(t, uidLine, stampLine, "DTSTART:20261105T100000Z", long, "ORGANIZER:mailto:o@x", "ATTENDEE:mailto:a@x", "COLOR:red")
	for _, current := range []map[PartKind][]byte{nil, {}} {
		got, changed := mustPatch(t, current, in)
		routed := Route(in)
		if !slices.Equal(slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(routed))) {
			t.Fatalf("parts %v, routed %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(routed)))
		}
		for k, props := range routed {
			if !bytes.Equal(got[k], Fragment(props)) || !changed[k] {
				t.Errorf("%s: %q changed=%v, want Fragment(Route)", k, got[k], changed[k])
			}
		}
		if bytes.HasSuffix(got[SharedEncrypted], []byte("\r\n")) || !bytes.HasPrefix(got[SharedEncrypted], []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n")) {
			t.Errorf("not canonical: %q", got[SharedEncrypted])
		}
		for l := range strings.SplitSeq(string(got[SharedEncrypted]), "\r\n") {
			if len(l) > 75 {
				t.Errorf("line of %d octets: %q", len(l), l)
			}
		}
		if bytes.Contains(got[SharedEncrypted], []byte("COLOR")) {
			t.Errorf("COLOR written into a part")
		}
	}
}

func TestPatchPartsComeAndGo(t *testing.T) {
	cur := stored()
	base := served(t, cur)
	got, changed := mustPatch(t, cur, without(without(base, named("COMMENT")), named("ATTENDEE")))
	want := stored()
	delete(want, CalendarEncrypted)
	delete(want, Attendees)
	expectParts(t, got, changed, want, CalendarEncrypted, Attendees)

	bare := map[PartKind][]byte{
		SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "ORGANIZER:mailto:o@x"),
		SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:s"),
		CalendarSigned:  frag(uidLine, stampLine),
	}
	in := served(t, bare)
	in.addProp(Property{Name: "COMMENT", Value: "hello"})
	in.addProp(Property{Name: "ATTENDEE", Value: "mailto:a@x"})
	got, changed = mustPatch(t, bare, in)
	want = maps.Clone(bare)
	want[CalendarEncrypted] = frag(uidLine, stampLine, "COMMENT:hello")
	want[Attendees] = frag(uidLine, stampLine, "ATTENDEE:mailto:a@x")
	expectParts(t, got, changed, want, CalendarEncrypted, Attendees)
}

func TestPatchErrors(t *testing.T) {
	in := comp(t, uidLine, stampLine, "DTSTART:20261105T100000Z")
	for name, part := range map[string][]byte{
		"not iCalendar": []byte("secret-title-xyz without a colon\r\n"),
		"no VEVENT":     []byte("BEGIN:VCALENDAR\r\nX-SECRET:secret-title-xyz\r\nEND:VCALENDAR"),
	} {
		t.Run(name, func(t *testing.T) {
			cur := stored()
			cur[CalendarEncrypted] = part
			_, _, err := Patch(cur, in)
			if err == nil || !strings.Contains(err.Error(), CalendarEncrypted.String()) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("err = %v, want the part named and no content", err)
			}
			if _, _, err := AttendeeChanges(cur, in); err == nil {
				t.Fatalf("AttendeeChanges accepted a broken part")
			}
		})
	}
}

func TestIsSharedSignedChange(t *testing.T) {
	old := comp(t, uidLine, stampLine, "DTSTART;TZID=Europe/Paris:20261105T100000", "DURATION:PT1H", "RRULE:FREQ=WEEKLY;BYDAY=TH;COUNT=5",
		"EXDATE;TZID=Europe/Paris:20261112T100000,20261119T100000", "SEQUENCE:2", "SUMMARY:s")
	same := comp(t, uidLine, "DTSTAMP:20261111T000000Z", "DTSTART;TZID=Europe/Paris:20261105T100000", "DTEND;TZID=Europe/Paris:20261105T110000",
		"RRULE:COUNT=5;FREQ=WEEKLY;BYDAY=TH", "EXDATE;TZID=Europe/Paris:20261119T100000", "EXDATE;TZID=Europe/Paris:20261112T100000",
		"SEQUENCE:3", "SUMMARY:other title", "BEGIN:VALARM", "ACTION:DISPLAY", "TRIGGER:-PT5M", "END:VALARM")
	if IsSharedSignedChange(old, same) {
		t.Fatalf("text, DTSTAMP, SEQUENCE, alarm, RRULE part order, EXDATE split and DURATION→DTEND count as a time change")
	}
	for name, edit := range map[string]func(Component) Component{
		"DTSTART":       func(c Component) Component { return set(t, c, "DTSTART", "DTSTART;TZID=Europe/Paris:20261105T090000") },
		"DTEND":         func(c Component) Component { return set(t, c, "DTEND", "DTEND;TZID=Europe/Paris:20261105T120000") },
		"zone":          func(c Component) Component { return set(t, c, "DTSTART", "DTSTART;TZID=Europe/Berlin:20261105T100000") },
		"RRULE":         func(c Component) Component { return set(t, c, "RRULE", "RRULE:FREQ=WEEKLY;BYDAY=TH;COUNT=6") },
		"RRULE removed": func(c Component) Component { return without(c, named("RRULE")) },
		"EXDATE added":  func(c Component) Component { return set(t, c, "X-NONE", "EXDATE;TZID=Europe/Paris:20261126T100000") },
		"EXDATE gone":   func(c Component) Component { return without(c, named("EXDATE")) },
		"RECURRENCE-ID": func(c Component) Component { return set(t, c, "X-NONE", "RECURRENCE-ID:20261105T090000Z") },
	} {
		if !IsSharedSignedChange(old, edit(same)) {
			t.Errorf("%s edit not seen as a shared signed change", name)
		}
	}
}
