package ical

import (
	"bytes"
	"maps"
	"strings"
	"testing"
)

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

// signedClean fails when a signed part of out holds an encrypted-only
// property: a cleartext text of ours the server could replay as proof
// (ruling M4-R16).
func signedClean(t *testing.T, out map[PartKind][]byte) {
	t.Helper()
	for _, k := range []PartKind{SharedSigned, CalendarSigned} {
		if b, ok := out[k]; ok && HoldsEncryptedOnly(b) {
			t.Errorf("the %s holds an encrypted-only property:\n%q", k, b)
		}
	}
}

// A stored line holding an encrypted-only property in a signed part (from
// another client, or injected by the server) leaves it for its routed
// encrypted part, its bytes kept, so that we never sign it in clear
// (ruling M4-R16). A copy the destination already holds is not repeated.
func TestPatchMovesEncryptedOnlyOutOfSignedParts(t *testing.T) {
	t.Parallel()
	oddEvil := "X-EVIL;A=1:a value that Proton folded at forty octets, not s\r\n eventy-five"
	cur := map[PartKind][]byte{
		SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z", oddEvil, "a.SUMMARY:bait", "SEQUENCE:1"),
		SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:Title", "LOCATION:Room 1"),
		CalendarSigned:  frag(uidLine, stampLine, "STATUS:CONFIRMED", "COMMENT:a note", "LOCATION:Room 1"),
	}
	// ParseWrite refuses a group name, so a client never sends a.SUMMARY
	// back; Patch is still given it here, as a defence of its own.
	view := maps.Clone(cur)
	view[SharedSigned] = frag(uidLine, stampLine, "DTSTART:20261105T100000Z", oddEvil, "SEQUENCE:1")
	in := set(t, served(t, view), "a.SUMMARY", "a.SUMMARY:bait")
	got, changed := mustPatch(t, cur, in)
	want := map[PartKind][]byte{
		SharedSigned:      frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "SEQUENCE:1"),
		SharedEncrypted:   frag(uidLine, stampLine, "SUMMARY:Title", "LOCATION:Room 1", oddEvil, "a.SUMMARY:bait"),
		CalendarSigned:    frag(uidLine, stampLine, "STATUS:CONFIRMED"),
		CalendarEncrypted: frag(uidLine, stampLine, "COMMENT:a note"),
	}
	expectParts(t, got, changed, want, SharedSigned, SharedEncrypted, CalendarSigned, CalendarEncrypted)
	signedClean(t, got)

	// The client drops the injected lines: they go from everywhere.
	in = without(in, func(p Property) bool {
		return strings.EqualFold(p.Name, "X-EVIL") || strings.EqualFold(p.Name, "a.SUMMARY")
	})
	got, _ = mustPatch(t, cur, in)
	if !bytes.Equal(got[SharedEncrypted], cur[SharedEncrypted]) {
		t.Errorf("dropped lines linger in the shared encrypted part:\n%q", got[SharedEncrypted])
	}
	signedClean(t, got)
}

// An edited value of a property stored off-table in a signed part goes to
// its routed encrypted part only: the new SUMMARY never lands in clear
// (re-review, Task 3 Patch observation).
func TestPatchEditedValueNeverLandsInClear(t *testing.T) {
	t.Parallel()
	for name, cur := range map[string]map[PartKind][]byte{
		"only in the signed part": {
			SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "SUMMARY:bait"),
			SharedEncrypted: frag(uidLine, stampLine, "LOCATION:Room 1"),
			CalendarSigned:  frag(uidLine, stampLine, "STATUS:CONFIRMED"),
		},
		"in both": {
			SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "SUMMARY:bait"),
			SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:Title", "LOCATION:Room 1"),
			CalendarSigned:  frag(uidLine, stampLine, "STATUS:CONFIRMED"),
		},
		"in the calendar signed part": {
			SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z"),
			SharedEncrypted: frag(uidLine, stampLine, "LOCATION:Room 1"),
			CalendarSigned:  frag(uidLine, stampLine, "STATUS:CONFIRMED", "SUMMARY:bait"),
		},
	} {
		in := set(t, served(t, cur), "SUMMARY", "SUMMARY:secret title")
		got, _ := mustPatch(t, cur, in)
		for _, k := range []PartKind{SharedSigned, CalendarSigned} {
			if bytes.Contains(got[k], []byte("secret")) || bytes.Contains(got[k], []byte("SUMMARY")) {
				t.Errorf("%s: the %s holds a SUMMARY:\n%q", name, k, got[k])
			}
		}
		if n := bytes.Count(got[SharedEncrypted], []byte("\r\nSUMMARY:secret title\r\n")); n != 1 || bytes.Contains(got[SharedEncrypted], []byte("bait")) {
			t.Errorf("%s: the shared encrypted part holds %d edited SUMMARY lines:\n%q", name, n, got[SharedEncrypted])
		}
		signedClean(t, got)
	}
}
