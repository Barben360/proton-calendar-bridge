package ical

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// PartKind is a card part's slot in a Proton event row, numbered as
// cards.Kind (without the personal part, which the bridge never writes).
type PartKind int

// The part kinds, in merge order.
const (
	SharedSigned PartKind = iota
	SharedEncrypted
	CalendarSigned
	CalendarEncrypted
	Attendees
)

var partKinds = []PartKind{SharedSigned, SharedEncrypted, CalendarSigned, CalendarEncrypted, Attendees}

func (k PartKind) String() string {
	switch k {
	case SharedSigned:
		return "shared signed part"
	case SharedEncrypted:
		return "shared encrypted part"
	case CalendarSigned:
		return "calendar signed part"
	case CalendarEncrypted:
		return "calendar encrypted part"
	case Attendees:
		return "attendees part"
	}
	return fmt.Sprintf("part kind %d", int(k))
}

// required parts are written even with nothing but UID and DTSTAMP, as the
// web client and our fake do; the calendar encrypted and attendees parts
// only when they carry something.
func (k PartKind) required() bool { return k <= CalendarSigned }

// routes is the write routing table (constraints, research B.1) for the
// properties that do not go to the shared encrypted part. UID and DTSTAMP
// go to every part. X-PM-CONFERENCE-ID is shared signed as in research B.1
// and the fake (the constraints' table omits it).
var routes = map[string][]PartKind{
	"DTSTART": {SharedSigned}, "DTEND": {SharedSigned}, "RECURRENCE-ID": {SharedSigned}, "RRULE": {SharedSigned},
	"SEQUENCE": {SharedSigned}, "ORGANIZER": {SharedSigned}, "X-PM-CONFERENCE-ID": {SharedSigned},
	"EXDATE": {SharedSigned, CalendarSigned},
	"STATUS": {CalendarSigned}, "TRANSP": {CalendarSigned},
	"COMMENT":  {CalendarEncrypted},
	"ATTENDEE": {Attendees},
}

func everyPart(name string) bool { return name == "UID" || name == "DTSTAMP" }

// ours are pcb's own markers and Proton-internal key material: never in a
// part we write, not even when a stored part holds them (ruling M4-R7).
func ours(name string) bool { return name == sessionKey || strings.HasPrefix(name, "X-PCB-") }

// neverRouted: what an incoming event never puts into a part. COLOR is the
// row's Color (a stored COLOR line is another client's, kept); DURATION is
// made DTEND by ParseWrite.
func neverRouted(name string) bool {
	return ours(name) || name == "COLOR" || name == "DURATION" || vcalendarLevel[name]
}

// routeOf is the parts a property goes to (name upper-case).
func routeOf(name string) []PartKind {
	switch {
	case everyPart(name):
		return partKinds
	case neverRouted(name):
		return nil
	}
	if r, ok := routes[name]; ok {
		return r
	}
	return []PartKind{SharedEncrypted}
}

// Route splits a normalised VEVENT (ParseWrite's) into card parts per the
// routing table, keeping its property order in each part. Nested
// components and never-routed properties are left out. The shared signed,
// shared encrypted and calendar signed parts are always there; the others
// only with a property besides UID and DTSTAMP.
func Route(ev Component) map[PartKind][]Property {
	content := map[PartKind][]Property{}
	has := map[PartKind]bool{}
	for _, p := range ev.Props() {
		name := strings.ToUpper(p.Name)
		for _, k := range routeOf(name) {
			content[k] = append(content[k], p)
			has[k] = has[k] || !everyPart(name)
		}
	}
	out := map[PartKind][]Property{}
	for _, k := range partKinds {
		if k.required() || has[k] {
			out[k] = content[k]
		}
	}
	return out
}

// Fragment serialises a part in Proton's form (constraints, research B.4):
// BEGIN:VCALENDAR, BEGIN:VEVENT, the lines folded at 75 octets, END:VEVENT,
// END:VCALENDAR, CRLF between lines and none after the last; no VERSION or
// PRODID.
func Fragment(props []Property) []byte {
	lines := make([]string, len(props))
	for i, p := range props {
		lines[i] = folded(p.Line())
	}
	return fragment(lines)
}

