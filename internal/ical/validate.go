package ical

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	// Go's zone database, embedded: every zone of the IANA database resolves
	// even on a host without one. time.LoadLocation reads ZONEINFO and the
	// host's database first, so a host may resolve a few more names
	// (posixrules, Factory); the write side refuses what does not resolve
	// (constraints: tzdata is embedded in internal/ical only, from this file).
	_ "time/tzdata"
)

// RefusalCode says why a PUT body is refused. The CalDAV adapter puts it,
// with Reason, in a pcb precondition element (constraints, Refusals).
type RefusalCode string

// The refusal codes of ParseWrite.
const (
	NotOneUID             RefusalCode = "not-one-uid"            // VEVENTs of several UIDs
	TooMany               RefusalCode = "too-many-events"        // more than MaxEvents VEVENTs
	Floating              RefusalCode = "floating-time"          // a date-time with neither Z nor TZID
	UnknownTZID           RefusalCode = "unknown-tzid"           // a TZID Go's zone database cannot resolve
	UnsupportedRecurrence RefusalCode = "unsupported-recurrence" // an RRULE outside Proton's set
	Range                 RefusalCode = "recurrence-range"       // RECURRENCE-ID;RANGE=…
	RDate                 RefusalCode = "rdate"                  // RDATE
	ExRule                RefusalCode = "exrule"                 // EXRULE
	BadData               RefusalCode = "bad-data"               // anything else malformed
)

// Refusal is ParseWrite's error. Reason is short and fixed: it never quotes
// the body (calendar content), only names properties and rules.
type Refusal struct {
	Reason string
	Code   RefusalCode
}

func (r *Refusal) Error() string { return fmt.Sprintf("ical: refused (%s): %s", r.Code, r.Reason) }

func refuse(code RefusalCode, reason string) *Refusal { return &Refusal{Reason: reason, Code: code} }

// MaxEvents is the most VEVENTs a PUT body may hold (master and exceptions).
const MaxEvents = 100

// Notes of ParseWrite: what normalisation changed, for a debug log. Fixed
// text, no content.
const (
	noteTentative = "STATUS:TENTATIVE dropped"
	noteColor     = "client COLOR ignored"
	noteOurs      = "X-PCB-* or X-PM-SESSION-KEY dropped"
	noteToken     = "X-PM-TOKEN dropped from ATTENDEE"
	noteDuration  = "DURATION converted to DTEND"
	noteMozilla   = "Mozilla TZID mapped to its IANA name"
	noteFaked     = "Thunderbird's faked master dropped"
	noteDaily     = "DAILY BYDAY written as WEEKLY"
	noteMonthly   = "MONTHLY BYDAY ordinal written as BYSETPOS"
)

// WriteEvent is one VEVENT of a PUT body, normalised for writing.
type WriteEvent struct {
	// Comp holds properties only: no VALARM, COLOR, X-PCB-*,
	// X-PM-SESSION-KEY, VCALENDAR-level property, DURATION (made DTEND) or
	// STATUS:TENTATIVE; no X-PM-TOKEN on ATTENDEE; Mozilla TZIDs mapped on
	// DTSTART, DTEND, RECURRENCE-ID and EXDATE.
	Comp Component
	// Alarms are its VALARMs as sent: NotificationsFromAlarms reads them,
	// they never go into a part.
	Alarms []Component
	// AllDay: DTSTART is a DATE.
	AllDay bool
}

// Incoming is a parsed PUT body: one UID, its master (nil when the body
// holds exceptions only) and its exceptions keyed by RECURRENCE-ID in Unix
// seconds (a DATE is midnight UTC, as Proton stores it).
type Incoming struct {
	UID        string
	Master     *WriteEvent
	Exceptions map[int64]WriteEvent
	// Notes says what normalisation changed (accepted and normalised, logged
	// at debug): fixed strings, each at most once, no content.
	Notes []string
}

