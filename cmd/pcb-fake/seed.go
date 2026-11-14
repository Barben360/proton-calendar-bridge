package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/seed"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// The data set's layouts (testdata/seed/dataset.yaml) and iCalendar's.
const (
	seedTimed = "2006-01-02 15:04"
	seedDate  = "2006-01-02"
	icalLocal = "20060102T150405"
	icalUTC   = "20060102T150405Z"
	icalDate  = "20060102"
	dtstamp   = "DTSTAMP:20261101T000000Z"
)

// uidDomain ends every seeded UID, so a rehearsal tells seeded events from
// the ones it writes.
const uidDomain = "@pcb-fake.invalid"

// seeder is what seeding needs of the fake (a *protontest.Fake).
type seeder interface {
	AddCalendar(username string, c protontest.Calendar) string
	SetDefaultCalendar(username, id string)
	AddEvent(calID string, e protontest.Event) string
}

// seeded counts what a seeding added.
type seeded struct {
	calendars, events int
	perCalendar       []calendarCount // in the order added
}

// calendarCount is one seeded calendar: its objects (UIDs, as CalDAV
// serves them) and rows (a series' exceptions are rows of their own).
type calendarCount struct {
	name          string
	isDefault     bool
	objects, rows int
}

// add counts a row of uid in calendar i.
func (n *seeded) add(i int, uid string, uids map[string]bool) {
	n.events++
	n.perCalendar[i].rows++
	if !uids[uid] {
		uids[uid] = true
		n.perCalendar[i].objects++
	}
}

// colours maps the data set's colour names to hex values like Proton's.
var colours = map[string]string{
	"strawberry": "#EC3E7C", "pacific": "#0F87D1", "fern": "#3CBB3A", "carrot": "#F78400",
	"purple": "#8080FF", "reef": "#1DA583", "cobalt": "#415DF0",
}

// seedDataset adds the data set's calendars and events (DATASET.md: timed,
// all-day, series with one edited and one deleted occurrence, text cases)
// for username, then what the data set lacks (decision of the rehearsal
// plan, task R2): an event with attendees and a calendar shared with the
// account read-only.
func seedDataset(f seeder, username string, ds seed.Dataset) (seeded, error) {
	var n seeded
	type calendar struct {
		id string
		i  int // in n.perCalendar
	}
	cals := map[string]calendar{} // data set key → calendar
	uids := map[string]bool{}
	def := calendar{i: -1}
	addCalendar := func(c protontest.Calendar, isDefault bool) calendar {
		cal := calendar{f.AddCalendar(username, c), len(n.perCalendar)}
		n.calendars++
		n.perCalendar = append(n.perCalendar, calendarCount{name: c.Name, isDefault: isDefault})
		if isDefault {
			f.SetDefaultCalendar(username, cal.id)
			def = cal
		}
		return cal
	}
	for _, c := range ds.Calendars {
		cals[c.Key] = addCalendar(protontest.Calendar{Name: c.Name, Color: colours[c.Color]}, c.Default)
	}
	for _, e := range ds.Expand() {
		evs, err := eventsOf(e)
		if err != nil {
			return seeded{}, fmt.Errorf("data set entry %s: %w", e.ID, err)
		}
		cal := cals[e.Calendar]
		for _, ev := range evs {
			f.AddEvent(cal.id, ev)
			n.add(cal.i, ev.UID, uids)
		}
	}
	if def.i < 0 {
		return seeded{}, fmt.Errorf("the data set has no default calendar")
	}
	m := meeting(username)
	f.AddEvent(def.id, m)
	n.add(def.i, m.UID, uids)
	shared := addCalendar(protontest.Calendar{
		Name: "Shared with me (view only)", Color: "#DB60D6",
		// Every bit but Write (8, research H), another member listed first:
		// a calendar someone else shares with the account, read-only.
		Permissions: 127 &^ 8, ForeignMemberFirst: true,
	}, false)
	for i, title := range []string{"Team offsite", "Quarterly planning", "Release party"} {
		start := time.Date(2027, time.January, 11+7*i, 9, 0, 0, 0, time.UTC)
		uid := fmt.Sprintf("pcb-fake-shared-%d%s", i+1, uidDomain)
		f.AddEvent(shared.id, protontest.Event{UID: uid, Start: start.Unix(), End: start.Add(2 * time.Hour).Unix(),
			Card: card(uid, "DTSTART:"+start.Format(icalUTC), "DTEND:"+start.Add(2*time.Hour).Format(icalUTC),
				"SUMMARY:"+escape("pcb-fake shared "+title), "SEQUENCE:0")})
		n.add(shared.i, uid, uids)
	}
	return n, nil
}

