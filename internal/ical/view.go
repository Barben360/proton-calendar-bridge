package ical

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Time is a DATE or DATE-TIME value of an event, in one of four forms:
//   - all-day (VALUE=DATE): Date is "YYYY-MM-DD", At is zero, TZID empty;
//   - UTC ("…Z"): At is the instant, in UTC, TZID empty;
//   - zoned (TZID): At is the instant the wall clock means in that zone,
//     kept in the zone's location (it prints with the local offset), and
//     TZID is the IANA name (Thunderbird's "/mozilla.org/…/" prefix removed);
//   - floating (neither Z nor TZID): Floating is true and At holds the wall
//     clock as if it were UTC. That is a placeholder, not an instant: a
//     floating time means the same wall clock in whatever zone it is read,
//     so a reader picks the zone and reads At's fields, never its instant.
type Time struct {
	At       time.Time // instant (zero for a date)
	Date     string    // YYYY-MM-DD for an all-day value
	TZID     string
	Floating bool
}

// Person is an ORGANIZER or ATTENDEE: CN, the address without "mailto:",
// ROLE and PARTSTAT (Status), the last two upper-cased.
type Person struct{ Name, Email, Role, Status string } // Status: PARTSTAT

// Alarm is a VALARM whose TRIGGER is a duration relative to the start:
// MinutesBefore is positive before the start, negative after (an all-day
// event's "on the day at 09:00" is -540). Action is ACTION upper-cased. A
// VALARM with an absolute trigger, one relative to the end or one that is
// not whole minutes is not an Alarm: it is counted in EventData.Unknown.
type Alarm struct {
	MinutesBefore int
	Action        string
}

// EventData is the decoded view of one VEVENT. Text values are unescaped
// (UnescapeText); URL and RRULE are as written; Status and Transp are
// upper-cased. End comes from DTEND, else from DURATION (weeks and days
// nominal, in the start's zone), else is the start's next day for a date
// and the start itself for a time (RFC 5545 §3.6.1).
//
// Unknown counts what the view does not show: every property not listed
// here, a repeat of a single-valued one, a VALARM that is no Alarm and any
// other nested component. DTSTAMP, SEQUENCE, CREATED and LAST-MODIFIED are
// bookkeeping, neither shown nor counted. The VALARMs' own properties are
// not counted.
type EventData struct {
	UID, Summary, Description, Location, URL string
	Status, Transp                           string
	Start, End                               Time
	RRule                                    string
	ExDates                                  []Time
	Alarms                                   []Alarm
	Organizer                                *Person
	Attendees                                []Person
	RecurrenceID                             *Time
	Overrides                                []EventData // on the master only
	Unknown                                  int         // properties not listed above
}

// bookkeeping are the change-management properties every rendered VEVENT
// carries: not part of the view, and not counted as unknown.
var bookkeeping = map[string]bool{"DTSTAMP": true, "SEQUENCE": true, "CREATED": true, "LAST-MODIFIED": true}

// viewSingle are the single-valued properties the view shows: the first one
// is decoded, a repeat is counted in Unknown.
var viewSingle = map[string]bool{
	"UID": true, "SUMMARY": true, "DESCRIPTION": true, "LOCATION": true, "URL": true, "STATUS": true, "TRANSP": true,
	"DTSTART": true, "DTEND": true, "DURATION": true, "RRULE": true, "RECURRENCE-ID": true, "ORGANIZER": true,
}

