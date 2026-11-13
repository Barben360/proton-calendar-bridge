package ical

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Scope says what an edit of a recurring event changes: the series (its
// master) or one occurrence (its override). "This and following" is not
// offered (design §5.6).
type Scope string

// The edit scopes.
const (
	ScopeSeries     Scope = "series"
	ScopeOccurrence Scope = "occurrence"
)

// Fields is an edit: nil leaves a field as it is; a pointer to "" clears a text field.
//
// Text is given unescaped (NewEvent and ApplyEdit escape it). Start and End
// are a zoned time, a UTC instant or an all-day date (Time); a floating
// time is refused, as Proton's write path refuses it. RRule is in Proton's
// set (checkRRule), written in Proton's form (a MONTHLY ordinal BYDAY as
// BYSETPOS, a DAILY BYDAY as WEEKLY). Alarms are minutes before the start
// (negative after it, as an all-day event's "on the day at 09:00", -540).
type Fields struct {
	Summary, Description, Location *string
	Start, End                     *Time
	RRule                          *string // "" removes the recurrence
	Alarms                         *[]int  // minutes before; replaces every relative alarm
	Busy                           *bool   // TRANSP OPAQUE / TRANSPARENT
}

// SeriesHasExceptions refuses a series edit that would leave an EXDATE or
// an override's RECURRENCE-ID at a time the new series no longer produces
// (design §5.6): nothing is dropped silently.
const SeriesHasExceptions RefusalCode = "pcb:series-has-exceptions"

// NewEvent is a new event's object: one VCALENDAR (as ical.Object writes
// it) holding one VEVENT with uid, DTSTAMP now and SEQUENCE:0, and the
// fields. It needs Summary, Start and End. Errors are *Refusal.
func NewEvent(uid string, f Fields, now time.Time) ([]byte, error) {
	if uid == "" || hasControl(uid) {
		return nil, refuse(BadData, "a new event needs a UID without control characters")
	}
	if f.Summary == nil || *f.Summary == "" || f.Start == nil || f.End == nil {
		return nil, refuse(BadData, "a new event needs a summary, a start and an end")
	}
	ev := Component{Name: "VEVENT"}
	ev.add(Property{Name: "UID", Value: uid})
	ev.add(Property{Name: "DTSTAMP", Value: dtstamp(now)})
	ev.add(Property{Name: "SEQUENCE", Value: "0"})
	if r := applyFields(&ev, f, true); r != nil {
		return nil, r
	}
	cal := Component{Name: "VCALENDAR"}
	cal.add(Property{Name: "VERSION", Value: "2.0"})
	cal.add(Property{Name: "PRODID", Value: prodID})
	cal.add(Property{Name: "CALSCALE", Value: "GREGORIAN"})
	cal.Items = append(cal.Items, Item{Comp: &ev})
	return cal.Bytes(), nil
}

// ApplyEdit patches a stored object (one VCALENDAR: master and overrides,
// as service.Object.Data holds it) with f, and returns the whole object.
// It never rebuilds the object from its view: every line f does not name
// is kept as it is, other VEVENTs and nested components included.
//
// ScopeSeries edits the master (recurrenceID is ignored: an occurrence's
// id may name its series). A change that moves Start and End by the same
// wall-clock delta in their zone moves every EXDATE and override
// RECURRENCE-ID by that delta, the overrides' own times staying; a change
// of End alone moves nothing. Any other change of a series with
// exceptions that could leave one off the new rule (an RRULE change or
// removal, a Start moved without End or by another delta, a change of
// zone or between date and time) is refused, a *Refusal SeriesHasExceptions
// naming how many there are.
//
// ScopeOccurrence edits the override at recurrenceID, or makes it from the
// master (its lines without RRULE and EXDATE, alarms included, at that
// occurrence's start and with the series' duration); an RRULE is refused.
//
// Every VEVENT changed gets DTSTAMP now and its SEQUENCE bumped. The input
// is not modified. Errors are *Refusal, except a stored object that does
// not parse.
func ApplyEdit(vcal []byte, scope Scope, recurrenceID *Time, f Fields, now time.Time) ([]byte, error) {
	if r := checkGivenTimes(f); r != nil {
		return nil, r
	}
	cal, err := parseStored(vcal)
	if err != nil {
		return nil, err
	}
	switch scope {
	case ScopeSeries:
		master := storedMaster(cal)
		if master == nil {
			return nil, refuse(BadData, "the object holds one occurrence and no series: edit the occurrence")
		}
		if r := moveExceptions(cal, master, f, now); r != nil {
			return nil, r
		}
		if r := applyFields(master, f, true); r != nil {
			return nil, r
		}
		touchEdited(master, now)
	case ScopeOccurrence:
		if recurrenceID == nil {
			return nil, refuse(BadData, "an occurrence edit needs the occurrence's RECURRENCE-ID")
		}
		ev, r := overrideAt(cal, *recurrenceID)
		if r != nil {
			return nil, r
		}
		if r := applyFields(ev, f, false); r != nil {
			return nil, r
		}
		touchEdited(ev, now)
	default:
		return nil, refuse(BadData, "an edit scope other than series or occurrence")
	}
	return cal.Bytes(), nil
}

// DeleteOccurrence removes one occurrence of a series: an EXDATE at
// recurrenceID, in the form of the master's DTSTART, and the override at
// that RECURRENCE-ID if there is one. The master gets DTSTAMP now and its
// SEQUENCE bumped. An event that does not recur, an occurrence already
// deleted, before the series or of another value type than the series'
// start is refused (*Refusal BadData).
func DeleteOccurrence(vcal []byte, recurrenceID Time, now time.Time) ([]byte, error) {
	cal, err := parseStored(vcal)
	if err != nil {
		return nil, err
	}
	master := storedMaster(cal)
	if master == nil {
		return nil, refuse(BadData, "the object holds one occurrence and no series: delete the object")
	}
	start, r := seriesStart(*master, recurrenceID)
	if r != nil {
		return nil, r
	}
	dropped := false
	kept := cal.Items[:0:0]
	for _, it := range cal.Items {
		if rid, ok := overrideRID(it.Comp); ok && sameOccurrence(rid, recurrenceID) {
			dropped = true
			continue
		}
		kept = append(kept, it)
	}
	cal.Items = kept
	if hasExdate(*master, recurrenceID) {
		if !dropped {
			return nil, refuse(BadData, "the occurrence is already deleted")
		}
	} else {
		params, value := encodeLike(recurrenceID, start)
		at := lastPropIndex(*master, "EXDATE")
		if at < 0 {
			at = lastPropIndex(*master, "RRULE")
		}
		insertProp(master, at+1, Property{Name: "EXDATE", Params: params, Value: value})
	}
	touchEdited(master, now)
	return cal.Bytes(), nil
}

// parseStored parses a stored object: one VCALENDAR.
func parseStored(vcal []byte) (*Component, error) {
	comps, err := Parse(vcal)
	if err != nil {
		return nil, err
	}
	if len(comps) != 1 || !strings.EqualFold(comps[0].Name, "VCALENDAR") {
		return nil, errors.New("ical: an event object is one VCALENDAR")
	}
	return &comps[0], nil
}

// storedMaster is the first VEVENT without RECURRENCE-ID, nil if none.
func storedMaster(cal *Component) *Component {
	for _, it := range cal.Items {
		if it.Comp != nil && strings.EqualFold(it.Comp.Name, "VEVENT") {
			if _, ok := it.Comp.Get("RECURRENCE-ID"); !ok {
				return it.Comp
			}
		}
	}
	return nil
}

// storedOverrides are the VEVENTs with a RECURRENCE-ID.
func storedOverrides(cal *Component) []*Component {
	var out []*Component
	for _, it := range cal.Items {
		if _, ok := overrideRID(it.Comp); ok {
			out = append(out, it.Comp)
		}
	}
	return out
}

// overrideRID is c's RECURRENCE-ID when c is a VEVENT with one that decodes.
func overrideRID(c *Component) (Time, bool) {
	if c == nil || !strings.EqualFold(c.Name, "VEVENT") {
		return Time{}, false
	}
	p, ok := c.Get("RECURRENCE-ID")
	if !ok {
		return Time{}, false
	}
	t, err := decodeTime(p, p.Value)
	return t, err == nil
}

// sameOccurrence: the same date, or the same instant (a floating time: the same
// wall clock).
func sameOccurrence(a, b Time) bool {
	if (a.Date != "") != (b.Date != "") {
		return false
	}
	if a.Date != "" {
		return a.Date == b.Date
	}
	return a.At.Equal(b.At)
}

// startsBefore: a is strictly before b, both dates or both times.
func startsBefore(a, b Time) bool {
	if a.Date != "" {
		return a.Date < b.Date
	}
	return a.At.Before(b.At)
}

// hasExdate: an EXDATE of ev names t.
func hasExdate(ev Component, t Time) bool {
	for _, p := range ev.Props() {
		if !strings.EqualFold(p.Name, "EXDATE") {
			continue
		}
		for v := range strings.SplitSeq(p.Value, ",") {
			if x, err := decodeTime(p, v); err == nil && sameOccurrence(x, t) {
				return true
			}
		}
	}
	return false
}

// seriesStart is the DTSTART of a recurring master, after checking that
// rid can name one of its occurrences: the master recurs, rid is of its
// start's value type and not before it.
func seriesStart(master Component, rid Time) (Time, *Refusal) {
	if _, ok := master.Get("RRULE"); !ok {
		return Time{}, refuse(BadData, "the event does not recur")
	}
	p, ok := master.Get("DTSTART")
	if !ok {
		return Time{}, refuse(BadData, "the stored series has no DTSTART")
	}
	start, err := decodeTime(p, p.Value)
	if err != nil {
		return Time{}, refuse(BadData, "the stored series' DTSTART is malformed")
	}
	if (start.Date != "") != (rid.Date != "") {
		return Time{}, refuse(BadData, "a RECURRENCE-ID of another value type than the series' start")
	}
	if startsBefore(rid, start) {
		return Time{}, refuse(BadData, "a RECURRENCE-ID before the series' start")
	}
	return start, nil
}

// overrideAt is the override at rid: the stored one, or a new one made
// from the master and added after the last VEVENT.
func overrideAt(cal *Component, rid Time) (*Component, *Refusal) {
	for _, ov := range storedOverrides(cal) {
		if t, _ := overrideRID(ov); sameOccurrence(t, rid) {
			return ov, nil
		}
	}
	master := storedMaster(cal)
	if master == nil {
		return nil, refuse(BadData, "no occurrence at this RECURRENCE-ID")
	}
	start, r := seriesStart(*master, rid)
	if r != nil {
		return nil, r
	}
	if hasExdate(*master, rid) {
		return nil, refuse(BadData, "the occurrence is deleted (EXDATE)")
	}
	d, err := decodeVEvent(*master)
	if err != nil {
		return nil, refuse(BadData, "the stored series is malformed")
	}
	ov := master.Clone()
	ov.removeProps(func(p Property) bool {
		switch strings.ToUpper(p.Name) {
		case "RRULE", "EXDATE", "RDATE", "EXRULE":
			return true
		}
		return false
	})
	params, value := encodeLike(rid, start)
	replaceProp(&ov, Property{Name: "DTSTART", Params: params, Value: value}, "DTSTART")
	insertProp(&ov, indexOf(ov, "UID")+1, Property{Name: "RECURRENCE-ID", Params: params, Value: value})
	if p, ok := ov.Get("DTEND"); ok {
		end := Time{At: rid.At.Add(d.End.At.Sub(d.Start.At))}
		if rid.Date != "" {
			from, _ := time.Parse(time.DateOnly, d.Start.Date)
			to, _ := time.Parse(time.DateOnly, d.End.Date)
			at, _ := time.Parse(time.DateOnly, rid.Date)
			end = Time{Date: at.Add(to.Sub(from)).Format(time.DateOnly)}
		}
		form, err := decodeTime(p, p.Value)
		if err != nil {
			return nil, refuse(BadData, "the stored series' DTEND is malformed")
		}
		params, value := encodeLike(end, form)
		replaceProp(&ov, Property{Name: "DTEND", Params: params, Value: value}, "DTEND")
	}
	at := len(cal.Items)
	for i, it := range cal.Items {
		if it.Comp != nil && strings.EqualFold(it.Comp.Name, "VEVENT") {
			at = i + 1
		}
	}
	cal.Items = slices.Insert(cal.Items, at, Item{Comp: &ov})
	return &ov, nil
}

// moveExceptions applies design §5.6's rule for a series edit: with no
// exception, or a change that cannot orphan one, nothing to do; a shift
// of Start and End by one wall-clock delta moves them; anything else is
// refused.
func moveExceptions(cal *Component, master *Component, f Fields, now time.Time) *Refusal {
	overrides := storedOverrides(cal)
	n := len(overrides)
	for _, p := range master.Props() {
		if strings.EqualFold(p.Name, "EXDATE") {
			n += strings.Count(p.Value, ",") + 1
		}
	}
	if n == 0 {
		return nil
	}
	refusal := refuse(SeriesHasExceptions, fmt.Sprintf(
		"the series has %d exceptions (deleted or changed occurrences) this change would leave off its occurrences: edit them first, or change only its times by one shift", n))
	if f.RRule != nil {
		stored, _ := master.Get("RRULE")
		if !sameRule(stored.Value, *f.RRule) {
			return refusal
		}
	}
	if f.Start == nil {
		return nil // occurrences start where they did
	}
	d, err := decodeVEvent(*master)
	if err != nil {
		return refuse(BadData, "the stored series is malformed")
	}
	if !sameForm(d.Start, *f.Start) {
		return refusal
	}
	delta := shiftOf(d.Start, *f.Start)
	if delta == 0 {
		return nil
	}
	_, hasDTEnd := master.Get("DTEND")
	switch {
	case f.End == nil && hasDTEnd: // the start moves alone
		return refusal
	case f.End != nil && (!sameForm(d.End, *f.End) || shiftOf(d.End, *f.End) != delta):
		return refusal
	}
	shift := func(p Property, v string) (string, bool) {
		t, err := decodeTime(p, v)
		if err != nil {
			return "", false
		}
		moved, ok := shiftException(t, d.Start, delta)
		if !ok {
			return "", false
		}
		_, value := encodeLike(moved, t)
		return value, true
	}
	for i, it := range master.Items {
		if it.Comp != nil || !strings.EqualFold(it.Prop.Name, "EXDATE") {
			continue
		}
		values := strings.Split(it.Prop.Value, ",")
		for j, v := range values {
			var ok bool
			if values[j], ok = shift(it.Prop, v); !ok {
				return refusal
			}
		}
		master.Items[i].Prop.Value = strings.Join(values, ",")
	}
	for _, ov := range overrides {
		i := indexOf(*ov, "RECURRENCE-ID")
		v, ok := shift(ov.Items[i].Prop, ov.Items[i].Prop.Value)
		if !ok {
			return refusal
		}
		ov.Items[i].Prop.Value = v
		touchEdited(ov, now)
	}
	return nil
}

// sameForm: both dates, or both times in the same zone (or both UTC).
func sameForm(a, b Time) bool {
	return (a.Date != "") == (b.Date != "") && a.TZID == b.TZID && a.Floating == b.Floating
}

// wallAsUTC is t's wall clock in loc, as if it were UTC: deltas between them
// are wall-clock deltas, the same across a DST change.
func wallAsUTC(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}

// wallZone is t's zone for wall clocks: its TZID's, else UTC (a floating
// time's At already holds its wall clock as UTC).
func wallZone(t Time) *time.Location {
	if loc, _, ok := location(t.TZID); ok {
		return loc
	}
	return time.UTC
}

// shiftOf is b - a on the wall clock of their (same) form.
func shiftOf(a, b Time) time.Duration {
	if a.Date != "" {
		x, _ := time.Parse(time.DateOnly, a.Date)
		y, _ := time.Parse(time.DateOnly, b.Date)
		return y.Sub(x)
	}
	loc := wallZone(a)
	return wallAsUTC(b.At, loc).Sub(wallAsUTC(a.At, loc))
}

// shiftException moves t, an exception of the series starting at start, by
// delta on the series' wall clock: the occurrence it names moves as the
// series' occurrences do. false when t is not of start's value type.
func shiftException(t, start Time, delta time.Duration) (Time, bool) {
	if (t.Date != "") != (start.Date != "") || t.Floating != start.Floating {
		return Time{}, false
	}
	if t.Date != "" {
		d, err := time.Parse(time.DateOnly, t.Date)
		if err != nil {
			return Time{}, false
		}
		return Time{Date: d.Add(delta).Format(time.DateOnly)}, true
	}
	loc := wallZone(start)
	w := wallAsUTC(t.At, loc).Add(delta)
	t.At = time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), loc)
	return t, true
}

