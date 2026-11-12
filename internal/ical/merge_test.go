package ical

import (
	"encoding/json"
	"strings"
	"testing"
)

// part builds a Proton-shaped card part: a VCALENDAR wrapper around one VEVENT.
func part(lines ...string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Proton AG//x//EN\r\nBEGIN:VEVENT\r\n" +
		strings.Join(lines, "\r\n") + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
}

func values(c Component, name string) []string {
	var out []string
	for _, p := range c.Props() {
		if strings.EqualFold(p.Name, name) {
			out = append(out, p.Params+":"+p.Value)
		}
	}
	return out
}

func mustMerge(t *testing.T, parts ...[]byte) Component {
	t.Helper()
	ev, err := MergeEvent(parts)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestMergeStructuralFromSharedSigned(t *testing.T) {
	ev := mustMerge(t,
		part("UID:real", "DTSTAMP:20261101T000000Z", "DTSTART;TZID=Europe/Paris:20261105T100000", "SEQUENCE:2", "RRULE:FREQ=WEEKLY"),
		part("UID:other", "DTSTART:19990101T000000Z", "SEQUENCE:9", "SUMMARY:first", "RRULE:FREQ=DAILY", "DTEND:20261105T110000Z"),
		part("SUMMARY:second", "LOCATION:here", "DTEND:20300101T000000Z"),
	)
	want := map[string]string{
		"UID": ":real", "DTSTART": ";TZID=Europe/Paris:20261105T100000", "SEQUENCE": ":2",
		"RRULE": ":FREQ=WEEKLY", "SUMMARY": ":first", "LOCATION": ":here", "DTEND": ":20261105T110000Z",
	}
	for name, w := range want {
		got := values(ev, name)
		if len(got) != 1 || got[0] != w {
			t.Errorf("%s = %v, want one value %q", name, got, w)
		}
	}
	if ev.Name != "VEVENT" {
		t.Fatalf("name %q", ev.Name)
	}
}

func TestMergeMultiValuedUnion(t *testing.T) {
	ev := mustMerge(t,
		part("UID:u", "EXDATE:20261112T100000Z", "EXDATE:20261119T100000Z", "ATTENDEE;X-PM-TOKEN=t1:mailto:a@x"),
		part("EXDATE:20261119T100000Z", "EXDATE:20261126T100000Z", "CATEGORIES:A", "ATTENDEE;X-PM-TOKEN=t1:mailto:a@x"),
		part("CATEGORIES:A", "CATEGORIES:B", "ATTENDEE;X-PM-TOKEN=t2:mailto:b@x", "COMMENT:c"),
	)
	if got := values(ev, "EXDATE"); strings.Join(got, ",") != ":20261112T100000Z,:20261119T100000Z,:20261126T100000Z" {
		t.Errorf("EXDATE = %v", got)
	}
	if got := values(ev, "ATTENDEE"); len(got) != 2 {
		t.Errorf("ATTENDEE = %v", got)
	}
	if got := values(ev, "CATEGORIES"); strings.Join(got, ",") != ":A,:B" {
		t.Errorf("CATEGORIES = %v", got)
	}
}

func TestMergeDropsSessionKey(t *testing.T) {
	ev := mustMerge(t,
		part("UID:u", "X-PM-SESSION-KEY:secretkey=="),
		part("X-PM-SESSION-KEY;X=1:other", "X-PM-CONFERENCE-ID:123", "X-MOZ-LASTACK:20261101T000000Z"),
	)
	if got := values(ev, "X-PM-SESSION-KEY"); len(got) != 0 {
		t.Fatalf("session key kept: %v", got)
	}
	if strings.Contains(string(ev.Bytes()), "secretkey") {
		t.Fatal("session key value leaked")
	}
	if len(values(ev, "X-PM-CONFERENCE-ID")) != 1 || len(values(ev, "X-MOZ-LASTACK")) != 1 {
		t.Fatal("other X- properties must be kept verbatim")
	}
}

func TestMergeAcceptsBareVEventAndLF(t *testing.T) {
	ev := mustMerge(t,
		[]byte("BEGIN:VEVENT\nUID:u\nSUMMARY:long\n  text\nEND:VEVENT"),
		part("LOCATION:x"),
	)
	if got := values(ev, "SUMMARY"); len(got) != 1 || got[0] != ":long text" {
		t.Fatalf("%v", got)
	}
	if len(values(ev, "LOCATION")) != 1 {
		t.Fatal("second part lost")
	}
}

func TestMergeErrors(t *testing.T) {
	if _, err := MergeEvent(nil); err == nil {
		t.Error("no parts must fail")
	}
	if _, err := MergeEvent([][]byte{part("UID:u"), []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nEND:VCALENDAR\r\n")}); err == nil {
		t.Error("a part without VEVENT must fail")
	}
	_, err := MergeEvent([][]byte{[]byte("BEGIN:VEVENT\r\nSECRETCONTENT\r\nEND:VEVENT\r\n")})
	if err == nil || strings.Contains(err.Error(), "SECRETCONTENT") {
		t.Errorf("malformed part: want error without content, got %v", err)
	}
}

func TestMergeKeepsNestedComponentsVerbatim(t *testing.T) {
	ev := mustMerge(t,
		part("UID:u", "BEGIN:VALARM", "ACTION:AUDIO", "TRIGGER:-PT5M", "END:VALARM"),
	)
	subs := ev.Subs("VALARM")
	if len(subs) != 1 || string(subs[0].Bytes()) != "BEGIN:VALARM\r\nACTION:AUDIO\r\nTRIGGER:-PT5M\r\nEND:VALARM\r\n" {
		t.Fatalf("%q", ev.Bytes())
	}
}

func TestMergeDoesNotUnfoldAwayContent(t *testing.T) {
	long := strings.Repeat("é", 100)
	ev := mustMerge(t, part("UID:u", "DESCRIPTION:"+long))
	p, _ := ev.Get("DESCRIPTION")
	if p.Value != long {
		t.Fatal("description changed")
	}
}

func alarmActions(c []Component) []string {
	var out []string
	for _, a := range c {
		act, _ := a.Get("ACTION")
		tr, _ := a.Get("TRIGGER")
		out = append(out, act.Value+"/"+tr.Value)
	}
	return out
}

func TestAlarmsTriState(t *testing.T) {
	defaults := json.RawMessage(`[{"Type":1,"Trigger":"-PT30M"}]`)
	fullDay := json.RawMessage(`[{"Type":0,"Trigger":"-PT15H"}]`)

	// null -> defaults.
	got, err := Alarms(json.RawMessage(`null`), defaults, false)
	if err != nil {
		t.Fatal(err)
	}
	if a := alarmActions(got); len(a) != 1 || a[0] != "DISPLAY/-PT30M" {
		t.Errorf("null: %v", a)
	}
	// [] -> none even with defaults.
	got, err = Alarms(json.RawMessage(`[]`), defaults, false)
	if err != nil || len(got) != 0 {
		t.Errorf("[]: %v %v", got, err)
	}
	// custom array wins over defaults; Type 0 = EMAIL, 1 = DISPLAY.
	got, err = Alarms(json.RawMessage(`[{"Type":0,"Trigger":"-PT15M"},{"Type":1,"Trigger":"PT0S"},{"Type":1,"Trigger":"-P1DT9H"}]`), defaults, false)
	if err != nil {
		t.Fatal(err)
	}
	if a := strings.Join(alarmActions(got), ","); a != "EMAIL/-PT15M,DISPLAY/PT0S,DISPLAY/-P1DT9H" {
		t.Errorf("custom: %s", a)
	}
	// DISPLAY and EMAIL carry DESCRIPTION; EMAIL also SUMMARY.
	for i, a := range got[:2] {
		if d, ok := a.Get("DESCRIPTION"); !ok || d.Value == "" {
			t.Errorf("alarm %d lacks DESCRIPTION", i)
		}
	}
	if _, ok := got[0].Get("SUMMARY"); !ok {
		t.Error("EMAIL alarm lacks SUMMARY")
	}
	if _, ok := got[1].Get("SUMMARY"); ok {
		t.Error("DISPLAY alarm must not carry SUMMARY")
	}
	// The exact serialisation.
	if s := string(got[1].Bytes()); !strings.HasPrefix(s, "BEGIN:VALARM\r\nACTION:DISPLAY\r\nTRIGGER:PT0S\r\nDESCRIPTION:") || !strings.HasSuffix(s, "\r\nEND:VALARM\r\n") {
		t.Errorf("shape: %q", s)
	}
	// All-day: the caller passes the full-day defaults; they are used as given.
	got, err = Alarms(json.RawMessage(`null`), fullDay, true)
	if err != nil {
		t.Fatal(err)
	}
	if a := alarmActions(got); len(a) != 1 || a[0] != "EMAIL/-PT15H" {
		t.Errorf("all-day defaults: %v", a)
	}
	// null defaults and null notifications: nothing.
	got, err = Alarms(json.RawMessage(`null`), nil, false)
	if err != nil || len(got) != 0 {
		t.Errorf("no defaults: %v %v", got, err)
	}
}

func TestAlarmsRejectsBadInput(t *testing.T) {
	for _, bad := range []string{`{"a":1}`, `[{"Type":1,"Trigger":"tomorrow"}]`, `[{"Type":1}]`, `[`,
		`[{"Type":1,"Trigger":"-PT5M\r\nX-EVIL:1"}]`, `[{"Type":1,"Trigger":"-PT5M\n"}]`, `[{"Type":1,"Trigger":"-PT5M\r"}]`,
		`[{"Type":1,"Trigger":"x-PT5M"}]`, `[{"Type":1,"Trigger":"-PT"}]`} {
		if _, err := Alarms(json.RawMessage(bad), nil, false); err == nil {
			t.Errorf("%s: expected error", bad)
		}
	}
	if _, err := Alarms(nil, json.RawMessage(`[{"Type":1,"Trigger":"x"}]`), false); err == nil {
		t.Error("bad defaults must fail")
	}
}

func TestApplyNotificationsReplacesPartAlarms(t *testing.T) {
	partAlarm := []string{"BEGIN:VALARM", "ACTION:AUDIO", "TRIGGER:-PT99M", "END:VALARM"}
	mk := func() Component { return mustMerge(t, part(append([]string{"UID:u"}, partAlarm...)...)) }
	defaults := json.RawMessage(`[{"Type":1,"Trigger":"-PT30M"}]`)

	// Notifications present (custom): part alarms dropped.
	ev := mk()
	if _, err := ApplyNotifications(&ev, json.RawMessage(`[{"Type":1,"Trigger":"-PT5M"}]`), defaults, false); err != nil {
		t.Fatal(err)
	}
	if a := alarmActions(ev.Subs("VALARM")); len(a) != 1 || a[0] != "DISPLAY/-PT5M" {
		t.Errorf("custom: %v", a)
	}
	// null counts as present: defaults replace part alarms.
	ev = mk()
	if _, err := ApplyNotifications(&ev, json.RawMessage(`null`), defaults, false); err != nil {
		t.Fatal(err)
	}
	if a := alarmActions(ev.Subs("VALARM")); len(a) != 1 || a[0] != "DISPLAY/-PT30M" {
		t.Errorf("null: %v", a)
	}
	// [] present: no alarms at all.
	ev = mk()
	if _, err := ApplyNotifications(&ev, json.RawMessage(`[]`), defaults, false); err != nil {
		t.Fatal(err)
	}
	if n := len(ev.Subs("VALARM")); n != 0 {
		t.Errorf("[]: %d alarms left", n)
	}
	// Absent (nil): part alarms untouched.
	ev = mk()
	if _, err := ApplyNotifications(&ev, nil, defaults, false); err != nil {
		t.Fatal(err)
	}
	if a := alarmActions(ev.Subs("VALARM")); len(a) != 1 || a[0] != "AUDIO/-PT99M" {
		t.Errorf("absent: %v", a)
	}
	// Alarms land after the properties and the VEVENT stays well-formed.
	ev = mk()
	_, _ = ApplyNotifications(&ev, json.RawMessage(`[{"Type":1,"Trigger":"-PT5M"}]`), nil, false)
	if _, err := Parse(ev.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRowColor(t *testing.T) {
	ev := mustMerge(t, part("UID:u", "COLOR:red"))
	ApplyRow(&ev, "#8080FF", nil)
	if got := values(ev, "COLOR"); len(got) != 1 || got[0] != ":#8080FF" {
		t.Fatalf("row color must replace the part's: %v", got)
	}
	ev = mustMerge(t, part("UID:u"))
	ApplyRow(&ev, "", nil)
	if len(values(ev, "COLOR")) != 0 {
		t.Fatal("empty row color must add nothing")
	}
}

func TestPartstatJoin(t *testing.T) {
	ev := mustMerge(t, part(
		"UID:u",
		"ATTENDEE;CN=A;X-PM-TOKEN=ta:mailto:a@x",
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;CN=\"B, b\";X-PM-TOKEN=tb;ROLE=OPT-PARTICIPANT:mailto:b@x",
		"ATTENDEE;X-PM-TOKEN=tc:mailto:c@x",
		"ATTENDEE;X-PM-TOKEN=td:mailto:d@x",
		"ATTENDEE;X-PM-TOKEN=unknown;PARTSTAT=TENTATIVE:mailto:e@x",
		"ATTENDEE;CN=NoToken:mailto:f@x",
		"ORGANIZER;CN=O:mailto:o@x",
	))
	ApplyRow(&ev, "", map[string]int{"ta": 3, "tb": 2, "tc": 1, "td": 0})
	want := []string{
		";CN=A;PARTSTAT=ACCEPTED:mailto:a@x",
		`;PARTSTAT=DECLINED;CN="B, b";ROLE=OPT-PARTICIPANT:mailto:b@x`,
		";PARTSTAT=TENTATIVE:mailto:c@x",
		";PARTSTAT=NEEDS-ACTION:mailto:d@x",
		";PARTSTAT=TENTATIVE:mailto:e@x",
		";CN=NoToken:mailto:f@x",
	}
	got := values(ev, "ATTENDEE")
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if strings.Contains(string(ev.Bytes()), "X-PM-TOKEN") {
		t.Fatal("X-PM-TOKEN must be dropped")
	}
	if o := values(ev, "ORGANIZER"); len(o) != 1 || o[0] != ";CN=O:mailto:o@x" {
		t.Fatalf("organizer touched: %v", o)
	}
}

func TestApplyRowRejectsColorInjection(t *testing.T) {
	ev := mustMerge(t, part("UID:u", "COLOR:red"))
	ApplyRow(&ev, "#fff\r\nX-EVIL:1", nil)
	if strings.Contains(string(ev.Bytes()), "X-EVIL") {
		t.Fatal("CR/LF color reached the output")
	}
	if got := values(ev, "COLOR"); len(got) != 1 || got[0] != ":red" {
		t.Fatalf("part color must stay: %v", got)
	}
}

func TestApplyRowDedupsAttendees(t *testing.T) {
	ev := mustMerge(t, part("UID:u",
		"ATTENDEE;X-PM-TOKEN=t1;CN=First:mailto:A@Example.com",
		"ATTENDEE;X-PM-TOKEN=t2;CN=Dup:mailto:a@example.com",
		"ATTENDEE;CN=Other:mailto:b@example.com"))
	ApplyRow(&ev, "", map[string]int{"t1": 3, "t2": 2})
	got := values(ev, "ATTENDEE")
	if len(got) != 2 || got[0] != ";CN=First;PARTSTAT=ACCEPTED:mailto:A@Example.com" {
		t.Fatalf("%v", got)
	}
}

// Ruling M3-R33: the integrity marker is pcb's alone. A part's own
// X-PCB-INTEGRITY (any case, any value) never reaches the client; an
// unverified event gets exactly one, as a property before the alarms.
func TestMarkIntegrity(t *testing.T) {
	ev := mustMerge(t, part("UID:u", "x-pcb-integrity:verified", "BEGIN:VALARM", "ACTION:DISPLAY", "END:VALARM"))
	MarkIntegrity(&ev, false)
	if got := values(ev, "X-PCB-INTEGRITY"); len(got) != 0 {
		t.Fatalf("a verified event keeps a part's marker: %v", got)
	}
	ev = mustMerge(t, part("UID:u", "X-PCB-INTEGRITY:verified", "BEGIN:VALARM", "ACTION:DISPLAY", "END:VALARM"))
	MarkIntegrity(&ev, true)
	if got := values(ev, "X-PCB-INTEGRITY"); len(got) != 1 || got[0] != ":unverified" {
		t.Fatalf("an unverified event's markers = %v, want [:unverified]", got)
	}
	if b := string(ev.Bytes()); !strings.Contains(b, "X-PCB-INTEGRITY:unverified\r\nBEGIN:VALARM") {
		t.Fatalf("the marker is not the last property before the alarm:\n%s", b)
	}
}

// A malformed reminder is left out and counted; the good ones and the rest of
// the event stay (audit read-M4). Alarms, the write path's validator, stays strict.
func TestApplyNotificationsDropsMalformedReminders(t *testing.T) {
	ev := mustMerge(t, part("UID:u", "SUMMARY:Keep"))
	raw := json.RawMessage(`[{"Trigger":"-PT5M"},{"Type":1,"Trigger":"soon"},{"Type":1,"Trigger":"-PT10M"}]`)
	dropped, err := ApplyNotifications(&ev, raw, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	if a := alarmActions(ev.Subs("VALARM")); len(a) != 1 || a[0] != "DISPLAY/-PT10M" {
		t.Errorf("alarms = %v, want only DISPLAY/-PT10M", a)
	}
	if v := values(ev, "SUMMARY"); len(v) != 1 {
		t.Errorf("event lost its SUMMARY: %v", v)
	}
	if _, err := Alarms(raw, nil, false); err == nil {
		t.Error("Alarms (write validation) must still refuse a malformed reminder")
	}
	if _, err := ApplyNotifications(&ev, json.RawMessage(`{"x":1}`), nil, false); err == nil {
		t.Error("a value that is no list must still fail")
	}
}

// Controls other than HTAB are dropped from every emitted value, parameter and
// nested component (audit read-M2); the TEXT escape for a newline stays.
func TestMergeEventDropsControls(t *testing.T) {
	ev := mustMerge(t, []byte("BEGIN:VEVENT\nUID:u\nSUMMARY:x\rX-EVIL:1\x00\x7f\ty\nDESCRIPTION:a\\nb\nATTENDEE;CN=a\x01b:mailto:a@x\nBEGIN:VALARM\nACTION:DISPLAY\x1b\nEND:VALARM\nEND:VEVENT"))
	out := string(ev.Bytes())
	for _, want := range []string{"SUMMARY:xX-EVIL:1\ty\r\n", "DESCRIPTION:a\\nb\r\n", "ATTENDEE;CN=ab:mailto:a@x\r\n", "ACTION:DISPLAY\r\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%q", want, out)
		}
	}
	if i := controlIn(out); i >= 0 {
		t.Errorf("control at %d in %q", i, out)
	}
	c := mustMerge(t, part("UID:u"))
	ApplyRow(&c, "#a\x01bc", nil)
	if v := values(c, "COLOR"); len(v) != 1 || v[0] != ":#abc" {
		t.Errorf("color = %v", v)
	}
}

// controlIn is the index of the first C0 control other than HTAB and the
// CRLF of a line end, or DEL; -1 for none.
func controlIn(s string) int {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\r' && i+1 < len(s) && s[i+1] == '\n', c == '\n' && i > 0 && s[i-1] == '\r', c == '\t':
		case c < 0x20, c == 0x7f:
			return i
		}
	}
	return -1
}

// Valid non-ASCII next to a dropped control survives intact (the drop is by
// rune, not by byte).
func TestDropControlsKeepsNonASCII(t *testing.T) {
	for in, want := range map[string]string{
		"é\x01😀":      "é😀",
		"日本\x7f語\x00": "日本語",
		"naïve\tcafé": "naïve\tcafé",
	} {
		if got := dropControls(in); got != want {
			t.Errorf("dropControls(%q) = %q, want %q", in, got, want)
		}
	}
	ev := mustMerge(t, []byte("BEGIN:VEVENT\nSUMMARY:é\x01😀\nEND:VEVENT"))
	if v := values(ev, "SUMMARY"); len(v) != 1 || v[0] != ":é😀" {
		t.Errorf("SUMMARY = %q", v)
	}
}
