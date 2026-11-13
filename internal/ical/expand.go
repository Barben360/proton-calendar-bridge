package ical

import (
	"cmp"
	"errors"
	"slices"
	"strings"
	"time"
)

// Recurrence expansion (M8, experiment E16: rrule-go is not adopted). Our own
// expander, written from RFC 5545 §3.3.5, §3.3.10 and §3.8.5.3, for exactly
// the RRULE shapes Proton accepts (checkRRule's shapes, ruling M4-R10),
// after the two rewrites ParseWrite applies (R5 monthlyOrdinal, R12
// dailyByday). Proton's numeric limits (COUNT, INTERVAL, UNTIL) bound writes
// only: a stored rule expands whatever its numbers. Rules are walked on wall
// clocks (time.Time fields read as UTC, no DST) and each wall clock is made
// an instant by resolve, never by bare time.Date.

// Occurrence is one occurrence of a series in a range.
type Occurrence struct {
	Start, End   time.Time  // instants; all-day: midnight of the dates in loc
	AllDay       bool       // DTSTART is a DATE
	StartDate    string     // all-day only, YYYY-MM-DD
	EndDate      string     // all-day only, exclusive
	TZID         string     // the DTSTART zone, "" for UTC, floating or all-day
	Floating     bool       // DTSTART is a local time without TZID, read in loc
	RecurrenceID *time.Time // nil for a non-recurring event; else the original start, read as Start is
	Override     bool       // taken from an overriding component
}

// Expand returns the occurrences of master and its overrides (components
// with a RECURRENCE-ID) that overlap [from, to), sorted by Start, at most
// max per series; truncated reports a cut. Floating and all-day times are
// read in loc (nil: UTC). EXDATEs remove occurrences; an override replaces
// the occurrence its RECURRENCE-ID names, or adds one when moved into the
// range. An override always shows at its own times, even when its
// RECURRENCE-ID names no occurrence of the rule (as Thunderbird shows it).
// A zero master (Name "") stands for a series whose master is gone: only
// its overrides are returned.
//
// The rule: DTSTART is the first occurrence and counts towards COUNT even
// off the rule (RFC 5545 §3.8.5.3); COUNT counts before EXDATEs remove;
// UNTIL is inclusive (a UTC UNTIL compares as an instant, a DATE UNTIL keeps
// its whole local day); invalid dates (31 April, 29 February of a common
// year) are skipped and not counted. A wall clock in a DST gap is read with
// the offset before the gap, one that occurs twice is the first (RFC 5545
// §3.3.5). An event's length is exact from DTEND, nominal in days and exact
// in hours from DURATION, one day for a DATE start without either, zero
// otherwise.
//
// An RRULE outside Proton's set, RDATE, EXRULE and a RECURRENCE-ID with
// RANGE are a *Refusal (UnsupportedRecurrence, RDate, ExRule, Range); a
// malformed time BadData, an unknown TZID UnknownTZID. Reasons never quote
// the event.
func Expand(master Component, overrides []Component, from, to time.Time, loc *time.Location, max int) (occ []Occurrence, truncated bool, err error) {
	if max < 1 {
		return nil, false, errors.New("ical: Expand needs a positive max")
	}
	if loc == nil {
		loc = time.UTC
	}
	var ser *series
	if master.Name != "" {
		s, r := readSeries(master, loc)
		if r != nil {
			return nil, false, r
		}
		ser = &s
	}
	var moved []Occurrence
	replaced := newExceptionSet()
	for _, c := range overrides {
		o, rid, r := readOverride(c, loc)
		if r != nil {
			return nil, false, r
		}
		if ser != nil {
			replaced.add(rid, ser.timing.start.date)
		}
		if overlaps(o, from, to) {
			moved = append(moved, o)
		}
	}
	if ser != nil {
		occ = ser.expand(from, to, replaced, max)
	}
	occ = append(occ, moved...)
	slices.SortStableFunc(occ, func(a, b Occurrence) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		return a.End.Compare(b.End)
	})
	if len(occ) > max {
		return occ[:max], true, nil
	}
	return occ, false, nil
}