// encodeLike writes t in form's form: a DATE, a wall clock of form's zone,
// a floating wall clock, or UTC.
func encodeLike(t, form Time) (params, value string) {
	switch {
	case form.Date != "":
		return ";VALUE=DATE", strings.ReplaceAll(t.Date, "-", "")
	case form.Floating:
		return "", t.At.Format(localLayout)
	case form.TZID != "":
		return ";TZID=" + form.TZID, t.At.In(wallZone(form)).Format(localLayout)
	}
	return "", t.At.UTC().Format(localLayout) + "Z"
}

// timeProp is the property name set to t, refused when Proton could not
// store it: a floating time, an unknown zone, a malformed date, no instant.
func timeProp(name string, t Time) (Property, *Refusal) {
	switch {
	case t.Date != "":
		if d, err := time.Parse(time.DateOnly, t.Date); err != nil || d.Format(time.DateOnly) != t.Date {
			return Property{}, refuse(BadData, name+" is not a date YYYY-MM-DD")
		}
	case t.Floating:
		return Property{}, refuse(Floating, name+" is a floating time: give a zone or an offset")
	case t.At.IsZero():
		return Property{}, refuse(BadData, name+" has neither a date nor an instant")
	case t.TZID != "":
		if _, _, ok := location(t.TZID); !ok {
			return Property{}, refuse(UnknownTZID, name+" has a time zone that is not known")
		}
	}
	params, value := encodeLike(t, t)
	return Property{Name: name, Params: params, Value: value}, nil
}

