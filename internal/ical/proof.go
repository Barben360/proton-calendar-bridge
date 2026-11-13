package ical

import (
	"slices"
	"strings"
)

// EncryptedOnly reports whether the write routing table puts property name
// (any case) in encrypted parts only: never in a signed part, and neither UID
// nor DTSTAMP (in every part) nor a property never routed. It reads routeOf,
// so it follows the table. Seal and Patch keep every such property out of
// the parts we sign in clear (rulings M4-R16, M4-R19), and ProvesKey asks
// for one.
func EncryptedOnly(name string) bool {
	r := routeOf(strings.ToUpper(name))
	return len(r) > 0 && !slices.Contains(r, SharedSigned) && !slices.Contains(r, CalendarSigned)
}

// HoldsEncryptedOnly reports whether text, a card part's plaintext, parses as
// iCalendar and a VEVENT of it (top level, or in a top-level VCALENDAR) has
// an EncryptedOnly property of its own. A nested component's properties do
// not count. Text that does not parse holds none. Seal refuses to sign in
// clear a fragment for which it is true (ruling M4-R16).
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

// ProvesKey reports whether text, the plaintext of the part of kind k, can
// stand as proof that the session key it opened with is ours (rulings
// M4-R14, M4-R19). Only a shared or calendar encrypted part can prove: an
// attendees part's plaintext (UID, DTSTAMP, ATTENDEE with tokens) is known to
// the server. The text must be in Proton's fragment form (one VCALENDAR
// holding exactly one VEVENT, and no property of its own but VERSION, PRODID
// or CALSCALE, which carbonate and caloxide write, M4-R20), the
// VEVENT must have exactly one UID whose TEXT value (UnescapeText) is the
// row's uid byte for byte, uid being the UID as Proton stores it, unescaped
// (SPEC §5.3; so a proof made for another event cannot serve), and hold an EncryptedOnly
// property of its own that the routing table puts in k itself (so an
// attendees text replayed into the shared encrypted slot proves nothing).
//
// It is a secondary defence: what it rules out is a text the server holds
// with our signature and can replay under a key it chose, and the forms it
// refuses (cleartext signed parts, which never hold such a property; an .ics
// a mail client signed, which always carries METHOD; another UID's parts) are the
// ones it knows. It cannot rule out a plaintext the server guesses or learnt
// for the same UID; key-packet pinning is the primary defence (SPEC §5.3).
func ProvesKey(text []byte, uid string, k PartKind) bool {
	if k != SharedEncrypted && k != CalendarEncrypted {
		return false
	}
	top, err := Parse(text)
	if err != nil || uid == "" || len(top) != 1 || !strings.EqualFold(top[0].Name, "VCALENDAR") {
		return false
	}
	var ev *Component
	for _, it := range top[0].Items {
		switch {
		case it.Comp != nil && strings.EqualFold(it.Comp.Name, "VEVENT") && ev == nil:
			ev = it.Comp
		case it.Comp == nil && fragmentHeader(it.Prop.Name):
		default:
			return false
		}
	}
	if ev == nil {
		return false
	}
	var uids []string
	for _, p := range ev.Props() {
		if strings.EqualFold(p.Name, "UID") {
			uids = append(uids, p.Value)
		}
	}
	if len(uids) != 1 || UnescapeText(uids[0]) != uid {
		return false
	}
	return slices.ContainsFunc(ev.Props(), func(p Property) bool {
		return EncryptedOnly(p.Name) && slices.Contains(routeOf(strings.ToUpper(p.Name)), k)
	})
}

// fragmentHeader reports the VCALENDAR-level properties a proving fragment
// may carry: the headers carbonate and caloxide write into their fragments
// (research B.4), never METHOD (a mailed .ics always has one) nor anything
// else (ruling M4-R20).
func fragmentHeader(name string) bool {
	switch strings.ToUpper(name) {
	case "VERSION", "PRODID", "CALSCALE":
		return true
	}
	return false
}