// overlaps: o meets [from, to). An event without length meets it when it
// starts in it (RFC 4791 §9.9).
func overlaps(o Occurrence, from, to time.Time) bool {
	if o.End.After(o.Start) {
		return o.Start.Before(to) && o.End.After(from)
	}
	return !o.Start.Before(from) && o.Start.Before(to)
}

// stamp is a parsed DATE or DATE-TIME value.
type stamp struct {
	wall     time.Time      // the wall clock (a DATE: its midnight), fields read as UTC
	date     bool           // a DATE
	frame    *time.Location // the zone wall is read in: the TZID's, UTC, or loc (floating, DATE)
	tzid     string         // the TZID after Mozilla mapping; "" for UTC, floating or DATE
	floating bool           // a local time without TZID
}

func (s stamp) instant() time.Time { return resolve(s.wall, s.frame) }

// parseStamp reads value as the DATE or DATE-TIME property p says. Unlike
// parseWhen (writes), a floating time is accepted and read in loc.
func parseStamp(p Property, value string, loc *time.Location) (stamp, *Refusal) {
	valueType, _ := param(p.Params, "VALUE")
	tzid, hasTZ := param(p.Params, "TZID")
	switch strings.ToUpper(valueType) {
	case "DATE":
		t, err := time.Parse(dateLayout, value)
		if hasTZ || err != nil || len(value) != 8 {
			return stamp{}, refuse(BadData, "a malformed DATE")
		}
		return stamp{wall: t, date: true, frame: loc}, nil
	case "", "DATE-TIME":
	default:
		return stamp{}, refuse(BadData, "a time property with an unsupported VALUE")
	}
	utc := len(value) == 16 && value[15] == 'Z'
	if utc {
		value = value[:15]
	}
	t, err := time.Parse(localLayout, value)
	if err != nil || len(value) != 15 || utc && hasTZ {
		return stamp{}, refuse(BadData, "a malformed DATE-TIME")
	}
	switch {
	case utc:
		return stamp{wall: t, frame: time.UTC}, nil
	case !hasTZ:
		return stamp{wall: t, frame: loc, floating: true}, nil
	}
	zone, name, ok := location(tzid)
	if !ok {
		return stamp{}, refuse(UnknownTZID, "a TZID that is not a known time zone")
	}
	return stamp{wall: t, frame: zone, tzid: name}, nil
}

// timing is a VEVENT's DTSTART and length.
type timing struct {
	start stamp
	days  int           // the nominal part of the length, in days
	exact time.Duration // the exact part
}

// maxExact bounds a DURATION's exact part (hours, minutes, seconds): about
// 31 years, far below time.Duration's limit.
const maxExact = 1_000_000_000

func readTiming(c Component, loc *time.Location) (timing, *Refusal) {
	p, ok := c.Get("DTSTART")
	if !ok {
		return timing{}, refuse(BadData, "a VEVENT without DTSTART")
	}
	start, r := parseStamp(p, p.Value, loc)
	if r != nil {
		return timing{}, r
	}
	tm := timing{start: start}
	end, hasEnd := c.Get("DTEND")
	dur, hasDur := c.Get("DURATION")
	switch {
	case hasEnd && hasDur:
		return timing{}, refuse(BadData, "both DTEND and DURATION")
	case hasEnd:
		e, r := parseStamp(end, end.Value, loc)
		if r != nil {
			return timing{}, r
		}
		if e.date != start.date {
			return timing{}, refuse(BadData, "DTEND is not of DTSTART's value type")
		}
		if start.date {
			tm.days = int((e.wall.Unix() - start.wall.Unix()) / 86400)
		} else {
			tm.exact = e.instant().Sub(start.instant())
		}
		if tm.days < 0 || tm.exact < 0 {
			return timing{}, refuse(BadData, "DTEND is before DTSTART")
		}
	case hasDur:
		d, ok := parseDuration(dur.Value)
		if !ok || d.neg {
			return timing{}, refuse(BadData, "DURATION is not a positive duration")
		}
		secs := d.hours*3600 + d.minutes*60 + d.seconds
		if secs > maxExact || start.date && secs != 0 {
			return timing{}, refuse(BadData, "DURATION does not fit the event")
		}
		tm.days, tm.exact = int(7*d.weeks+d.days), time.Duration(secs)*time.Second
	case start.date:
		tm.days = 1
	}
	return tm, nil
}