// checkGivenTimes refuses a Start or End timeProp refuses, before any
// other rule looks at them.
func checkGivenTimes(f Fields) *Refusal {
	if f.Start != nil {
		if _, r := timeProp("DTSTART", *f.Start); r != nil {
			return r
		}
	}
	if f.End != nil {
		if _, r := timeProp("DTEND", *f.End); r != nil {
			return r
		}
	}
	return nil
}

// applyFields writes f into ev. rruleOK is false for an override.
func applyFields(ev *Component, f Fields, rruleOK bool) *Refusal {
	if f.Start != nil {
		p, r := timeProp("DTSTART", *f.Start)
		if r != nil {
			return r
		}
		replaceProp(ev, p, "DTSTART")
	}
	if f.End != nil {
		p, r := timeProp("DTEND", *f.End)
		if r != nil {
			return r
		}
		replaceProp(ev, p, "DTEND", "DURATION")
	}
	if f.Start != nil || f.End != nil {
		if r := checkSpan(*ev); r != nil {
			return r
		}
	}
	if f.RRule != nil {
		if !rruleOK {
			return refuse(UnsupportedRecurrence, "an occurrence takes no RRULE")
		}
		stored, has := ev.Get("RRULE")
		switch {
		case *f.RRule == "":
			ev.removeProps(func(p Property) bool { return strings.EqualFold(p.Name, "RRULE") })
		case has && sameRule(stored.Value, *f.RRule): // kept as written
		default:
			rule, r := protonRule(*ev, *f.RRule)
			if r != nil {
				return r
			}
			replaceProp(ev, Property{Name: "RRULE", Value: rule}, "RRULE")
		}
	}
	for _, t := range []struct {
		name string
		v    *string
	}{{"SUMMARY", f.Summary}, {"DESCRIPTION", f.Description}, {"LOCATION", f.Location}} {
		switch {
		case t.v == nil:
		case *t.v == "":
			ev.removeProps(func(p Property) bool { return strings.EqualFold(p.Name, t.name) })
		default:
			v := escapeText(*t.v)
			if hasControl(v) {
				return refuse(BadData, t.name+" holds a control character")
			}
			replaceProp(ev, Property{Name: t.name, Value: v}, t.name)
		}
	}
	if f.Busy != nil {
		transp := "OPAQUE"
		if !*f.Busy {
			transp = "TRANSPARENT"
		}
		replaceProp(ev, Property{Name: "TRANSP", Value: transp}, "TRANSP")
	}
	if f.Alarms != nil {
		return replaceAlarms(ev, *f.Alarms)
	}
	return nil
}

