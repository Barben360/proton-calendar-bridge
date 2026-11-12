package ical

import "testing"

// EncryptedOnly names the properties the routing table never puts in a
// signed part (ruling M4-R14): what a cleartext signed part of ours cannot
// hold, so the server has no signature of ours over them.
func TestEncryptedOnlyFollowsTheRoutingTable(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"SUMMARY": true, "DESCRIPTION": true, "LOCATION": true, "X-MOZ-GENERATION": true,
		"COMMENT": true, "ATTENDEE": true, "attendee": true, "Summary": true, "CREATED": true,
		// in every part
		"UID": false, "DTSTAMP": false,
		// signed parts
		"DTSTART": false, "DTEND": false, "RECURRENCE-ID": false, "RRULE": false, "EXDATE": false,
		"SEQUENCE": false, "ORGANIZER": false, "X-PM-CONFERENCE-ID": false, "STATUS": false, "TRANSP": false,
		"dtstart": false,
		// never routed
		"COLOR": false, "DURATION": false, "X-PCB-INTEGRITY": false, "X-PM-SESSION-KEY": false, "VERSION": false,
	} {
		if got := EncryptedOnly(name); got != want {
			t.Errorf("EncryptedOnly(%q) = %v, want %v", name, got, want)
		}
	}
}

// HoldsEncryptedOnly reports whether a part's text is a VEVENT holding an
// EncryptedOnly property: a signed fragment of ours never does.
func TestHoldsEncryptedOnly(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		text []byte
		want bool
	}{
		{"UID and DTSTAMP only", frag("UID:a", "DTSTAMP:20261111T090000Z"), false},
		{"a shared signed fragment", frag("UID:a", "DTSTAMP:20261111T090000Z", "DTSTART:20261112T090000Z", "SEQUENCE:0", "ORGANIZER:mailto:a@b"), false},
		{"a calendar signed fragment", frag("UID:a", "DTSTAMP:20261111T090000Z", "STATUS:CONFIRMED", "EXDATE:20261119T090000Z"), false},
		{"a SUMMARY", frag("UID:a", "DTSTAMP:20261111T090000Z", "SUMMARY:x"), true},
		{"a COMMENT", frag("UID:a", "DTSTAMP:20261111T090000Z", "COMMENT:x"), true},
		{"an ATTENDEE", frag("UID:a", "DTSTAMP:20261111T090000Z", "ATTENDEE;CN=A:mailto:a@b"), true},
		{"a folded SUMMARY", frag("UID:a", "DTSTAMP:20261111T090000Z", "SUMM", " ARY:x"), true},
		{"a SUMMARY only in a nested VALARM", frag("UID:a", "DTSTAMP:20261111T090000Z", "BEGIN:VALARM", "SUMMARY:x", "END:VALARM"), false},
		{"a SUMMARY at VCALENDAR level", []byte("BEGIN:VCALENDAR\r\nSUMMARY:x\r\nBEGIN:VEVENT\r\nUID:a\r\nEND:VEVENT\r\nEND:VCALENDAR"), false},
		{"a bare VEVENT with a SUMMARY", []byte("BEGIN:VEVENT\r\nUID:a\r\nSUMMARY:x\r\nEND:VEVENT"), true},
		{"not iCalendar", []byte("passphrase-bytes"), false},
		{"unclosed", []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:x"), false},
	} {
		if got := HoldsEncryptedOnly(c.text); got != c.want {
			t.Errorf("%s: HoldsEncryptedOnly = %v, want %v", c.name, got, c.want)
		}
	}
}
