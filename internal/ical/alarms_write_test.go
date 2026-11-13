package ical

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// alarm builds a VALARM from content lines.
func alarm(lines ...string) Component {
	c := Component{Name: "VALARM"}
	for _, l := range lines {
		p, ok := splitLine(l)
		if !ok {
			panic("bad test line " + l)
		}
		c.add(p)
	}
	return c
}

func display(trigger string) Component { return alarm("ACTION:DISPLAY", trigger) }

func TestNotificationsFromAlarms(t *testing.T) {
	cases := []struct {
		name   string
		alarm  Component
		allDay bool
		want   string // Type/Trigger, "" when dropped
	}{
		{"display", display("TRIGGER:-PT15M"), false, "1/-PT15M"},
		{"audio", alarm("ACTION:AUDIO", "TRIGGER:-PT15M"), false, "1/-PT15M"},
		{"email (Proton-side, ignored)", alarm("ACTION:EMAIL", "TRIGGER:-P1D", "SUMMARY:s", "DESCRIPTION:d"), false, ""},
		{"lower-case email", alarm("ACTION:email", "TRIGGER:-P1D"), false, ""},
		{"lower-case action", alarm("ACTION:display", "TRIGGER:-PT5M"), false, "1/-PT5M"},
		{"procedure", alarm("ACTION:PROCEDURE", "TRIGGER:-PT15M"), false, ""},
		{"no action", alarm("TRIGGER:-PT15M"), false, ""},
		{"no trigger", alarm("ACTION:DISPLAY"), false, ""},
		{"two triggers", alarm("ACTION:DISPLAY", "TRIGGER:-PT15M", "TRIGGER:-PT5M"), false, ""},
		{"absolute", display("TRIGGER;VALUE=DATE-TIME:20261105T090000Z"), false, ""},
		{"absolute type, duration value", display("TRIGGER;VALUE=DATE-TIME:-PT15M"), false, ""},
		{"related end", display("TRIGGER;RELATED=END:-PT5M"), false, ""},
		{"related start", display("TRIGGER;RELATED=START:-PT5M"), false, "1/-PT5M"},
		{"value duration", display("TRIGGER;VALUE=DURATION:-PT5M"), false, "1/-PT5M"},
		{"after start", display("TRIGGER:PT5M"), false, ""},
		{"after start by seconds", display("TRIGGER:PT30S"), false, ""},
		{"at start", display("TRIGGER:PT0S"), false, "1/PT0S"},
		{"at start, negative zero", display("TRIGGER:-PT0S"), false, "1/PT0S"},
		{"all-day, same day 09:00", display("TRIGGER:PT9H"), true, "1/PT9H"},
		{"all-day, 23:59", display("TRIGGER:PT23H59M"), true, "1/PT23H59M"},
		{"all-day, 24 hours after", display("TRIGGER:PT24H"), true, ""},
		{"all-day, a day after", display("TRIGGER:P1D"), true, ""},
		{"all-day, day before 15:00", display("TRIGGER:-PT9H"), true, "1/-PT9H"},
		{"all-day, mixed", display("TRIGGER:-P1DT15H"), true, "1/-P1DT15H"},
		{"seconds forced to 0", display("TRIGGER:-PT15M30S"), false, "1/-PT15M"},
		{"folded to hours", display("TRIGGER:-PT60M"), false, "1/-PT1H"},
		{"folded to days", display("TRIGGER:-PT1440M"), false, "1/-P1D"},
		{"folded to weeks", display("TRIGGER:-P7D"), false, "1/-P1W"},
		{"mixed timed", display("TRIGGER:-P1DT1H"), false, "1/-PT25H"},
		{"999 weeks", display("TRIGGER:-P999W"), false, "1/-P999W"},
		{"1000 weeks", display("TRIGGER:-P1000W"), false, ""},
		{"6999 days", display("TRIGGER:-P6999D"), false, "1/-P6999D"},
		{"7001 days", display("TRIGGER:-P7001D"), false, ""},
		{"999 hours", display("TRIGGER:-PT999H"), false, "1/-PT999H"},
		{"1000 hours", display("TRIGGER:-PT1000H"), false, ""},
		{"9999 minutes", display("TRIGGER:-PT9999M"), false, "1/-PT9999M"},
		{"10000 minutes", display("TRIGGER:-PT10000M"), false, ""},
		{"all-day 6999 days", display("TRIGGER:-P6999DT1H"), true, "1/-P6999DT1H"},
		{"all-day 7001 days", display("TRIGGER:-P7001DT1H"), true, ""},
		{"not a duration", display("TRIGGER:15M"), false, ""},
		{"bare T", display("TRIGGER:-PT"), false, ""},
		{"weeks and days", display("TRIGGER:-P1W2D"), false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, dropped := NotificationsFromAlarms([]Component{c.alarm}, c.allDay)
			if c.want == "" {
				if len(got) != 0 || dropped != 1 {
					t.Fatalf("kept %+v, dropped %d; want it dropped", got, dropped)
				}
				return
			}
			if dropped != 0 || len(got) != 1 {
				t.Fatalf("got %+v, dropped %d", got, dropped)
			}
			typ, trigger, _ := strings.Cut(c.want, "/")
			if wantType := map[string]int{"0": 0, "1": 1}[typ]; got[0].Type != wantType || got[0].Trigger != trigger {
				t.Fatalf("got %+v, want %s", got[0], c.want)
			}
		})
	}
}