// checkSpan refuses a DTSTART and DTEND that are not both dates or both
// times, or an end before the start.
func checkSpan(ev Component) *Refusal {
	sp, _ := ev.Get("DTSTART")
	start, err := decodeTime(sp, sp.Value)
	if err != nil {
		return refuse(BadData, "the stored DTSTART is malformed")
	}
	ep, ok := ev.Get("DTEND")
	if !ok {
		return nil
	}
	end, err := decodeTime(ep, ep.Value)
	switch {
	case err != nil:
		return refuse(BadData, "the stored DTEND is malformed")
	case (start.Date != "") != (end.Date != ""):
		return refuse(BadData, "the start and the end are not both dates or both times")
	case startsBefore(end, start):
		return refuse(BadData, "the end is before the start")
	}
	return nil
}

// protonRule is rule in Proton's form, refused outside Proton's set
// (checkRRule against ev's DTSTART).
func protonRule(ev Component, rule string) (string, *Refusal) {
	if hasControl(rule) {
		return "", refuse(BadData, "RRULE holds a control character")
	}
	rule = normalRule(rule)
	sp, _ := ev.Get("DTSTART")
	start, r := parseWhen(&sp, sp.Value, nil)
	if r != nil {
		return "", r
	}
	if r := checkRRule(rule, start); r != nil {
		return "", r
	}
	return rule, nil
}