type notes struct{ list []string }

func (n *notes) add(s string) {
	for _, have := range n.list {
		if have == s {
			return
		}
	}
	n.list = append(n.list, s)
}

// ParseWrite reads what a CalDAV client PUTs: one VCALENDAR whose VEVENTs
// share one UID (at most MaxEvents, at most one master, no two exceptions
// at the same RECURRENCE-ID). VCALENDAR-level properties and VTIMEZONEs are
// dropped (TZIDs are resolved by name); any other component is refused.
// Each VEVENT is validated and normalised (see WriteEvent). Errors are
// *Refusal.
func ParseWrite(body []byte) (Incoming, error) {
	if !utf8.Valid(body) {
		return Incoming{}, refuse(BadData, "not valid UTF-8")
	}
	comps, err := Parse(body)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			return Incoming{}, refuse(BadData, fmt.Sprintf("not iCalendar (line %d: %s)", pe.Line, pe.Msg))
		}
		return Incoming{}, refuse(BadData, "not iCalendar")
	}
	if len(comps) != 1 || !strings.EqualFold(comps[0].Name, "VCALENDAR") {
		return Incoming{}, refuse(BadData, "the body is not one VCALENDAR")
	}
	var n notes
	var events []Component
	for _, it := range comps[0].Items {
		if it.Comp == nil {
			continue
		}
		switch strings.ToUpper(it.Comp.Name) {
		case "VEVENT":
			if fakedMaster(*it.Comp) {
				n.add(noteFaked)
				continue
			}
			events = append(events, *it.Comp)
		case "VTIMEZONE":
		default:
			return Incoming{}, refuse(BadData, "the calendar holds a component other than VEVENT")
		}
	}
	if len(events) == 0 {
		return Incoming{}, refuse(BadData, "the calendar holds no VEVENT")
	}
	if len(events) > MaxEvents {
		return Incoming{}, refuse(TooMany, fmt.Sprintf("more than %d VEVENTs", MaxEvents))
	}
	in := Incoming{Exceptions: map[int64]WriteEvent{}}
	for i, ev := range events {
		var uids []string
		for _, p := range ev.Props() {
			if strings.EqualFold(p.Name, "UID") {
				uids = append(uids, p.Value)
			}
		}
		if len(uids) != 1 || uids[0] == "" {
			return Incoming{}, refuse(BadData, "a VEVENT without exactly one non-empty UID")
		}
		if i == 0 {
			in.UID = uids[0]
		} else if uids[0] != in.UID {
			return Incoming{}, refuse(NotOneUID, "VEVENTs of more than one UID")
		}
	}
	ridDate := map[int64]bool{}
	for _, ev := range events {
		we, rid, r := normalise(ev, &n)
		if r != nil {
			return Incoming{}, r
		}
		switch {
		case rid == nil && in.Master != nil:
			return Incoming{}, refuse(BadData, "two VEVENTs without RECURRENCE-ID")
		case rid == nil:
			in.Master = &we
		default:
			key := rid.t.Unix()
			if _, dup := in.Exceptions[key]; dup {
				return Incoming{}, refuse(BadData, "two VEVENTs with the same RECURRENCE-ID")
			}
			in.Exceptions[key], ridDate[key] = we, rid.date
		}
	}
	for _, date := range ridDate {
		if in.Master != nil && date != in.Master.AllDay {
			return Incoming{}, refuse(BadData, "a RECURRENCE-ID of another value type than the master's DTSTART")
		}
	}
	in.Notes = n.list
	return in, nil
}

// fakedMaster: Thunderbird's stand-in master for an orphan exception
// (E08 §6.4), X-MOZ-FAKED-MASTER:1, never a real event (ruling M4-R11).
func fakedMaster(ev Component) bool {
	p, ok := ev.Get("X-MOZ-FAKED-MASTER")
	return ok && strings.TrimSpace(p.Value) == "1"
}

