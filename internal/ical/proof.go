package ical

import (
	"slices"
	"strings"
)

// EncryptedOnly reports whether the write routing table puts property name
// (any case) in encrypted parts only: never in a signed part, and neither UID
// nor DTSTAMP (in every part) nor a property never routed. A cleartext signed
// part of ours never holds one, so the server holds no signature of ours over
// such text (ruling M4-R14). It reads routeOf, so it follows the table.
func EncryptedOnly(name string) bool {
	r := routeOf(strings.ToUpper(name))
	return len(r) > 0 && !slices.Contains(r, SharedSigned) && !slices.Contains(r, CalendarSigned)
}

// HoldsEncryptedOnly reports whether text, a card part's plaintext, parses as
// iCalendar and a VEVENT of it (top level, or in a top-level VCALENDAR) has
// an EncryptedOnly property of its own. A nested component's properties do
// not count. Text that does not parse holds none.
func HoldsEncryptedOnly(text []byte) bool {
	top, err := Parse(text)
	if err != nil {
		return false
	}
	var evs []Component
	for _, c := range top {
		switch {
		case strings.EqualFold(c.Name, "VEVENT"):
			evs = append(evs, c)
		case strings.EqualFold(c.Name, "VCALENDAR"):
			evs = append(evs, c.Subs("VEVENT")...)
		}
	}
	for _, ev := range evs {
		if slices.ContainsFunc(ev.Props(), func(p Property) bool { return EncryptedOnly(p.Name) }) {
			return true
		}
	}
	return false
}