// normalRule is rule without an "RRULE:" prefix, in Proton's form.
func normalRule(rule string) string {
	rule = strings.TrimSpace(rule)
	if len(rule) >= 6 && strings.EqualFold(rule[:6], "RRULE:") {
		rule = rule[6:]
	}
	if v, ok := monthlyOrdinal(rule); ok {
		rule = v
	}
	if v, ok := dailyByday(rule); ok {
		rule = v
	}
	return rule
}

// sameRule compares two RRULEs by meaning as IsSharedSignedChange does:
// parts in any order, case-folded, INTERVAL=1 as absent.
func sameRule(a, b string) bool {
	key := func(r string) string {
		if r = normalRule(r); r == "" {
			return ""
		}
		parts := slices.DeleteFunc(strings.Split(strings.ToUpper(r), ";"), func(s string) bool { return s == "INTERVAL=1" })
		slices.Sort(parts)
		return strings.Join(parts, ";")
	}
	return key(a) == key(b)
}

// replaceAlarms replaces every VALARM the view shows as an Alarm (a whole
// number of minutes relative to the start) by one DISPLAY alarm per entry,
// in the wording ical.Alarms renders. Other VALARMs stay. Alarms Proton
// would drop (NotificationsFromAlarms: more than ten, after a timed
// event's start, a day or more after an all-day one's) are refused.
func replaceAlarms(ev *Component, minutes []int) *Refusal {
	sp, _ := ev.Get("DTSTART")
	allDay := strings.EqualFold(firstParam(sp, "VALUE"), "DATE")
	text := "Reminder"
	if allDay {
		text = "Reminder (all-day event)"
	}
	alarms := make([]Component, len(minutes))
	for i, m := range minutes {
		trigger := fmt.Sprintf("-PT%dM", m)
		if m <= 0 {
			trigger = fmt.Sprintf("PT%dM", -m)
		}
		a := Component{Name: "VALARM"}
		a.add(Property{Name: "ACTION", Value: "DISPLAY"})
		a.add(Property{Name: "TRIGGER", Value: trigger})
		a.add(Property{Name: "DESCRIPTION", Value: text})
		alarms[i] = a
	}
	if _, dropped := NotificationsFromAlarms(alarms, allDay); dropped > 0 {
		return refuse(BadData, fmt.Sprintf("an alarm Proton cannot keep: at most %d, none after a timed event's start or a day after an all-day one's", maxNotifications))
	}
	kept := ev.Items[:0:0]
	for _, it := range ev.Items {
		if it.Comp != nil {
			if _, relative := alarmOf(*it.Comp); relative {
				continue
			}
		}
		kept = append(kept, it)
	}
	ev.Items = kept
	for i := range alarms {
		ev.Items = append(ev.Items, Item{Comp: &alarms[i]})
	}
	return nil
}