// vcalendarLevel are properties of the VCALENDAR, never of a VEVENT.
var vcalendarLevel = map[string]bool{"VERSION": true, "PRODID": true, "CALSCALE": true, "METHOD": true}

// validName: RFC 5545 iana-token / x-name characters.
func validName(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return name != ""
}

// hasControl: a control character other than HTAB, which RFC 5545 allows
// in no parameter or value (a bare CR would end a fragment line).
func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return true
		}
	}
	return false
}

// normalise validates and normalises one VEVENT. rid is its RECURRENCE-ID,
// nil for a master.
func normalise(c Component, n *notes) (we WriteEvent, rid *when, r *Refusal) {
	out := Component{Name: "VEVENT"}
	seen := map[string]bool{}
	for _, it := range c.Items {
		if it.Comp != nil {
			if !strings.EqualFold(it.Comp.Name, "VALARM") {
				return we, nil, refuse(BadData, "a VEVENT holds a component other than VALARM")
			}
			we.Alarms = append(we.Alarms, it.Comp.Clone())
			continue
		}
		p := it.Prop
		name := strings.ToUpper(p.Name)
		switch {
		case !validName(name) || name == "BEGIN" || name == "END":
			return we, nil, refuse(BadData, "a property name that is not valid")
		case hasControl(p.Params) || hasControl(p.Value):
			return we, nil, refuse(BadData, "a control character in a property")
		case name == "RDATE":
			return we, nil, refuse(RDate, "RDATE has no Proton equivalent")
		case name == "EXRULE":
			return we, nil, refuse(ExRule, "EXRULE has no Proton equivalent")
		case name == "COLOR":
			n.add(noteColor)
			continue
		case name == sessionKey || strings.HasPrefix(name, "X-PCB-"):
			n.add(noteOurs)
			continue
		case vcalendarLevel[name]:
			continue
		case name == "STATUS" && strings.EqualFold(strings.TrimSpace(p.Value), "TENTATIVE"):
			n.add(noteTentative)
			continue
		case name == "ATTENDEE":
			if params, dropped := dropParam(p.Params, "X-PM-TOKEN"); dropped {
				p.Params = params
				n.add(noteToken)
			}
		}
		if singleValued[name] {
			if seen[name] {
				return we, nil, refuse(BadData, name+" appears more than once")
			}
			seen[name] = true
		}
		out.add(p)
	}
	start, r := checkTimes(&out, n)
	if r != nil {
		return we, nil, r
	}
	we.AllDay = start.date
	if i := indexOf(out, "RECURRENCE-ID"); i >= 0 {
		w, _ := parseWhen(&out.Items[i].Prop, out.Items[i].Prop.Value, nil)
		rid = &w
	}
	isException := rid != nil
	if i := indexOf(out, "RRULE"); i >= 0 {
		if isException {
			return we, nil, refuse(UnsupportedRecurrence, "an exception with RRULE")
		}
		rule := &out.Items[i].Prop
		if v, ok := monthlyOrdinal(rule.Value); ok {
			rule.Value = v
			n.add(noteMonthly)
		}
		if v, ok := dailyByday(rule.Value); ok {
			rule.Value = v
			n.add(noteDaily)
		}
		if r := checkRRule(rule.Value, start); r != nil {
			return we, nil, r
		}
	}
	if _, ok := out.Get("EXDATE"); ok && isException {
		return we, nil, refuse(UnsupportedRecurrence, "an exception with EXDATE")
	}
	if seq, ok := out.Get("SEQUENCE"); ok && !isCount(seq.Value) {
		return we, nil, refuse(BadData, "SEQUENCE is not a number")
	}
	we.Comp = out
	return we, rid, nil
}

func indexOf(c Component, name string) int {
	for i, it := range c.Items {
		if it.Comp == nil && strings.EqualFold(it.Prop.Name, name) {
			return i
		}
	}
	return -1
}