// meeting is an event the account organises with two attendees, one of
// whom has accepted.
func meeting(username string) protontest.Event {
	start := time.Date(2027, time.January, 13, 14, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	uid := "pcb-fake-meeting" + uidDomain
	return protontest.Event{UID: uid, Start: start.Unix(), End: end.Unix(),
		Card: card(uid, "DTSTART;TZID=Europe/Paris:"+start.In(paris()).Format(icalLocal),
			"DTEND;TZID=Europe/Paris:"+end.In(paris()).Format(icalLocal),
			"SUMMARY:pcb-fake meeting with attendees", "SEQUENCE:0",
			"ORGANIZER;CN=Fake Organizer:mailto:"+username+"@proton.local",
			"ATTENDEE;CN=Lea;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED;X-PM-TOKEN=pcbfaketoken1:mailto:lea@example.org",
			"ATTENDEE;CN=Sam;ROLE=OPT-PARTICIPANT;PARTSTAT=NEEDS-ACTION;X-PM-TOKEN=pcbfaketoken2:mailto:sam@example.org")}
}

func paris() *time.Location {
	l, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		return time.UTC
	}
	return l
}

// eventsOf turns one data set entry into its rows: the event, and for a
// series with an edited occurrence its exception (the deleted occurrence
// is an EXDATE of the master).
func eventsOf(e seed.Entry) ([]protontest.Event, error) {
	loc, err := time.LoadLocation(tzOf(e))
	if err != nil {
		return nil, err
	}
	uid := "pcb-seed-" + e.ID + uidDomain
	start, end, err := span(e, e.Start, e.End, loc)
	if err != nil {
		return nil, err
	}
	lines := []string{prop("DTSTART", start, e.AllDay, loc), prop("DTEND", end, e.AllDay, loc),
		"SUMMARY:" + escape(seed.Prefix+" "+e.ID+" "+e.Title)}
	if e.Description != "" {
		lines = append(lines, "DESCRIPTION:"+escape(e.Description))
	}
	if e.Location != "" {
		lines = append(lines, "LOCATION:"+escape(e.Location))
	}
	rrule := rruleOf(e, loc)
	if rrule != "" {
		lines = append(lines, "RRULE:"+rrule)
	}
	if e.Delete != nil {
		at, err := parseIn(e.Delete.Occurrence, e.AllDay, loc)
		if err != nil {
			return nil, err
		}
		lines = append(lines, prop("EXDATE", at, e.AllDay, loc))
	}
	lines = append(lines, "SEQUENCE:0")
	notes, err := notifications(e.Reminders)
	if err != nil {
		return nil, err
	}
	master := protontest.Event{UID: uid, Start: start.Unix(), End: end.Unix(), FullDay: e.AllDay, RRule: rrule,
		Color: colours[e.Color], Notifications: notes, Card: card(uid, lines...)}
	out := []protontest.Event{master}
	if e.Edit != nil {
		orig, err := parseIn(e.Edit.Occurrence, e.AllDay, loc)
		if err != nil {
			return nil, err
		}
		newEnd := e.Edit.End
		s, en, err := span(e, e.Edit.Start, newEnd, loc)
		if err != nil {
			return nil, err
		}
		if newEnd == "" && !e.AllDay {
			en = s.Add(end.Sub(start)) // the master's duration
		}
		rid := orig.Unix()
		out = append(out, protontest.Event{UID: uid, Start: s.Unix(), End: en.Unix(), FullDay: e.AllDay, RecurrenceID: &rid,
			Color: colours[e.Color], Notifications: notes,
			Card: card(uid, prop("RECURRENCE-ID", orig, e.AllDay, loc), prop("DTSTART", s, e.AllDay, loc), prop("DTEND", en, e.AllDay, loc),
				"SUMMARY:"+escape(seed.Prefix+" "+e.ID+".e "+e.Title+" (edited)"), "SEQUENCE:0")})
	}
	return out, nil
}

func tzOf(e seed.Entry) string {
	if e.TZ == "" || e.AllDay {
		return "UTC"
	}
	return e.TZ
}

