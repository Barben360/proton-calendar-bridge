package ical

import (
	"bytes"
	"errors"
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

// refusedMisrouted fails unless err is Patch's refusal of a stored
// misrouted line.
func refusedMisrouted(t *testing.T, what string, err error) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Code != MisroutedStoredLine {
		t.Errorf("%s: Patch error %v, want a %s refusal", what, err, MisroutedStoredLine)
	}
}

// A stored signed part holding an encrypted-only property (from another
// client, or injected by the server) is refused, never moved nor kept
// (ruling M4-R19): kept, we would sign it in clear; moved, it would seed an
// encrypted part whose plaintext the server knows. The refusal quotes
// nothing.
func TestPatchRefusesMisroutedStoredLines(t *testing.T) {
	t.Parallel()
	base := map[PartKind][]byte{
		SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z"),
		SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:Title"),
		CalendarSigned:  frag(uidLine, stampLine, "STATUS:CONFIRMED"),
	}
	in := served(t, base)
	for _, c := range []struct {
		kind PartKind
		line string
	}{
		{SharedSigned, "X-EVIL;A=1:zq9 value"},
		{SharedSigned, "a.SUMMARY:zq9bait"},
		{SharedSigned, "SUMMARY:zq9bait"},
		{SharedSigned, "ATTENDEE:mailto:zq9@evil.test"},
		{CalendarSigned, "COMMENT:zq9note"},
		{CalendarSigned, "location:zq9room"},
	} {
		cur := maps.Clone(base)
		cur[c.kind] = frag(uidLine, stampLine, c.line)
		out, _, err := Patch(cur, in)
		refusedMisrouted(t, c.line, err)
		if out != nil {
			t.Errorf("%s: Patch returned parts with its refusal", c.line)
		}
		if err != nil && strings.Contains(err.Error(), "zq9") {
			t.Errorf("%s: the refusal quotes the line: %v", c.line, err)
		}
	}
	// Signed-routed lines, nested components and COLOR in a signed part are
	// not misrouted: patched as before.
	cur := maps.Clone(base)
	cur[SharedSigned] = frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "X-PM-CONFERENCE-ID:c1", "COLOR:red",
		"BEGIN:VALARM", "ACTION:DISPLAY", "DESCRIPTION:x", "TRIGGER:-PT5M", "END:VALARM")
	if _, _, err := Patch(cur, served(t, cur)); err != nil {
		t.Errorf("a signed part with signed-routed lines only: %v", err)
	}
}

// An edited SUMMARY never lands in clear: with a SUMMARY stored in a signed
// part, Patch refuses; stored where it belongs, the edit goes to the shared
// encrypted part only.
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
		_, _, err := Patch(cur, set(t, served(t, cur), "SUMMARY", "SUMMARY:secret title"))
		refusedMisrouted(t, name, err)
	}
	for name, cur := range map[string]map[PartKind][]byte{
		"none stored": {
			SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z"),
			SharedEncrypted: frag(uidLine, stampLine, "LOCATION:Room 1"),
			CalendarSigned:  frag(uidLine, stampLine, "STATUS:CONFIRMED"),
		},
		"in the shared encrypted part": {
			SharedSigned:    frag(uidLine, stampLine, "DTSTART:20261105T100000Z"),
			SharedEncrypted: frag(uidLine, stampLine, "SUMMARY:Title", "LOCATION:Room 1"),
			CalendarSigned:  frag(uidLine, stampLine, "STATUS:CONFIRMED"),
		},
	} {
		got, _ := mustPatch(t, cur, set(t, served(t, cur), "SUMMARY", "SUMMARY:secret title"))
		for k, b := range got {
			if k != SharedEncrypted && bytes.Contains(b, []byte("secret")) {
				t.Errorf("%s: the %s holds the edited SUMMARY:\n%q", name, k, b)
			}
		}
		if n := bytes.Count(got[SharedEncrypted], []byte("\r\nSUMMARY:secret title\r\n")); n != 1 {
			t.Errorf("%s: the shared encrypted part holds %d edited SUMMARY lines:\n%q", name, n, got[SharedEncrypted])
		}
		signedClean(t, got)
	}
}