// isCount: 1 to 9 decimal digits.
func isCount(s string) bool {
	if s == "" || len(s) > 9 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// checkTimes validates DTSTART (required), DTEND, RECURRENCE-ID and every
// EXDATE value, maps Mozilla TZIDs in place (n records it), and turns a
// DURATION into the DTEND it means, at the DURATION's place. It returns
// DTSTART.
func checkTimes(ev *Component, n *notes) (when, *Refusal) {
	var start, end *when
	dur := -1
	for i := range ev.Items {
		p := &ev.Items[i].Prop
		name := strings.ToUpper(p.Name)
		switch name {
		case "DTSTART", "DTEND", "RECURRENCE-ID", "EXDATE":
		case "DURATION":
			dur = i
			continue
		default:
			continue
		}
		if _, ok := param(p.Params, "RANGE"); ok && name == "RECURRENCE-ID" {
			return when{}, refuse(Range, "RECURRENCE-ID with RANGE has no Proton equivalent")
		}
		values := []string{p.Value}
		if name == "EXDATE" {
			values = strings.Split(p.Value, ",")
		}
		for _, v := range values {
			w, r := parseWhen(p, v, n)
			if r != nil {
				return when{}, r
			}
			switch name {
			case "DTSTART":
				start = &w
			case "DTEND":
				end = &w
			}
		}
	}
	if start == nil {
		return when{}, refuse(BadData, "a VEVENT without DTSTART")
	}
	if end != nil {
		if dur >= 0 {
			return when{}, refuse(BadData, "both DTEND and DURATION")
		}
		if end.date != start.date {
			return when{}, refuse(BadData, "DTEND is not of DTSTART's value type")
		}
		if end.t.Before(start.t) {
			return when{}, refuse(BadData, "DTEND is before DTSTART")
		}
	}
	if dur >= 0 {
		p, r := endOf(*start, ev.Items[dur].Prop.Value)
		if r != nil {
			return when{}, r
		}
		ev.Items[dur].Prop = p
		n.add(noteDuration)
	}
	return *start, nil
}

// when is a parsed DATE or DATE-TIME.
type when struct {
	t    time.Time
	date bool           // a DATE (t is midnight UTC)
	loc  *time.Location // the zone of a TZID time, nil for UTC or a DATE
	tzid string         // the TZID as written after mapping
}

const (
	dateLayout  = "20060102"
	localLayout = "20060102T150405"
)

// parseWhen reads value as the DATE or DATE-TIME property p says. A Mozilla
// TZID is replaced in p by the IANA name it carries, unless n is nil.
func parseWhen(p *Property, value string, n *notes) (when, *Refusal) {
	valueType, _ := param(p.Params, "VALUE")
	tzid, hasTZ := param(p.Params, "TZID")
	switch strings.ToUpper(valueType) {
	case "DATE":
		if hasTZ {
			return when{}, refuse(BadData, "a DATE with a TZID")
		}
		t, err := time.Parse(dateLayout, value)
		if err != nil || len(value) != 8 {
			return when{}, refuse(BadData, "a malformed DATE")
		}
		return when{t: t, date: true}, nil
	case "", "DATE-TIME":
	default:
		return when{}, refuse(BadData, "a time property with an unsupported VALUE")
	}
	if len(value) == 16 && value[15] == 'Z' {
		if hasTZ {
			return when{}, refuse(BadData, "a UTC time with a TZID")
		}
		t, err := time.Parse(localLayout, value[:15])
		if err != nil {
			return when{}, refuse(BadData, "a malformed DATE-TIME")
		}
		return when{t: t}, nil
	}
	if len(value) != 15 {
		return when{}, refuse(BadData, "a malformed DATE-TIME")
	}
	if !hasTZ {
		return when{}, refuse(Floating, "a floating DATE-TIME (no Z, no TZID)")
	}
	loc, name, ok := location(tzid)
	if !ok {
		return when{}, refuse(UnknownTZID, "a TZID that is not a known time zone")
	}
	if name != tzid && n != nil {
		p.Params = setParam(p.Params, "TZID", name)
		n.add(noteMozilla)
	}
	t, err := time.ParseInLocation(localLayout, value, loc)
	if err != nil {
		return when{}, refuse(BadData, "a malformed DATE-TIME")
	}
	return when{t: t, loc: loc, tzid: name}, nil
}

// location resolves a TZID with Go's zone database, after mapping
// Thunderbird's legacy /mozilla.org/<date>/<Area/City> to Area/City. ""
// and "Local" (the host's zone in Go) are not zones.
func location(tzid string) (*time.Location, string, bool) {
	name := tzid
	if rest, ok := strings.CutPrefix(name, "/mozilla.org/"); ok {
		_, after, ok := strings.Cut(rest, "/")
		if !ok {
			return nil, "", false
		}
		name = after
	}
	if name == "" || name == "Local" {
		return nil, "", false
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, "", false
	}
	return loc, name, true
}

// format writes t in w's form: a DATE, a local time of w's zone, or UTC.
func (w when) format(t time.Time) (params, value string) {
	switch {
	case w.date:
		return ";VALUE=DATE", t.Format(dateLayout)
	case w.loc != nil:
		return ";TZID=" + w.tzid, t.In(w.loc).Format(localLayout)
	}
	return "", t.UTC().Format(localLayout) + "Z"
}

// endOf is the DTEND a DURATION means from start, in start's form (RFC 5545
// §3.3.6: weeks and days are nominal, counted on the local calendar; hours,
// minutes and seconds are exact). A DATE start takes whole days or weeks
// only; a negative duration is refused.
func endOf(start when, value string) (Property, *Refusal) {
	d, ok := parseDuration(value)
	if !ok || d.neg {
		return Property{}, refuse(BadData, "DURATION is not a positive duration")
	}
	exact := time.Duration(d.hours)*time.Hour + time.Duration(d.minutes)*time.Minute + time.Duration(d.seconds)*time.Second
	if start.date && exact != 0 {
		return Property{}, refuse(BadData, "a DATE event with a DURATION in hours")
	}
	end := start.t.AddDate(0, 0, int(7*d.weeks+d.days)).Add(exact)
	params, v := start.format(end)
	return Property{Name: "DTEND", Params: params, Value: v}, nil
}

// duration is an RFC 5545 dur-value, by component.
type duration struct {
	neg                                  bool
	weeks, days, hours, minutes, seconds int64
}

// signed is the duration in seconds, negative before the start.
func (d duration) signed() int64 {
	s := d.weeks*7*86400 + d.days*86400 + d.hours*3600 + d.minutes*60 + d.seconds
	if d.neg {
		return -s
	}
	return s
}

// parseDuration reads [+|-]P(nW | [nD][T[nH][nM][nS]]): weeks alone, units
// in order, each at most once, at most 9 digits each, at least one unit, no
// bare T.
func parseDuration(s string) (duration, bool) {
	var d duration
	if s != "" && (s[0] == '+' || s[0] == '-') {
		d.neg = s[0] == '-'
		s = s[1:]
	}
	s, ok := strings.CutPrefix(s, "P")
	if !ok {
		return d, false
	}
	datePart, timePart, hasT := strings.Cut(s, "T")
	if hasT && timePart == "" {
		return d, false
	}
	dv, ok := durationUnits(datePart, "WD")
	if !ok || len(dv) == 2 {
		return d, false
	}
	tv, ok := durationUnits(timePart, "HMS")
	if !ok || len(dv)+len(tv) == 0 {
		return d, false
	}
	if _, weeks := dv['W']; weeks && hasT {
		return d, false
	}
	d.weeks, d.days = dv['W'], dv['D']
	d.hours, d.minutes, d.seconds = tv['H'], tv['M'], tv['S']
	return d, true
}

// durationUnits reads "<digits><unit>…" with units from allowed, in its order.
func durationUnits(s, allowed string) (map[byte]int64, bool) {
	out := map[byte]int64{}
	last := -1
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i > 9 || i == len(s) {
			return nil, false
		}
		pos := strings.IndexByte(allowed, s[i])
		if pos <= last {
			return nil, false
		}
		v, _ := strconv.ParseInt(s[:i], 10, 64)
		out[s[i]], last, s = v, pos, s[i+1:]
	}
	return out, true
}