// occurrence is the occurrence of tm that starts at wall clock w.
func (tm timing) occurrence(w time.Time) Occurrence {
	s := tm.start
	s.wall = w
	o := Occurrence{Start: s.instant(), AllDay: s.date, TZID: s.tzid, Floating: s.floating}
	switch {
	case s.date:
		end := w.AddDate(0, 0, tm.days)
		o.End = resolve(end, s.frame)
		o.StartDate, o.EndDate = w.Format(time.DateOnly), end.Format(time.DateOnly)
	case tm.days != 0:
		o.End = resolve(w.AddDate(0, 0, tm.days), s.frame).Add(tm.exact)
	default:
		o.End = o.Start.Add(tm.exact)
	}
	return o
}

func readOverride(c Component, loc *time.Location) (Occurrence, stamp, *Refusal) {
	p, ok := c.Get("RECURRENCE-ID")
	if !ok {
		return Occurrence{}, stamp{}, refuse(BadData, "an override without RECURRENCE-ID")
	}
	if _, ok := param(p.Params, "RANGE"); ok {
		return Occurrence{}, stamp{}, refuse(Range, "RECURRENCE-ID with RANGE has no Proton equivalent")
	}
	rid, r := parseStamp(p, p.Value, loc)
	if r != nil {
		return Occurrence{}, stamp{}, r
	}
	tm, r := readTiming(c, loc)
	if r != nil {
		return Occurrence{}, stamp{}, r
	}
	o := tm.occurrence(tm.start.wall)
	at := rid.instant()
	o.RecurrenceID, o.Override = &at, true
	return o, rid, nil
}

// exceptionSet holds EXDATEs or overrides' RECURRENCE-IDs by what they
// match: an instant, or a local date (a DATE against a timed series, any
// value against an all-day one: the date of its wall clock).
type exceptionSet struct {
	instants map[int64]bool
	dates    map[string]bool
}

func newExceptionSet() exceptionSet {
	return exceptionSet{instants: map[int64]bool{}, dates: map[string]bool{}}
}

func (e exceptionSet) add(s stamp, allDay bool) {
	if allDay || s.date {
		e.dates[s.wall.Format(dateLayout)] = true
		return
	}
	e.instants[s.instant().Unix()] = true
}

// has: the occurrence at wall clock w (instant at) is in the set.
func (e exceptionSet) has(w, at time.Time, allDay bool) bool {
	return !allDay && e.instants[at.Unix()] || e.dates[w.Format(dateLayout)]
}

// series is a master VEVENT: its timing, rule (nil: not recurring) and
// EXDATEs.
type series struct {
	timing  timing
	rule    *rule
	exdates exceptionSet
}

func readSeries(c Component, loc *time.Location) (series, *Refusal) {
	tm, r := readTiming(c, loc)
	if r != nil {
		return series{}, r
	}
	s := series{timing: tm, exdates: newExceptionSet()}
	for _, p := range c.Props() {
		switch strings.ToUpper(p.Name) {
		case "RDATE":
			return series{}, refuse(RDate, "RDATE has no Proton equivalent")
		case "EXRULE":
			return series{}, refuse(ExRule, "EXRULE has no Proton equivalent")
		case "RRULE":
			if s.rule != nil {
				return series{}, refuse(UnsupportedRecurrence, "several RRULEs")
			}
			if s.rule, r = parseRule(p.Value, tm.start); r != nil {
				return series{}, r
			}
		case "EXDATE":
			for v := range strings.SplitSeq(p.Value, ",") {
				ex, r := parseStamp(p, v, loc)
				if r != nil {
					return series{}, r
				}
				s.exdates.add(ex, tm.start.date)
			}
		}
	}
	return s, nil
}

