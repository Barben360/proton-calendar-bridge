package ical

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// singleValued are the properties that appear once on a VEVENT: the first one
// seen, in part order, wins. The structural ones (UID, DTSTAMP, DTSTART, DTEND,
// DURATION, RRULE, RECURRENCE-ID, SEQUENCE) therefore come from the shared
// signed part, which is first. Every other property (ATTENDEE, EXDATE, RDATE,
// CATEGORIES, COMMENT, X-*, unknown ones) is a union without exact-duplicate
// lines.
var singleValued = map[string]bool{
	"UID": true, "DTSTAMP": true, "DTSTART": true, "DTEND": true, "DURATION": true,
	"RRULE": true, "RECURRENCE-ID": true, "SEQUENCE": true,
	"SUMMARY": true, "DESCRIPTION": true, "LOCATION": true, "STATUS": true, "CLASS": true,
	"TRANSP": true, "ORGANIZER": true, "URL": true, "CREATED": true, "LAST-MODIFIED": true,
	"GEO": true, "PRIORITY": true, "COLOR": true,
}

// sessionKey is Proton-internal key material, never served.
const sessionKey = "X-PM-SESSION-KEY"

// findVEvent returns the first VEVENT of a parsed part, whether the part is a
// VCALENDAR wrapper or a bare VEVENT.
func findVEvent(comps []Component) (Component, bool) {
	for _, c := range comps {
		if strings.EqualFold(c.Name, "VEVENT") {
			return c, true
		}
		if subs := c.Subs("VEVENT"); len(subs) > 0 {
			return subs[0], true
		}
	}
	return Component{}, false
}

// MergeEvent merges the decrypted card parts, in part order (shared signed
// first), into one VEVENT. Nested components are kept verbatim, X-PM-SESSION-KEY
// is dropped.
func MergeEvent(parts [][]byte) (Component, error) {
	if len(parts) == 0 {
		return Component{}, errors.New("ical: no parts to merge")
	}
	out := Component{Name: "VEVENT"}
	seenSingle := map[string]bool{}
	seenLine := map[string]bool{}
	for i, raw := range parts {
		comps, err := Parse(raw)
		if err != nil {
			return Component{}, fmt.Errorf("ical: part %d: %w", i, err)
		}
		ev, ok := findVEvent(comps)
		if !ok {
			return Component{}, fmt.Errorf("ical: part %d has no VEVENT", i)
		}
		for _, it := range ev.Items {
			if it.Comp != nil {
				if hasControl(it.Comp.Name) {
					continue
				}
				sub := it.Comp.Clone()
				sub.stripControls()
				out.Items = append(out.Items, Item{Comp: &sub})
				continue
			}
			if hasControl(it.Prop.Name) {
				continue // the rule of Component.stripControls
			}
			it.Prop = it.Prop.stripControls()
			name := strings.ToUpper(it.Prop.Name)
			if name == sessionKey {
				continue
			}
			if singleValued[name] {
				if seenSingle[name] {
					continue
				}
				seenSingle[name] = true
			} else {
				line := it.Prop.Line()
				if seenLine[line] {
					continue
				}
				seenLine[line] = true
			}
			out.Items = append(out.Items, it)
		}
	}
	return out, nil
}

// Notification is one entry of a row's Notifications (Type 0 = email, 1 = display).
type notification struct {
	Type    *int
	Trigger string
}

var durationRE = regexp.MustCompile(`^[+-]?P(?:\d+W|\d+D(?:T(?:\d+H)?(?:\d+M)?(?:\d+S)?)?|T(?:\d+H)?(?:\d+M)?(?:\d+S)?)$`)

// parseNotifications decodes a notification list. absent is true for a missing
// or JSON null value (the tri-state's "use the calendar default"). A
// malformed entry is an error, or, with dropped non-nil, left out and counted
// in *dropped.
func parseNotifications(raw json.RawMessage, dropped *int) (list []notification, absent bool, err error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, true, nil
	}
	var all []notification
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, false, errors.New("ical: notifications are not a list")
	}
	for _, n := range all {
		var bad error
		switch {
		case n.Type == nil:
			bad = errors.New("ical: notification without Type")
		case !durationRE.MatchString(n.Trigger) || strings.HasSuffix(n.Trigger, "T"):
			bad = errors.New("ical: notification Trigger is not an iCalendar duration")
		}
		switch {
		case bad == nil:
			list = append(list, n)
		case dropped == nil:
			return nil, false, bad
		default:
			*dropped++
		}
	}
	return list, false, nil
}