// The RRULE limits (constraints, research E.1).
const maxCount = 49

var (
	untilLimit  = time.Date(2037, 12, 31, 0, 0, 0, 0, time.UTC)
	maxInterval = map[string]int{"DAILY": 999, "WEEKLY": 4999, "YEARLY": 99} // MONTHLY: no bound
	setPos      = map[string]bool{"-1": true, "1": true, "2": true, "3": true, "4": true}
	weekdays    = map[string]bool{"MO": true, "TU": true, "WE": true, "TH": true, "FR": true, "SA": true, "SU": true}
)

// checkRRule refuses an RRULE outside Proton's supported set: FREQ DAILY,
// WEEKLY, MONTHLY or YEARLY; COUNT ≤ 49; UNTIL on or before 2037-12-31 (the
// date in DTSTART's zone); not both COUNT and UNTIL; INTERVAL ≤ 999 daily,
// 4999 weekly, 99 yearly; besides FREQ, INTERVAL, COUNT, UNTIL and WKST
// (a weekday), only these parts (ruling M4-R10): WEEKLY BYDAY of plain
// weekdays; MONTHLY one BYMONTHDAY, or one plain BYDAY weekday with one
// BYSETPOS in {-1,1,2,3,4}; YEARLY one BYMONTH with one BYMONTHDAY. A
// malformed rule is BadData.
func checkRRule(rule string, start when) *Refusal {
	parts := map[string]string{}
	for kv := range strings.SplitSeq(rule, ";") {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.ToUpper(k)
		if !ok || k == "" || v == "" {
			return refuse(BadData, "a malformed RRULE")
		}
		if _, dup := parts[k]; dup {
			return refuse(BadData, "an RRULE part given twice")
		}
		parts[k] = v
	}
	freq, ok := parts["FREQ"]
	if !ok {
		return refuse(BadData, "an RRULE without FREQ")
	}
	freq = strings.ToUpper(freq)
	switch freq {
	case "DAILY", "WEEKLY", "MONTHLY", "YEARLY":
	default:
		return refuse(UnsupportedRecurrence, "RRULE FREQ is not DAILY, WEEKLY, MONTHLY or YEARLY")
	}
	if v, ok := parts["INTERVAL"]; ok {
		if !isCount(v) || atoi(v) == 0 {
			return refuse(BadData, "RRULE INTERVAL is not a positive number")
		}
		if limit, bounded := maxInterval[freq]; bounded && atoi(v) > limit {
			return refuse(UnsupportedRecurrence, fmt.Sprintf("RRULE INTERVAL above %d for %s", limit, freq))
		}
	}
	count, hasCount := parts["COUNT"]
	if hasCount {
		if !isCount(count) || atoi(count) == 0 {
			return refuse(BadData, "RRULE COUNT is not a positive number")
		}
		if atoi(count) > maxCount {
			return refuse(UnsupportedRecurrence, fmt.Sprintf("RRULE COUNT above %d", maxCount))
		}
	}
	if until, ok := parts["UNTIL"]; ok {
		if hasCount {
			return refuse(UnsupportedRecurrence, "RRULE with both COUNT and UNTIL")
		}
		day, r := untilDay(until, start)
		if r != nil {
			return r
		}
		if day.After(untilLimit) {
			return refuse(UnsupportedRecurrence, "RRULE UNTIL after 2037-12-31")
		}
	}
	for k := range parts {
		if !allowedParts[freq][k] {
			return refuse(UnsupportedRecurrence, "an RRULE part Proton does not support for this FREQ")
		}
	}
	if v, ok := parts["WKST"]; ok && !weekdays[strings.ToUpper(v)] {
		return refuse(UnsupportedRecurrence, "RRULE WKST is not a weekday")
	}
	switch freq {
	case "WEEKLY":
		if v, ok := parts["BYDAY"]; ok {
			for d := range strings.SplitSeq(v, ",") {
				if !weekdays[strings.ToUpper(d)] {
					return refuse(UnsupportedRecurrence, "WEEKLY RRULE BYDAY other than plain weekdays")
				}
			}
		}
	case "MONTHLY":
		if !monthlyShape(parts) {
			return refuse(UnsupportedRecurrence, "MONTHLY RRULE other than one BYMONTHDAY or one BYDAY with one BYSETPOS")
		}
	case "YEARLY":
		month, okMonth := parts["BYMONTH"]
		day, okDay := parts["BYMONTHDAY"]
		if okMonth != okDay || okMonth && (!inRange(month, 1, 12) || !monthDay(day)) {
			return refuse(UnsupportedRecurrence, "YEARLY RRULE other than none or one BYMONTH with one BYMONTHDAY")
		}
	}
	return nil
}