// DecodeEvent decodes a rendered object (one VCALENDAR: master and
// overrides, as service.Object.Data holds it).
//
// The VEVENT without RECURRENCE-ID is the master and the others are its
// Overrides, in source order. With no master, a single overridden
// occurrence (one occurrence of a series the account was invited to)
// stands for the event, with its RecurrenceID set. Two masters, several
// occurrences without a master, no VEVENT or a VEVENT without DTSTART are
// errors. Errors name a property or a line number, never content.
func DecodeEvent(vcal []byte) (EventData, error) {
	comps, err := Parse(vcal)
	if err != nil {
		return EventData{}, err
	}
	if len(comps) != 1 || !strings.EqualFold(comps[0].Name, "VCALENDAR") {
		return EventData{}, errors.New("ical: an event object is one VCALENDAR")
	}
	var master *EventData
	var overrides []EventData
	for i, ev := range comps[0].Subs("VEVENT") {
		d, err := decodeVEvent(ev)
		if err != nil {
			return EventData{}, fmt.Errorf("ical: VEVENT %d: %w", i+1, err)
		}
		switch {
		case d.RecurrenceID != nil:
			overrides = append(overrides, d)
		case master != nil:
			return EventData{}, errors.New("ical: two VEVENTs without RECURRENCE-ID")
		default:
			master = &d
		}
	}
	switch {
	case master != nil:
		master.Overrides = overrides
		return *master, nil
	case len(overrides) == 1:
		return overrides[0], nil
	case len(overrides) == 0:
		return EventData{}, errors.New("ical: no VEVENT")
	}
	return EventData{}, errors.New("ical: several occurrences without their master")
}