// Alarms renders a row's notifications as VALARMs. Tri-state: null (or absent)
// means the calendar's default list (defaults, which the caller picks as the
// part-day or full-day one), [] means none, an array is custom. Type 0 is an
// EMAIL alarm, anything else DISPLAY; both carry a DESCRIPTION, EMAIL also a
// SUMMARY. allDay only selects the wording.
func Alarms(notifications, defaults json.RawMessage, allDay bool) ([]Component, error) {
	out, err := alarms(notifications, defaults, allDay, nil)
	return out, err
}

// alarms is Alarms; with dropped non-nil it leaves a malformed reminder out
// and counts it in *dropped instead of failing.
func alarms(notifications, defaults json.RawMessage, allDay bool, dropped *int) ([]Component, error) {
	list, absent, err := parseNotifications(notifications, dropped)
	if err != nil {
		return nil, err
	}
	if absent {
		if list, _, err = parseNotifications(defaults, dropped); err != nil {
			return nil, err
		}
	}
	text := "Reminder"
	if allDay {
		text = "Reminder (all-day event)"
	}
	out := make([]Component, 0, len(list))
	for _, n := range list {
		action := "DISPLAY"
		if *n.Type == 0 {
			action = "EMAIL"
		}
		a := Component{Name: "VALARM"}
		a.add(Property{Name: "ACTION", Value: action})
		a.add(Property{Name: "TRIGGER", Value: n.Trigger})
		a.add(Property{Name: "DESCRIPTION", Value: text})
		if action == "EMAIL" {
			a.add(Property{Name: "SUMMARY", Value: text})
		}
		out = append(out, a)
	}
	return out, nil
}

// dropControls removes the C0 controls but HTAB, and DEL, from s: a bare CR
// (unfold keeps one inside a line) must not reach a client that splits
// lines on it (audit read-M2). TEXT's CR and LF are escaped ("\\n") and
// untouched.
func dropControls(s string) string {
	if !strings.ContainsFunc(s, isControl) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f }

func (p Property) stripControls() Property {
	return Property{Name: p.Name, Params: dropControls(p.Params), Value: dropControls(p.Value)}
}

// stripControls drops the controls from the parameters and values of c's
// properties and, recursively, its nested components, and drops every
// property and component whose name holds one: stripping would make another
// name of it (an empty one, BEGIN, END) and break the output's nesting. c's
// own name is the caller's to check. It filters c.Items in place: c must be
// a private copy (Clone).
func (c *Component) stripControls() {
	kept := c.Items[:0]
	for _, it := range c.Items {
		switch {
		case it.Comp != nil && hasControl(it.Comp.Name), it.Comp == nil && hasControl(it.Prop.Name):
			continue
		case it.Comp != nil:
			it.Comp.stripControls()
		default:
			it.Prop = it.Prop.stripControls()
		}
		kept = append(kept, it)
	}
	c.Items = kept
}

func (c *Component) add(p Property) { c.Items = append(c.Items, Item{Prop: p}) }

// addProp inserts a property after the last existing one, before nested components.
func (c *Component) addProp(p Property) {
	at := c.firstSub()
	c.Items = append(c.Items, Item{})
	copy(c.Items[at+1:], c.Items[at:])
	c.Items[at] = Item{Prop: p}
}