func fragment(lines []string) []byte {
	var b bytes.Buffer
	b.WriteString("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\n")
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\r\n")
	}
	b.WriteString("END:VEVENT\r\nEND:VCALENDAR")
	return b.Bytes()
}

// folded is line folded at 75 octets, without the final CRLF.
func folded(line string) string {
	b := foldLine(nil, line)
	return string(b[:len(b)-2])
}

// unit is a content line of a stored part's VEVENT as stored: raw is its
// physical lines joined by CRLF (folding kept), prop the unfolded property;
// a nested component is one block unit, raw from BEGIN to END.
type unit struct {
	raw   string
	prop  Property
	block bool
}

// veventUnits returns the units of the VEVENT of a stored part (the one
// MergeEvent reads), byte for byte; line ends are read as CRLF.
func veventUnits(b []byte) ([]unit, error) {
	comps, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if _, ok := findVEvent(comps); !ok {
		return nil, errors.New("no VEVENT")
	}
	type logical struct {
		phys []string
		text strings.Builder
	}
	var lines []*logical
	for len(b) > 0 {
		var line []byte
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i], b[i+1:]
		} else {
			line, b = b, nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		switch {
		case len(line) == 0:
		case line[0] == ' ' || line[0] == '\t':
			l := lines[len(lines)-1] // Parse refused a continuation with nothing to continue
			l.phys = append(l.phys, string(line))
			l.text.Write(line[1:])
		default:
			l := &logical{phys: []string{string(line)}}
			l.text.Write(line)
			lines = append(lines, l)
		}
	}
	var out []unit
	depth, inner := 0, 0
	inside := false
	for _, l := range lines {
		raw := strings.Join(l.phys, "\r\n")
		p, _ := splitLine(l.text.String()) // Parse accepted every line
		begin := strings.EqualFold(p.Name, "BEGIN") && p.Params == ""
		end := strings.EqualFold(p.Name, "END") && p.Params == ""
		if !inside {
			switch {
			case begin && depth <= 1 && strings.EqualFold(strings.TrimSpace(p.Value), "VEVENT"):
				inside = true
			case begin:
				depth++
			case end:
				depth--
			}
			continue
		}
		switch {
		case end && inner == 0: // END:VEVENT
			return out, nil
		case inner > 0:
			out[len(out)-1].raw += "\r\n" + raw
			if begin {
				inner++
			} else if end {
				inner--
			}
		case begin:
			out = append(out, unit{raw: raw, block: true})
			inner = 1
		default:
			out = append(out, unit{raw: raw, prop: p})
		}
	}
	return out, nil
}

// identity is what makes two lines the same line: the name (case-folded),
// the parameters as a sorted set (names case-folded, one pair of quotes
// removed, enumerated values case-folded) and the value byte for byte.
// Folding never counts. An ATTENDEE is identified by its address alone:
// Thunderbird re-sends it with the PARTSTAT we injected and without its
// X-PM-TOKEN, and the stored line is the truth.
func identity(p Property) string {
	name := strings.ToUpper(p.Name)
	if name == "ATTENDEE" {
		return "ATTENDEE:" + attendeeAddress(p.Value)
	}
	return name + canonParams(p.Params) + ":" + p.Value
}