// span is the start and the end of an entry: a timed end defaults to an
// hour after its start, an all-day end (inclusive in the data set) to the
// start day; the end returned is exclusive for all-day events.
func span(e seed.Entry, from, to string, loc *time.Location) (start, end time.Time, err error) {
	if start, err = parseIn(from, e.AllDay, loc); err != nil {
		return
	}
	switch {
	case to == "" && e.AllDay:
		return start, start.AddDate(0, 0, 1), nil
	case to == "":
		return start, start.Add(time.Hour), nil
	}
	if end, err = parseIn(to, e.AllDay, loc); err != nil {
		return
	}
	if e.AllDay {
		end = end.AddDate(0, 0, 1)
	}
	return start, end, nil
}

func parseIn(v string, allDay bool, loc *time.Location) (time.Time, error) {
	if allDay {
		return time.ParseInLocation(seedDate, v, time.UTC)
	}
	return time.ParseInLocation(seedTimed, v, loc)
}

// prop is a date-time property in the entry's form: a DATE, a UTC time
// for UTC entries, else a local time with its TZID.
func prop(name string, t time.Time, allDay bool, loc *time.Location) string {
	switch {
	case allDay:
		return name + ";VALUE=DATE:" + t.Format(icalDate)
	case loc == time.UTC || loc.String() == "UTC":
		return name + ":" + t.UTC().Format(icalUTC)
	}
	return name + ";TZID=" + loc.String() + ":" + t.In(loc).Format(icalLocal)
}

// rruleOf is the entry's RRULE: its own, or one built from repeat, every,
// count and until (until: the end of that day, in UTC for a timed series).
func rruleOf(e seed.Entry, loc *time.Location) string {
	if e.RRule != "" {
		return e.RRule
	}
	if e.Repeat == "" {
		return ""
	}
	parts := []string{"FREQ=" + strings.ToUpper(e.Repeat)}
	if e.Every > 1 {
		parts = append(parts, fmt.Sprintf("INTERVAL=%d", e.Every))
	}
	if e.Count > 0 {
		parts = append(parts, fmt.Sprintf("COUNT=%d", e.Count))
	}
	if e.Until != "" {
		if day, err := time.ParseInLocation(seedDate, e.Until, loc); err == nil {
			if e.AllDay {
				parts = append(parts, "UNTIL="+day.Format(icalDate))
			} else {
				parts = append(parts, "UNTIL="+day.AddDate(0, 0, 1).Add(-time.Second).UTC().Format(icalUTC))
			}
		}
	}
	return strings.Join(parts, ";")
}

// notifications maps the data set's reminders: none is [], an empty list
// the calendar's defaults (null), 15m, 1d and email:1h Proton's
// notifications (type 1 a device notification, 0 an email).
func notifications(rs []string) ([]protontest.Notification, error) {
	if len(rs) == 0 {
		return nil, nil
	}
	out := []protontest.Notification{}
	for _, r := range rs {
		typ := 1
		if v, ok := strings.CutPrefix(r, "email:"); ok {
			typ, r = 0, v
		}
		var trigger string
		switch r {
		case "none":
			continue
		case "15m":
			trigger = "-PT15M"
		case "1h":
			trigger = "-PT1H"
		case "1d":
			trigger = "-P1D"
		default:
			return nil, fmt.Errorf("unknown reminder %q", r)
		}
		out = append(out, protontest.Notification{Type: typ, Trigger: trigger})
	}
	return out, nil
}

// card is a VEVENT body: UID, DTSTAMP, then lines, each folded at 75
// octets as RFC 5545 asks.
func card(uid string, lines ...string) string {
	all := append([]string{"UID:" + uid, dtstamp}, lines...)
	for i, l := range all {
		all[i] = fold(l)
	}
	return strings.Join(all, "\r\n")
}

// escape is s as an iCalendar TEXT value.
func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\n", `\n`).Replace(s)
}

// fold splits l into lines of at most 75 octets, never inside a UTF-8
// sequence; each continuation starts with a space.
func fold(l string) string {
	var b strings.Builder
	limit := 75
	for len(l) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(l[cut]) {
			cut--
		}
		b.WriteString(l[:cut])
		b.WriteString("\r\n ")
		l = l[cut:]
		limit = 74 // the leading space counts
	}
	b.WriteString(l)
	return b.String()
}