// ApplyNotifications makes the row's Notifications the source of truth for the
// alarms: when the row carries them (nil means the row has none; JSON null
// counts as present and selects the defaults) every VALARM of the parts is
// replaced by the rendered ones; otherwise the event is left alone. A
// reminder without Type or with a Trigger that is no duration is left out
// and counted in dropped (the event still renders, audit read-M4); only a
// value that is no list fails.
func ApplyNotifications(ev *Component, notifications, defaults json.RawMessage, allDay bool) (dropped int, err error) {
	if len(strings.TrimSpace(string(notifications))) == 0 {
		return 0, nil
	}
	alarms, err := alarms(notifications, defaults, allDay, &dropped)
	if err != nil {
		return 0, err
	}
	kept := ev.Items[:0:0]
	for _, it := range ev.Items {
		if it.Comp != nil && strings.EqualFold(it.Comp.Name, "VALARM") {
			continue
		}
		kept = append(kept, it)
	}
	ev.Items = kept
	for i := range alarms {
		ev.Items = append(ev.Items, Item{Comp: &alarms[i]})
	}
	return dropped, nil
}

var partstat = map[int]string{0: "NEEDS-ACTION", 1: "TENTATIVE", 2: "DECLINED", 3: "ACCEPTED"}

// ApplyRow renders the row-level data into the VEVENT: Color replaces any
// COLOR of the parts (an empty color leaves them), and each ATTENDEE's
// X-PM-TOKEN is consumed: PARTSTAT is set from statuses[token] and the
// Proton-internal parameter is dropped. ATTENDEE lines repeating an earlier
// address (case-insensitive) are dropped.
func ApplyRow(ev *Component, color string, statuses map[string]int) {
	if color != "" && !strings.ContainsAny(color, "\r\n") {
		color = dropControls(color)
		ev.removeProps(func(p Property) bool { return strings.EqualFold(p.Name, "COLOR") })
		ev.addProp(Property{Name: "COLOR", Value: color})
	}
	for i, it := range ev.Items {
		if it.Comp == nil && strings.EqualFold(it.Prop.Name, "ATTENDEE") {
			ev.Items[i].Prop.Params = joinStatus(it.Prop.Params, statuses)
		}
	}
	seen := map[string]bool{}
	ev.removeProps(func(p Property) bool {
		if !strings.EqualFold(p.Name, "ATTENDEE") {
			return false
		}
		k := strings.ToLower(strings.TrimSpace(p.Value))
		dup := seen[k]
		seen[k] = true
		return dup
	})
}

// splitParams splits a raw ";a=b;c=\"d;e\"" parameter string on the ";" outside quotes.
func splitParams(raw string) []string {
	var out []string
	inQuote := false
	start := 1 // skip the leading ";"
	if raw == "" {
		return nil
	}
	for i := 1; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			inQuote = !inQuote
		case ';':
			if !inQuote {
				out = append(out, raw[start:i])
				start = i + 1
			}
		}
	}
	return append(out, raw[start:])
}

func joinStatus(params string, statuses map[string]int) string {
	parts := splitParams(params)
	token, found := "", false
	kept := parts[:0:0]
	for _, p := range parts {
		if k, v, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, "X-PM-TOKEN") {
			token, found = strings.Trim(v, `"`), true
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return params
	}
	if st, ok := statuses[token]; ok {
		if name, ok := partstat[st]; ok {
			set := false
			for i, p := range kept {
				if k, _, ok := strings.Cut(p, "="); ok && strings.EqualFold(k, "PARTSTAT") {
					kept[i], set = "PARTSTAT="+name, true
					break
				}
			}
			if !set {
				kept = append(kept, "PARTSTAT="+name)
			}
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return ";" + strings.Join(kept, ";")
}

// integrityProp marks an event a signed part of which did not verify (SPEC
// §5.3, ruling M3-R33).
const integrityProp = "X-PCB-INTEGRITY"

// MarkIntegrity sets ev's integrity marker, which is pcb's alone: every
// X-PCB-INTEGRITY the parts carry is dropped (a part cannot vouch for
// itself, and one written back by a client says nothing of now), then
// X-PCB-INTEGRITY:unverified is added after the last property when
// unverified.
func MarkIntegrity(ev *Component, unverified bool) {
	ev.removeProps(func(p Property) bool { return strings.EqualFold(p.Name, integrityProp) })
	if unverified {
		ev.addProp(Property{Name: integrityProp, Value: "unverified"})
	}
}
