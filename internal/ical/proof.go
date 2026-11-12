package ical

import (
	"slices"
	"strings"
)

// EncryptedOnly reports whether the write routing table puts property name
// (any case) in encrypted parts only: never in a signed part, and neither UID
// nor DTSTAMP (in every part) nor a property never routed. It reads routeOf,
// so it follows the table.
//
// It is the proof SessionKeys asks of a reused key (ruling M4-R14). The
// premise: the server holds the plaintext of our cleartext signed parts with
// our signatures, and Patch and Seal keep every such property out of them
// (ruling M4-R16), so no cleartext pair of ours can serve as proof. It is
// not airtight (ruling M4-R17): an encrypted part's detached signature is
// sent in clear, so the server can confirm a guess of a low-entropy
// plaintext (a default title, attendees it already knows) offline with our
// public key, and replay it. See SPEC §5.3 and OPEN.md for the mitigation
// and the fix.
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