// expand lists the master's occurrences that overlap [from, to), minus the
// EXDATEs and the occurrences replaced holds, stopping at max+1: the caller
// cuts at max after merging the overrides, which then never displace an
// occurrence not generated (they all start after the last one kept).
func (s series) expand(from, to time.Time, replaced exceptionSet, max int) []Occurrence {
	allDay := s.timing.start.date
	var out []Occurrence
	emit := func(w time.Time) bool {
		o := s.timing.occurrence(w)
		if s.exdates.has(w, o.Start, allDay) || replaced.has(w, o.Start, allDay) || !overlaps(o, from, to) {
			return true
		}
		if s.rule != nil {
			at := o.Start
			o.RecurrenceID = &at
		}
		out = append(out, o)
		return len(out) <= max
	}
	if s.rule == nil {
		emit(s.timing.start.wall)
		return out
	}
	// Wall-clock bounds: two days of margin cover any UTC offset; lo also
	// reaches back by the event's length, for occurrences that started
	// before from and still run.
	lo := from.UTC().AddDate(0, 0, -s.timing.days-2).Add(-s.timing.exact)
	hi := to.UTC().AddDate(0, 0, 2)
	s.rule.walk(s.timing.start, lo, hi, emit)
	return out
}

// rule is an RRULE of Proton's set, parsed.
type rule struct {
	freq     string
	interval int
	count    int // 0: none
	until    *until
	wkst     time.Weekday
	byday    []time.Weekday // WEEKLY: the days (DTSTART's when none); MONTHLY: the BYSETPOS day
	setPos   int            // MONTHLY BYSETPOS, 0: none
	monthDay int            // BYMONTHDAY, 0: none
	month    time.Month     // YEARLY BYMONTH, 0: none
}

// until is an RRULE UNTIL as it compares: an instant (a UTC UNTIL on a timed
// start), the last local date (a DATE, or any UNTIL on an all-day start), or
// a local time (a local UNTIL, read in DTSTART's frame).
type until struct {
	instant time.Time // set for a UTC UNTIL on a timed start
	date    time.Time // set for a date bound: the day after the last one, midnight
	wall    time.Time // set for a local time
}

// after: an occurrence at wall clock w of start's frame comes after u.
func (u *until) after(w time.Time, start stamp) bool {
	switch {
	case !u.instant.IsZero():
		return resolve(w, start.frame).After(u.instant)
	case !u.date.IsZero():
		return !w.Before(u.date)
	}
	return w.After(u.wall)
}

var weekdayOf = map[string]time.Weekday{
	"SU": time.Sunday, "MO": time.Monday, "TU": time.Tuesday, "WE": time.Wednesday,
	"TH": time.Thursday, "FR": time.Friday, "SA": time.Saturday,
}

// parseRule reads an RRULE after ParseWrite's two rewrites (R5, R12): a
// shape outside Proton's set is UnsupportedRecurrence, a malformed one
// BadData.
func parseRule(text string, start stamp) (*rule, *Refusal) {
	text, _ = monthlyOrdinal(text)
	text, _ = dailyByday(text)
	parts, freq, r := rruleParts(text)
	if r != nil {
		return nil, r
	}
	if r := rruleShape(freq, parts); r != nil {
		return nil, r
	}
	ru := &rule{freq: freq, interval: 1, wkst: time.Monday}
	for _, n := range []struct {
		name string
		dst  *int
	}{{"INTERVAL", &ru.interval}, {"COUNT", &ru.count}} {
		if v, ok := parts[n.name]; ok {
			if !isCount(v) || atoi(v) == 0 {
				return nil, refuse(BadData, "RRULE "+n.name+" is not a positive number")
			}
			*n.dst = atoi(v)
		}
	}
	if v, ok := parts["UNTIL"]; ok {
		if ru.count > 0 {
			return nil, refuse(UnsupportedRecurrence, "RRULE with both COUNT and UNTIL")
		}
		if ru.until, r = parseUntil(v, start); r != nil {
			return nil, r
		}
	}
	if v, ok := parts["WKST"]; ok {
		ru.wkst = weekdayOf[strings.ToUpper(v)]
	}
	if v, ok := parts["BYDAY"]; ok {
		for d := range strings.SplitSeq(v, ",") {
			ru.byday = append(ru.byday, weekdayOf[strings.ToUpper(d)])
		}
	} else if freq == "WEEKLY" {
		ru.byday = []time.Weekday{start.wall.Weekday()}
	}
	ru.setPos, ru.monthDay = atoi(parts["BYSETPOS"]), atoi(parts["BYMONTHDAY"])
	ru.month = time.Month(atoi(parts["BYMONTH"]))
	return ru, nil
}