// ProvesKey is the proof SessionKeys asks of a key's encrypted part (ruling
// M4-R19): Proton's fragment form (a VCALENDAR holding exactly one VEVENT
// and nothing else), the row's UID, and an encrypted-only property of the
// VEVENT's own.
func TestProvesKey(t *testing.T) {
	t.Parallel()
	const uid = "u1"
	reply := "BEGIN:VCALENDAR\r\nPRODID:-//Proton AG//WebCalendar//EN\r\nVERSION:2.0\r\nMETHOD:REPLY\r\n" +
		"BEGIN:VEVENT\r\nUID:u1\r\nDTSTAMP:20261101T080000Z\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:a@b\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, c := range []struct {
		name string
		text []byte
		want bool
	}{
		{"a fragment with a SUMMARY", frag(uidLine, stampLine, "SUMMARY:x"), true},
		{"another UID", frag("UID:u2", stampLine, "SUMMARY:x"), false},
		{"a UID differing in case", frag("UID:U1", stampLine, "SUMMARY:x"), false},
		{"no UID", frag(stampLine, "SUMMARY:x"), false},
		{"two UIDs", frag(uidLine, "UID:u2", stampLine, "SUMMARY:x"), false},
		{"UID and DTSTAMP only", frag(uidLine, stampLine), false},
		{"signed-routed lines only", frag(uidLine, stampLine, "DTSTART:20261105T100000Z", "STATUS:CONFIRMED"), false},
		{"a METHOD:REPLY .ics", []byte(reply), false},
		{"VERSION at VCALENDAR level", []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:u1\r\nSUMMARY:x\r\nEND:VEVENT\r\nEND:VCALENDAR"), false},
		{"a VTIMEZONE beside", []byte("BEGIN:VCALENDAR\r\nBEGIN:VTIMEZONE\r\nTZID:X\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:u1\r\nSUMMARY:x\r\nEND:VEVENT\r\nEND:VCALENDAR"), false},
		{"two VEVENTs", []byte("BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nUID:u1\r\nSUMMARY:x\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:u1\r\nEND:VEVENT\r\nEND:VCALENDAR"), false},
		{"two VCALENDARs", append(frag(uidLine, "SUMMARY:x"), []byte("\r\n"+string(frag(uidLine)))...), false},
		{"a bare VEVENT", []byte("BEGIN:VEVENT\r\nUID:u1\r\nSUMMARY:x\r\nEND:VEVENT"), false},
		{"a SUMMARY in a nested VALARM only", frag(uidLine, stampLine, "BEGIN:VALARM", "SUMMARY:x", "END:VALARM"), false},
		{"not iCalendar", []byte("passphrase-bytes"), false},
	} {
		if got := ProvesKey(c.text, uid, SharedEncrypted); got != c.want {
			t.Errorf("%s: ProvesKey = %v, want %v", c.name, got, c.want)
		}
	}
	if ProvesKey(frag(uidLine, "SUMMARY:x"), "", SharedEncrypted) {
		t.Error("an empty row UID is proven")
	}
	// The property must be one the routing table puts in that very part,
	// and only the shared and calendar encrypted parts prove.
	for _, c := range []struct {
		line string
		k    PartKind
		want bool
	}{
		{"SUMMARY:x", SharedEncrypted, true},
		{"X-FOO:x", SharedEncrypted, true},
		{"COMMENT:x", CalendarEncrypted, true},
		{"COMMENT:x", SharedEncrypted, false},
		{"ATTENDEE:mailto:a@b", SharedEncrypted, false},
		{"SUMMARY:x", CalendarEncrypted, false},
		{"ATTENDEE:mailto:a@b", Attendees, false},
		{"SUMMARY:x", Attendees, false},
		{"SUMMARY:x", SharedSigned, false},
	} {
		if got := ProvesKey(frag(uidLine, stampLine, c.line), uid, c.k); got != c.want {
			t.Errorf("%s in the %s: ProvesKey = %v, want %v", c.line, c.k, got, c.want)
		}
	}
}