// decodeVEvent is the view of one VEVENT, Overrides left empty.
func decodeVEvent(ev Component) (EventData, error) {
	var d EventData
	var dtend, duration *Property
	seen := map[string]bool{}
	hasStart := false
	for _, it := range ev.Items {
		if it.Comp != nil {
			if a, ok := alarmOf(*it.Comp); ok {
				d.Alarms = append(d.Alarms, a)
			} else {
				d.Unknown++
			}
			continue
		}
		p := it.Prop
		name := strings.ToUpper(p.Name)
		if viewSingle[name] {
			if seen[name] {
				d.Unknown++
				continue
			}
			seen[name] = true
		}
		var err error
		switch name {
		case "UID":
			d.UID = UnescapeText(p.Value)
		case "SUMMARY":
			d.Summary = UnescapeText(p.Value)
		case "DESCRIPTION":
			d.Description = UnescapeText(p.Value)
		case "LOCATION":
			d.Location = UnescapeText(p.Value)
		case "URL":
			d.URL = p.Value
		case "STATUS":
			d.Status = strings.ToUpper(p.Value)
		case "TRANSP":
			d.Transp = strings.ToUpper(p.Value)
		case "RRULE":
			d.RRule = p.Value
		case "DTSTART":
			d.Start, err = decodeTime(p, p.Value)
			hasStart = true
		case "DTEND":
			dtend = &p
		case "DURATION":
			duration = &p
		case "RECURRENCE-ID":
			var t Time
			t, err = decodeTime(p, p.Value)
			d.RecurrenceID = &t
		case "EXDATE":
			for v := range strings.SplitSeq(p.Value, ",") {
				var t Time
				if t, err = decodeTime(p, v); err != nil {
					break
				}
				d.ExDates = append(d.ExDates, t)
			}
		case "ORGANIZER":
			d.Organizer = new(personOf(p))
		case "ATTENDEE":
			d.Attendees = append(d.Attendees, personOf(p))
		default:
			if !bookkeeping[name] {
				d.Unknown++
			}
		}
		if err != nil {
			return EventData{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	if !hasStart {
		return EventData{}, errors.New("no DTSTART")
	}
	var err error
	switch {
	case dtend != nil:
		if d.End, err = decodeTime(*dtend, dtend.Value); err != nil {
			return EventData{}, fmt.Errorf("DTEND: %w", err)
		}
	case duration != nil:
		if d.End, err = endAfter(d.Start, duration.Value); err != nil {
			return EventData{}, fmt.Errorf("DURATION: %w", err)
		}
	case d.Start.Date != "":
		d.End, _ = endAfter(d.Start, "P1D")
	default:
		d.End = d.Start
	}
	return d, nil
}

// decodeTime reads value as the DATE or DATE-TIME property p says. Unlike
// parseWhen (writes), a floating time is a value, not a refusal.
func decodeTime(p Property, value string) (Time, error) {
	valueType, _ := param(p.Params, "VALUE")
	tzid, hasTZ := param(p.Params, "TZID")
	switch strings.ToUpper(valueType) {
	case "DATE":
		t, err := time.Parse(dateLayout, value)
		if err != nil || len(value) != 8 {
			return Time{}, errors.New("a malformed DATE")
		}
		return Time{Date: t.Format(time.DateOnly)}, nil
	case "", "DATE-TIME":
	default:
		return Time{}, errors.New("an unsupported VALUE")
	}
	if len(value) == 16 && value[15] == 'Z' {
		t, err := time.Parse(localLayout, value[:15])
		if err != nil {
			return Time{}, errors.New("a malformed DATE-TIME")
		}
		return Time{At: t}, nil
	}
	if len(value) != 15 {
		return Time{}, errors.New("a malformed DATE-TIME")
	}
	if !hasTZ {
		t, err := time.Parse(localLayout, value)
		if err != nil {
			return Time{}, errors.New("a malformed DATE-TIME")
		}
		return Time{At: t, Floating: true}, nil
	}
	loc, name, ok := location(tzid)
	if !ok {
		return Time{}, errors.New("a TZID that is not a known time zone")
	}
	t, err := parseWall(value, loc)
	if err != nil {
		return Time{}, errors.New("a malformed DATE-TIME")
	}
	return Time{At: t, TZID: name}, nil
}

// endAfter is start plus the DURATION value dur: weeks and days nominal (on
// start's calendar, in its zone), hours, minutes and seconds exact. A date
// takes whole days only; a negative duration is an error.
func endAfter(start Time, dur string) (Time, error) {
	d, ok := parseDuration(dur)
	if !ok || d.neg {
		return Time{}, errors.New("not a positive duration")
	}
	days := int(7*d.weeks + d.days)
	exact := time.Duration(d.hours)*time.Hour + time.Duration(d.minutes)*time.Minute + time.Duration(d.seconds)*time.Second
	if start.Date != "" {
		if exact != 0 {
			return Time{}, errors.New("a duration in hours for a date")
		}
		t, err := time.Parse(time.DateOnly, start.Date)
		if err != nil {
			return Time{}, errors.New("a malformed DATE")
		}
		return Time{Date: t.AddDate(0, 0, days).Format(time.DateOnly)}, nil
	}
	end := start
	end.At = start.At.AddDate(0, 0, days).Add(exact)
	return end, nil
}

// alarmOf is the Alarm of a VALARM whose TRIGGER is whole minutes relative
// to the start; false for anything else (another component included).
func alarmOf(c Component) (Alarm, bool) {
	if !strings.EqualFold(c.Name, "VALARM") {
		return Alarm{}, false
	}
	trig, ok := c.Get("TRIGGER")
	if !ok {
		return Alarm{}, false
	}
	if v, _ := param(trig.Params, "VALUE"); v != "" && !strings.EqualFold(v, "DURATION") {
		return Alarm{}, false
	}
	if r, _ := param(trig.Params, "RELATED"); r != "" && !strings.EqualFold(r, "START") {
		return Alarm{}, false
	}
	d, ok := parseDuration(trig.Value)
	if !ok || d.signed()%60 != 0 {
		return Alarm{}, false
	}
	a := Alarm{MinutesBefore: int(-d.signed() / 60)}
	if act, ok := c.Get("ACTION"); ok {
		a.Action = strings.ToUpper(act.Value)
	}
	return a, true
}

// personOf reads an ORGANIZER or ATTENDEE line.
func personOf(p Property) Person {
	cn, _ := param(p.Params, "CN")
	role, _ := param(p.Params, "ROLE")
	status, _ := param(p.Params, "PARTSTAT")
	email := strings.TrimSpace(p.Value)
	if len(email) >= 7 && strings.EqualFold(email[:7], "mailto:") {
		email = email[7:]
	}
	return Person{Name: cn, Email: email, Role: strings.ToUpper(role), Status: strings.ToUpper(status)}
}