func attendeeAddress(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// enumerated are the parameters whose values are case-insensitive tokens
// (RFC 5545 §3.2).
var enumerated = map[string]bool{
	"CUTYPE": true, "ENCODING": true, "FBTYPE": true, "PARTSTAT": true, "RANGE": true, "RELATED": true,
	"RELTYPE": true, "ROLE": true, "RSVP": true, "VALUE": true,
}

func canonParams(raw string) string {
	parts := splitParams(raw)
	for i, p := range parts {
		k, v, ok := strings.Cut(p, "=")
		k = strings.ToUpper(k)
		if !ok {
			parts[i] = k
			continue
		}
		v = unquote(v)
		if enumerated[k] {
			v = strings.ToUpper(v)
		}
		parts[i] = k + "=" + v
	}
	slices.Sort(parts)
	return ";" + strings.Join(parts, ";")
}

// entry is a stored unit during a patch.
type entry struct {
	unit
	name, key string
	keep      bool   // in the output
	repl      string // the replacing line, folded; "" keeps raw
}

// Patch writes an incoming VEVENT over the current parts of its row
// (the decrypted fragments by kind, as Proton stores them), line by line:
//   - a multi-valued line whose identity (see identity) is stored keeps
//     its part(s) and its stored bytes; lines compare as multisets, so a
//     line sent n times keeps n stored copies per part;
//   - a single-valued property is compared with the copy the client was
//     served, the first stored one in part order (MergeEvent's rule): the
//     same, every stored copy stays as it is, even one that differs in
//     another part (DTSTAMP per part); another value replaces the first
//     copy in place in every part holding one (DTSTAMP everywhere, a
//     SUMMARY where it is, even off-table), further copies go;
//   - a stored line the event no longer carries is removed, from every
//     part (an EXDATE from both signed parts), except nested components
//     and COLOR lines, another client's data that an incoming event never
//     writes, kept verbatim; stored X-PCB-* and X-PM-SESSION-KEY lines are
//     always removed (ruling M4-R7);
//   - a new line goes where Route says, after the part's last property
//     (before END:VEVENT and any nested component);
//   - a part missing UID or DTSTAMP gets the incoming ones first.
//
// Never-routed properties and nested components of incoming are ignored.
// The output is in Proton's fragment form (Fragment): the VCALENDAR
// wrapper is rewritten, VERSION, PRODID and other VCALENDAR-level content
// dropped, line ends made CRLF; every kept VEVENT line keeps its bytes.
// A required part missing from current is created; an optional one
// (calendar encrypted, attendees) appears with its first line and goes
// when the patch empties it. With no current part, this is a create:
// Fragment of Route. changed[k] is true when out[k] differs from
// current[k] in bytes or presence: an unchanged part may be sent as
// stored, with its stored signature. Errors name the part, never its
// content.
func Patch(current map[PartKind][]byte, incoming Component) (map[PartKind][]byte, map[PartKind]bool, error) {
	var in []Property
	for _, p := range incoming.Props() {
		if !neverRouted(strings.ToUpper(p.Name)) {
			in = append(in, p)
		}
	}
	out := map[PartKind][]byte{}
	changed := map[PartKind]bool{}
	if len(current) == 0 {
		for k, props := range Route(Component{Name: "VEVENT", Items: items(in)}) {
			out[k], changed[k] = Fragment(props), true
		}
		return out, changed, nil
	}
	stored, err := storedEntries(current)
	if err != nil {
		return nil, nil, err
	}
	free := func(k PartKind, match func(*entry) bool) *entry {
		for _, e := range stored[k] {
			if !e.block && !e.keep && match(e) {
				return e
			}
		}
		return nil
	}
	placed := make([]bool, len(in))
	for i, p := range in { // single-valued: the client saw the first stored copy
		name, key := strings.ToUpper(p.Name), identity(p)
		if !singleValued[name] {
			continue
		}
		var served *entry
		for _, k := range partKinds {
			for _, e := range stored[k] {
				if served == nil && !e.block && e.name == name {
					served = e
				}
			}
		}
		if served == nil {
			continue
		}
		placed[i] = true
		for _, k := range partKinds {
			for _, e := range stored[k] {
				if !e.block && e.name == name && served.key == key {
					e.keep = true // unedited: every copy stays, with its bytes
				}
			}
			if served.key == key {
				continue
			}
			if e := free(k, func(e *entry) bool { return e.name == name }); e != nil {
				e.keep = true // edited: replaced in place, wherever it lives
				if e.key != key {
					e.repl = folded(p.Line())
				}
			}
		}
	}
	for i, p := range in { // multi-valued: kept where stored, as a multiset
		key := identity(p)
		if singleValued[strings.ToUpper(p.Name)] {
			continue
		}
		for _, k := range partKinds {
			if e := free(k, func(e *entry) bool { return e.key == key }); e != nil {
				e.keep, placed[i] = true, true
			}
		}
	}
	adds := map[PartKind][]string{}
	var common []Property
	for i, p := range in {
		name := strings.ToUpper(p.Name)
		switch {
		case everyPart(name):
			common = append(common, p)
		case !placed[i]:
			for _, k := range routeOf(name) {
				adds[k] = append(adds[k], folded(p.Line()))
			}
		}
	}
	for _, k := range partKinds {
		es, had := stored[k]
		lines, isBlock, content, hadContent := []string{}, []bool{}, false, false
		present := map[string]bool{}
		for _, e := range es {
			if !e.keep && (e.block || e.name == "COLOR") {
				e.keep = true
			}
			hadContent = hadContent || !everyPart(e.name)
			if !e.keep {
				continue
			}
			line := e.raw
			if e.repl != "" {
				line = e.repl
			}
			lines, isBlock = append(lines, line), append(isBlock, e.block)
			present[e.name] = true
			content = content || !everyPart(e.name)
		}
		var head []string
		for _, p := range common {
			if name := strings.ToUpper(p.Name); !present[name] {
				head, present[name] = append(head, folded(p.Line())), true
			}
		}
		at := len(lines)
		for at > 0 && isBlock[at-1] {
			at--
		}
		lines = slices.Concat(head, lines[:at], adds[k], lines[at:])
		content = content || len(adds[k]) > 0
		switch {
		case had && !k.required() && hadContent && !content: // emptied
		case had || k.required() || content:
			out[k] = fragment(lines)
		}
		if b, ok := out[k]; ok != had || !bytes.Equal(b, current[k]) {
			changed[k] = true
		} else if had {
			changed[k] = false
		}
	}
	return out, changed, nil
}

func items(props []Property) []Item {
	out := make([]Item, len(props))
	for i, p := range props {
		out[i] = Item{Prop: p}
	}
	return out
}

// storedEntries reads every current part's VEVENT units.
func storedEntries(current map[PartKind][]byte) (map[PartKind][]*entry, error) {
	stored := map[PartKind][]*entry{}
	for _, k := range partKinds {
		raw, ok := current[k]
		if !ok {
			continue
		}
		units, err := veventUnits(raw)
		if err != nil {
			return nil, fmt.Errorf("ical: %s: %w", k, err)
		}
		es := make([]*entry, len(units))
		for i, u := range units {
			es[i] = &entry{unit: u}
			if !u.block {
				es[i].name, es[i].key = strings.ToUpper(u.prop.Name), identity(u.prop)
			}
		}
		stored[k] = es
	}
	return stored, nil
}

// AttendeeChanges compares the attendees of the current parts (none on a
// create) with the incoming event's, by address (case-folded), as sets:
// what Patch would add and remove. The write planner refuses either
// (constraints).
func AttendeeChanges(current map[PartKind][]byte, incoming Component) (added, removed []string, err error) {
	stored, err := storedEntries(current)
	if err != nil {
		return nil, nil, err
	}
	var have []string
	for _, k := range partKinds {
		for _, e := range stored[k] {
			if e.name == "ATTENDEE" {
				have = append(have, attendeeAddress(e.prop.Value))
			}
		}
	}
	var want []string
	for _, p := range incoming.Props() {
		if strings.EqualFold(p.Name, "ATTENDEE") {
			want = append(want, attendeeAddress(p.Value))
		}
	}
	for _, a := range want {
		if !slices.Contains(have, a) && !slices.Contains(added, a) {
			added = append(added, a)
		}
	}
	for _, a := range have {
		if !slices.Contains(want, a) && !slices.Contains(removed, a) {
			removed = append(removed, a)
		}
	}
	return added, removed, nil
}

// IsSharedSignedChange reports whether the incoming event changes the
// shared signed content that decides a SEQUENCE bump (ruling M4-R4):
// DTSTART, DTEND, RRULE, EXDATE or RECURRENCE-ID, by meaning. DTSTART,
// DTEND and RECURRENCE-ID compare as a DATE, or as an instant with its zone
// (UTC or the TZID); EXDATE values as a set of DATEs or instants, whatever
// the zone they are written in or the lines they are split into; a stored
// DURATION counts as the DTEND it means; RRULE parts compare in any order,
// INTERVAL=1 as absent. DTSTAMP, SEQUENCE, ORGANIZER, text and alarms do
// not count.
func IsSharedSignedChange(stored, incoming Component) bool {
	return !slices.Equal(signedKeys(stored), signedKeys(incoming))
}

func signedKeys(c Component) []string {
	c = c.Clone()
	_, _ = checkTimes(&c, &notes{}) // DURATION → DTEND when it can; else the DURATION line counts
	var out []string
	for _, p := range c.Props() {
		name := strings.ToUpper(p.Name)
		switch name {
		case "DTSTART", "DTEND", "RECURRENCE-ID":
			out = append(out, name+":"+instantKey(p, p.Value, true))
		case "DURATION":
			out = append(out, identity(p))
		case "RRULE":
			parts := slices.DeleteFunc(strings.Split(strings.ToUpper(p.Value), ";"), func(s string) bool { return s == "INTERVAL=1" })
			slices.Sort(parts)
			out = append(out, "RRULE:"+strings.Join(parts, ";"))
		case "EXDATE":
			for v := range strings.SplitSeq(p.Value, ",") {
				key := "EXDATE:" + instantKey(p, v, false)
				if !slices.Contains(out, key) {
					out = append(out, key)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// instantKey is a time value's meaning: a DATE as written, else its Unix
// instant, with the zone (UTC or the IANA name) when withZone. A value that
// does not parse compares as written.
func instantKey(p Property, v string, withZone bool) string {
	w, r := parseWhen(&p, v, nil)
	switch {
	case r != nil:
		return "?" + canonParams(p.Params) + ":" + v
	case w.date:
		return "DATE:" + v
	case withZone && w.loc != nil:
		return "TZ:" + w.tzid + ":" + strconv.FormatInt(w.t.Unix(), 10)
	case withZone:
		return "UTC:" + strconv.FormatInt(w.t.Unix(), 10)
	}
	return "AT:" + strconv.FormatInt(w.t.Unix(), 10)
}

// AttendeeEdits names (by address, case-folded) the attendees the
// incoming event keeps with other parameters than we served them: the
// first stored line of the address as ApplyRow renders it (X-PM-TOKEN
// consumed, PARTSTAT from statuses, by token), against the incoming line
// without X-PM-TOKEN, parameters compared as in identity. Ruling M4-R9:
// v1 does not write such edits (ROLE, CN, PARTSTAT, RSVP): Patch keeps the
// stored line and the planner logs these at debug.
func AttendeeEdits(current map[PartKind][]byte, incoming Component, statuses map[string]int) ([]string, error) {
	stored, err := storedEntries(current)
	if err != nil {
		return nil, err
	}
	served := map[string]string{}
	for _, k := range partKinds {
		for _, e := range stored[k] {
			if a := attendeeAddress(e.prop.Value); e.name == "ATTENDEE" && served[a] == "" {
				served[a] = canonParams(joinStatus(e.prop.Params, statuses))
			}
		}
	}
	var edits []string
	for _, p := range incoming.Props() {
		if !strings.EqualFold(p.Name, "ATTENDEE") {
			continue
		}
		a := attendeeAddress(p.Value)
		params, _ := dropParam(p.Params, "X-PM-TOKEN")
		if was, ok := served[a]; ok && was != canonParams(params) && !slices.Contains(edits, a) {
			edits = append(edits, a)
		}
	}
	return edits, nil
}