// allowedParts are the RRULE parts Proton supports per FREQ (ruling
// M4-R10); checkRRule checks their values.
var allowedParts = func() map[string]map[string]bool {
	set := func(extra ...string) map[string]bool {
		m := map[string]bool{"FREQ": true, "INTERVAL": true, "COUNT": true, "UNTIL": true, "WKST": true}
		for _, k := range extra {
			m[k] = true
		}
		return m
	}
	return map[string]map[string]bool{
		"DAILY":   set(),
		"WEEKLY":  set("BYDAY"),
		"MONTHLY": set("BYDAY", "BYSETPOS", "BYMONTHDAY"),
		"YEARLY":  set("BYMONTH", "BYMONTHDAY"),
	}
}()

// inRange: one integer from lo to hi (no sign, no list).
func inRange(v string, lo, hi int) bool {
	return isCount(v) && atoi(v) >= lo && atoi(v) <= hi
}

// monthDay: one day of the month, 1 to 31 or -31 to -1.
func monthDay(v string) bool {
	return inRange(strings.TrimPrefix(v, "-"), 1, 31)
}

// monthlyOrdinal writes a MONTHLY rule's BYDAY=<n><DAY> (n in 1..4 or -1,
// "+" allowed, one day, no BYSETPOS) in Proton's form BYDAY=<DAY>;BYSETPOS=<n>,
// in place (ruling M4-R5: same meaning, Thunderbird writes the first). Any
// other rule comes back as it is, and checkRRule judges it.
func monthlyOrdinal(rule string) (string, bool) {
	parts := strings.Split(rule, ";")
	at, monthly := -1, false
	for i, kv := range parts {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return rule, false
		}
		switch strings.ToUpper(k) {
		case "FREQ":
			monthly = strings.EqualFold(v, "MONTHLY")
		case "BYDAY":
			at = i
		case "BYSETPOS":
			return rule, false
		}
	}
	if !monthly || at < 0 {
		return rule, false
	}
	_, v, _ := strings.Cut(parts[at], "=")
	v = strings.ToUpper(v)
	if len(v) < 3 {
		return rule, false
	}
	pos, day := v[:len(v)-2], v[len(v)-2:]
	if rest, plus := strings.CutPrefix(pos, "+"); plus && rest != "-1" {
		pos = rest
	}
	if !weekdays[day] || !setPos[pos] {
		return rule, false
	}
	parts[at] = "BYDAY=" + day
	return strings.Join(slices.Insert(parts, at+1, "BYSETPOS="+pos), ";"), true
}