func firstParam(p Property, name string) string {
	v, _ := param(p.Params, name)
	return v
}

// textEscaper escapes a TEXT value (RFC 5545 §3.3.11); a CR, LF or CRLF
// is one "\n".
var textEscaper = strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\r", `\n`, "\n", `\n`)

func escapeText(s string) string { return textEscaper.Replace(s) }

// touchEdited sets ev's DTSTAMP to now and bumps its SEQUENCE (absent or not a
// number counts as 0).
func touchEdited(ev *Component, now time.Time) {
	replaceProp(ev, Property{Name: "DTSTAMP", Value: dtstamp(now)}, "DTSTAMP")
	seq := 0
	if p, ok := ev.Get("SEQUENCE"); ok && isCount(strings.TrimSpace(p.Value)) {
		seq = atoi(strings.TrimSpace(p.Value))
	}
	replaceProp(ev, Property{Name: "SEQUENCE", Value: fmt.Sprint(seq + 1)}, "SEQUENCE")
}

func dtstamp(now time.Time) string { return now.UTC().Format(localLayout) + "Z" }

// replaceProp puts p in place of the first property called one of names
// and removes the others so called; with none, p goes after the last
// property.
func replaceProp(ev *Component, p Property, names ...string) {
	match := func(it Item) bool {
		return it.Comp == nil && slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(it.Prop.Name, n) })
	}
	at := slices.IndexFunc(ev.Items, match)
	if at < 0 {
		ev.addProp(p)
		return
	}
	ev.Items[at] = Item{Prop: p}
	ev.Items = slices.Concat(ev.Items[:at+1], slices.DeleteFunc(slices.Clone(ev.Items[at+1:]), match))
}

// insertProp inserts p at index at of ev's items, kept before the first
// nested component.
func insertProp(ev *Component, at int, p Property) {
	at = min(at, ev.firstSub())
	ev.Items = slices.Insert(ev.Items, at, Item{Prop: p})
}

// lastPropIndex is the index of the last property called name, or -1.
func lastPropIndex(ev Component, name string) int {
	for i := len(ev.Items) - 1; i >= 0; i-- {
		if it := ev.Items[i]; it.Comp == nil && strings.EqualFold(it.Prop.Name, name) {
			return i
		}
	}
	return -1
}