// parseUntil reads an UNTIL against DTSTART: a DATE keeps its whole local
// day (on a timed start too, E16's call: checkRRule accepts the shape); a
// UTC time is an instant, or its UTC date on an all-day start (as
// untilDay reads it); a local time (RFC 5545 for a floating start) is read
// in DTSTART's frame.
func parseUntil(v string, start stamp) (*until, *Refusal) {
	utc := len(v) == 16 && v[15] == 'Z'
	local := strings.TrimSuffix(v, "Z")
	var t time.Time
	var err error
	switch {
	case len(v) == 8:
		t, err = time.Parse(dateLayout, v)
	case len(local) == 15:
		t, err = time.Parse(localLayout, local)
	default:
		err = errors.New("length")
	}
	if err != nil {
		return nil, refuse(BadData, "a malformed RRULE UNTIL")
	}
	switch {
	case len(v) == 8 || start.date:
		y, m, d := t.Date()
		return &until{date: time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)}, nil
	case utc:
		return &until{instant: t}, nil
	}
	return &until{wall: t}, nil
}

// maxYear: a walk stops past it (a huge INTERVAL never overflows).
const maxYear = 9999

// walk calls emit with each occurrence's wall clock in order, from DTSTART
// (always the first) or, for a rule without COUNT, from the last period
// starting a whole period before lo; it stops past hi, UNTIL, COUNT, year
// 9999, or when emit returns false.
func (ru *rule) walk(start stamp, lo, hi time.Time, emit func(time.Time) bool) {
	s := start.wall
	k, n := 0, 0
	if ru.count == 0 {
		k = ru.jump(s, lo)
	}
	if k == 0 {
		n = 1
		if !emit(s) || ru.count == 1 {
			return
		}
	}
	var buf []time.Time
	for ; ; k++ {
		anchor, dates, ok := ru.period(s, k, buf[:0])
		buf = dates
		if !ok || anchor.After(hi) {
			return
		}
		for _, w := range dates {
			if !w.After(s) {
				continue
			}
			if w.After(hi) || ru.until != nil && ru.until.after(w, start) {
				return
			}
			n++
			if !emit(w) || ru.count > 0 && n >= ru.count {
				return
			}
		}
	}
}

// jump is the index of a period that starts before lo, with every
// occurrence of the periods before it earlier still (0: walk from DTSTART).
func (ru *rule) jump(s, lo time.Time) int {
	if !lo.After(s) {
		return 0
	}
	var units int
	switch ru.freq {
	case "DAILY":
		units = int((lo.Unix() - s.Unix()) / 86400)
	case "WEEKLY":
		units = int((lo.Unix() - ru.week0(s).Unix()) / (7 * 86400))
	case "MONTHLY":
		units = lo.Year()*12 + int(lo.Month()) - s.Year()*12 - int(s.Month())
	case "YEARLY":
		units = lo.Year() - s.Year()
	}
	return max(units/ru.interval-1, 0)
}

// week0 is the first day of DTSTART's week (WKST), at DTSTART's time.
func (ru *rule) week0(s time.Time) time.Time {
	return s.AddDate(0, 0, -((int(s.Weekday()) - int(ru.wkst) + 7) % 7))
}