func TestNotificationsAtMostTen(t *testing.T) {
	var alarms []Component
	for i := 1; i <= 12; i++ {
		alarms = append(alarms, display(fmt.Sprintf("TRIGGER:-PT1%dM", i%10)))
	}
	alarms = append([]Component{alarm("ACTION:PROCEDURE", "TRIGGER:-PT1M")}, alarms...)
	got, dropped := NotificationsFromAlarms(alarms, false)
	if len(got) != 10 || dropped != 3 {
		t.Fatalf("kept %d, dropped %d; want 10 and 3", len(got), dropped)
	}
	if got[0].Trigger != "-PT11M" || got[9].Trigger != "-PT10M" {
		t.Fatalf("not the first ten valid ones in order: %+v", got)
	}
	none, dropped := NotificationsFromAlarms(nil, false)
	if none == nil || len(none) != 0 || dropped != 0 {
		t.Fatalf("no alarm: %#v, %d; want an empty non-nil list ([] on the wire)", none, dropped)
	}
	if b, _ := json.Marshal(none); string(b) != "[]" {
		t.Fatalf("no alarm marshals to %s", b)
	}
	if b, _ := json.Marshal(got[:1]); string(b) != `[{"Type":1,"Trigger":"-PT11M"}]` {
		t.Fatalf("wire form %s", b)
	}
}

func TestSameNotificationsComparesByMeaning(t *testing.T) {
	n := func(typ int, trigger string) Notification { return Notification{Type: typ, Trigger: trigger} }
	cases := []struct {
		name string
		a, b []Notification
		same bool
	}{
		{"order and form ignored", []Notification{n(1, "-PT15M"), n(0, "-P1D")}, []Notification{n(0, "-PT24H"), n(1, "-PT900S")}, true},
		{"zero forms", []Notification{n(1, "-PT0S")}, []Notification{n(1, "PT0S")}, true},
		{"weeks and days", []Notification{n(1, "-P1W")}, []Notification{n(1, "-P7D")}, true},
		{"type differs", []Notification{n(1, "-PT15M")}, []Notification{n(0, "-PT15M")}, false},
		{"offset differs", []Notification{n(1, "-PT15M")}, []Notification{n(1, "-PT16M")}, false},
		{"sign differs", []Notification{n(1, "-PT9H")}, []Notification{n(1, "PT9H")}, false},
		{"length differs", []Notification{n(1, "-PT15M")}, []Notification{n(1, "-PT15M"), n(1, "-PT15M")}, false},
		{"multiset", []Notification{n(1, "-PT15M"), n(1, "-PT15M")}, []Notification{n(1, "-PT15M"), n(1, "-PT30M")}, false},
		{"unreadable", []Notification{n(1, "soon")}, []Notification{n(1, "soon")}, false},
		{"both empty", nil, []Notification{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SameNotifications(c.a, c.b); got != c.same {
				t.Fatalf("SameNotifications = %v, want %v", got, c.same)
			}
			if got := SameNotifications(c.b, c.a); got != c.same {
				t.Fatalf("not symmetric")
			}
		})
	}
}

// The comparison half of "unchanged default alarms keep null": the VALARMs
// our render makes from the calendar defaults, read back, equal the
// defaults' device part (their email part is Proton-side, not served).
func TestRenderedDefaultsReadBackAsTheDefaults(t *testing.T) {
	for _, c := range []struct {
		defaults string
		allDay   bool
	}{
		{`[{"Type":1,"Trigger":"-PT15M"},{"Type":0,"Trigger":"-PT1H"}]`, false},
		{`[{"Type":1,"Trigger":"-PT15H"},{"Type":0,"Trigger":"-P1DT0H0M0S"}]`, true},
	} {
		rendered, err := Alarms(json.RawMessage("null"), json.RawMessage(c.defaults), c.allDay)
		if err != nil {
			t.Fatal(err)
		}
		got, dropped := NotificationsFromAlarms(rendered, c.allDay)
		var defaults []Notification
		if err := json.Unmarshal([]byte(c.defaults), &defaults); err != nil {
			t.Fatal(err)
		}
		device, email := SplitEmail(defaults)
		if len(email) != 1 || len(device) != 1 {
			t.Fatalf("test bug: defaults %s", c.defaults)
		}
		if dropped != 0 || !SameNotifications(got, device) {
			t.Fatalf("defaults %s read back as %+v (dropped %d)", c.defaults, got, dropped)
		}
		changed := slices.Clone(got)
		changed[0].Trigger = "-PT20M"
		if SameNotifications(changed, device) {
			t.Fatalf("an edited alarm still equals the defaults")
		}
	}
}