// dailyByday writes FREQ=DAILY with a BYDAY of plain weekdays and INTERVAL
// absent or 1 as the FREQ=WEEKLY rule it means, only FREQ's value changed
// (ruling M4-R12: Thunderbird's "every weekday" [inferred]). Any other rule
// comes back as it is: checkRRule refuses BYDAY on DAILY.
func dailyByday(rule string) (string, bool) {
	parts := strings.Split(rule, ";")
	freq, daily, days := -1, false, false
	for i, kv := range parts {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return rule, false
		}
		switch strings.ToUpper(k) {
		case "FREQ":
			freq, daily = i, strings.EqualFold(v, "DAILY")
		case "INTERVAL":
			if !isCount(v) || atoi(v) != 1 {
				return rule, false
			}
		case "BYDAY":
			days = true
			for d := range strings.SplitSeq(v, ",") {
				if !weekdays[strings.ToUpper(d)] {
					return rule, false
				}
			}
		}
	}
	if !daily || !days {
		return rule, false
	}
	k, _, _ := strings.Cut(parts[freq], "=")
	parts[freq] = k + "=WEEKLY"
	return strings.Join(parts, ";"), true
}

func atoi(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

// monthlyShape: no BY part; BYMONTHDAY alone with one day; or BYDAY with
// one plain weekday and BYSETPOS with one allowed value, alone.
func monthlyShape(parts map[string]string) bool {
	by := 0
	for k := range parts {
		if strings.HasPrefix(k, "BY") {
			by++
		}
	}
	weekday, okDay := parts["BYDAY"]
	pos, okPos := parts["BYSETPOS"]
	day, okMonthDay := parts["BYMONTHDAY"]
	switch {
	case by == 0:
		return true
	case by == 1 && okMonthDay:
		return monthDay(day)
	case by == 2 && okDay && okPos:
		return weekdays[strings.ToUpper(weekday)] && setPos[pos]
	}
	return false
}

// untilDay is the calendar date of an UNTIL: a DATE as written, a UTC time
// on DTSTART's local calendar (UTC for a UTC or DATE start).
func untilDay(v string, start when) (time.Time, *Refusal) {
	switch {
	case len(v) == 8:
		t, err := time.Parse(dateLayout, v)
		if err != nil {
			return time.Time{}, refuse(BadData, "a malformed RRULE UNTIL")
		}
		return t, nil
	case len(v) == 16 && v[15] == 'Z':
		t, err := time.Parse(localLayout, v[:15])
		if err != nil {
			return time.Time{}, refuse(BadData, "a malformed RRULE UNTIL")
		}
		if start.loc != nil {
			t = t.In(start.loc)
		}
		y, m, d := t.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, refuse(BadData, "RRULE UNTIL is not a DATE or a UTC DATE-TIME")
}

// param returns the value of parameter name in a raw parameter string
// (name case-insensitive, one pair of surrounding quotes removed).
func param(params, name string) (string, bool) {
	for _, p := range splitParams(params) {
		if k, v, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, name) {
			return unquote(v), true
		}
	}
	return "", false
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' && !strings.Contains(v[1:len(v)-1], `"`) {
		return v[1 : len(v)-1]
	}
	return v
}

// setParam replaces the value of parameter name, keeping the others and
// their order.
func setParam(params, name, value string) string {
	parts := splitParams(params)
	for i, p := range parts {
		if k, _, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, name) {
			parts[i] = k + "=" + value
		}
	}
	return ";" + strings.Join(parts, ";")
}

// dropParam removes every parameter called name.
func dropParam(params, name string) (string, bool) {
	var kept []string
	dropped := false
	for _, p := range splitParams(params) {
		if k, _, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, name) {
			dropped = true
			continue
		}
		kept = append(kept, p)
	}
	if !dropped {
		return params, false
	}
	if len(kept) == 0 {
		return "", true
	}
	return ";" + strings.Join(kept, ";"), true
}