// period appends to buf the dates period k of the rule yields from DTSTART
// s, in order, at s's time of day, and returns the period's first day;
// ok is false past year 9999.
func (ru *rule) period(s time.Time, k int, buf []time.Time) (anchor time.Time, dates []time.Time, ok bool) {
	h, mi, sec := s.Clock()
	at := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, h, mi, sec, 0, time.UTC) }
	switch ru.freq {
	case "DAILY":
		anchor = s.AddDate(0, 0, k*ru.interval)
		return anchor, append(buf, anchor), anchor.Year() <= maxYear
	case "WEEKLY":
		anchor = ru.week0(s).AddDate(0, 0, 7*k*ru.interval)
		if anchor.Year() > maxYear {
			return anchor, buf, false
		}
		offsets := make([]int, 0, len(ru.byday))
		for _, d := range ru.byday {
			offsets = append(offsets, (int(d)-int(ru.wkst)+7)%7)
		}
		slices.Sort(offsets)
		for _, off := range slices.Compact(offsets) {
			buf = append(buf, anchor.AddDate(0, 0, off))
		}
		return anchor, buf, true
	case "MONTHLY":
		idx := s.Year()*12 + int(s.Month()) - 1 + k*ru.interval
		y, m := idx/12, time.Month(idx%12+1)
		if y > maxYear {
			return time.Time{}, buf, false
		}
		if d := ru.dayIn(y, m, s.Day()); d > 0 {
			buf = append(buf, at(y, m, d))
		}
		return at(y, m, 1), buf, true
	default: // YEARLY
		y, m := s.Year()+k*ru.interval, cmp.Or(ru.month, s.Month())
		if y > maxYear {
			return time.Time{}, buf, false
		}
		if d := ru.dayIn(y, m, s.Day()); d > 0 {
			buf = append(buf, at(y, m, d))
		}
		return at(y, 1, 1), buf, true
	}
}

// dayIn is the day of month m the rule takes (dflt, DTSTART's, without BY
// part), or 0 when the month has no such day.
func (ru *rule) dayIn(y int, m time.Month, dflt int) int {
	last := time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
	d := dflt
	switch {
	case ru.setPos != 0:
		first := 1 + (int(ru.byday[0])-int(time.Date(y, m, 1, 0, 0, 0, 0, time.UTC).Weekday())+7)%7
		if ru.setPos > 0 {
			d = first + 7*(ru.setPos-1)
		} else { // -1, the only negative BYSETPOS of Proton's set: the last
			d = first + 7*((last-first)/7)
		}
	case ru.monthDay > 0:
		d = ru.monthDay
	case ru.monthDay < 0:
		d = last + ru.monthDay + 1
	}
	if d < 1 || d > last {
		return 0
	}
	return d
}

// parseWall reads value, a local DATE-TIME (no Z), as a wall clock of loc
// by resolve's rule: every reader of a TZID time (the view, the edits, the
// CalDAV write path) gives the instant Expand gives, DST overlaps and gaps
// included.
func parseWall(value string, loc *time.Location) (time.Time, error) {
	w, err := time.Parse(localLayout, value)
	if err != nil {
		return time.Time{}, err
	}
	return resolve(w, loc), nil
}

// resolve is the instant of wall clock w (its fields read as a local time of
// loc), by RFC 5545 §3.3.5, which §3.3.10 applies to recurrence instances: a
// local time that occurs twice is the first of the two; one that does not
// exist (a DST gap) is read with the UTC offset before the gap, so 02:30 in
// a 02:00→03:00 gap is 03:30 summer time. time.Date guarantees neither. It
// walks the zone periods in effect from two days before w to two days after
// (any UTC offset is within that), the earliest first.
func resolve(w time.Time, loc *time.Location) time.Time {
	if loc == time.UTC {
		return w
	}
	const probe = 2 * 86400
	u := w.Unix()
	t := time.Unix(u-probe, 0).In(loc)
	for range 64 {
		_, off := t.Zone()
		start, end := t.ZoneBounds()
		at := u - int64(off)
		if (start.IsZero() || at >= start.Unix()) && (end.IsZero() || at < end.Unix()) {
			return time.Unix(at, 0).In(loc)
		}
		if end.IsZero() || end.Unix() > u+probe {
			break
		}
		next := end.In(loc)
		if _, nextOff := next.Zone(); at >= end.Unix() && u-int64(nextOff) < end.Unix() {
			return time.Unix(at, 0).In(loc) // in the gap after this period: its offset
		}
		t = next
	}
	y, m, d := w.Date()
	h, mi, s := w.Clock()
	return time.Date(y, m, d, h, mi, s, 0, loc)
}
