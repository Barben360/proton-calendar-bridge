package protontest

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ProtonMail/go-proton-api/server/backend"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// The fake serves real card parts, built the way the M3 research (B.1) reads
// Proton's: each part is a complete VCALENDAR holding one VEVENT, its lines
// CRLF-separated with no CRLF after the last one [inferred]; cleartext parts
// carry a detached signature, encrypted parts a SEIPD packet under a session
// key whose PKESK is the row's key packet, and a detached signature over the
// plaintext. Every signature is made by the address key (AddressKeyRing), or
// by an unrelated key with Event.ForeignKey.

// Part names a part that Event.BrokenPart makes undecryptable, or that
// Event.Unsigned serves without a signature.
type Part int

// The parts Event.BrokenPart can break: the encrypted ones. Event.Unsigned
// takes any but PersonalPart, and SharedSignedPart (the cleartext one).
const (
	NoPart Part = iota
	SharedEncryptedPart
	CalendarEncryptedPart
	AttendeesPart
	PersonalPart
	SharedSignedPart
)

// Notification is an event reminder as the row serves it.
type Notification struct {
	Type    int
	Trigger string
}

// signer is the unlocked key of every address of every account: with
// fastKeys (see borrow) the backend locks one precomputed key for each user
// and address key, and FastGenerateKey hands that same key out locked with a
// passphrase of ours.
var signer = sync.OnceValues(func() (*crypto.KeyRing, error) {
	pass := []byte("protontest-signer")
	armored, err := backend.FastGenerateKey("", "", pass, "", 0)
	if err != nil {
		return nil, err
	}
	locked, err := crypto.NewKeyFromArmored(armored)
	if err != nil {
		return nil, err
	}
	key, err := locked.Unlock(pass)
	if err != nil {
		return nil, err
	}
	return crypto.NewKeyRing(key)
})

// stranger is an unrelated key, for Event.ForeignKey.
var stranger = sync.OnceValues(func() (*crypto.KeyRing, error) {
	key, err := crypto.GenerateKey("Stranger", "stranger@protontest.invalid", "x25519", 0)
	if err != nil {
		return nil, err
	}
	return crypto.NewKeyRing(key)
})

// AddressKeyRing returns a copy of the unlocked key every address of the
// fake's accounts has: the key that signs the parts and that an address key
// packet is encrypted to.
func (f *Fake) AddressKeyRing() *crypto.KeyRing {
	f.t.Helper()
	kr, err := signer()
	if err != nil {
		f.t.Fatalf("protontest: address key: %v", err)
	}
	c, err := kr.Copy()
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// CalendarKeyRing returns a copy of the calendar's unlocked key.
func (f *Fake) CalendarKeyRing(calID string) *crypto.KeyRing {
	f.t.Helper()
	f.cal.mu.Lock()
	rec := f.cal.byID[calID]
	f.cal.mu.Unlock()
	if rec == nil {
		f.t.Fatalf("protontest: unknown calendar %q", calID)
	}
	c, err := rec.ring.Copy()
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// cardParts are the content lines of each part, a folded property or a
// nested component kept as one entry, in the card's order.
type cardParts struct {
	common                             []string // UID and DTSTAMP: in every part
	sharedSigned, sharedEnc, calSigned []string
	calEnc, attendees, personal        []string
}

// splitCard routes e's card (or a minimal one from its columns) per the
// merge table: structural properties to the shared signed part,
// STATUS/TRANSP/EXDATE to the calendar signed part, COMMENT to the calendar
// encrypted part, ATTENDEE to the attendees part, VALARM to the personal
// part, everything else to the shared encrypted part.
func splitCard(e Event) cardParts {
	var p cardParts
	units := cardUnits(e.Card)
	if e.Card == "" {
		units = minimalCard(e)
	}
	hasUID := false
	for _, u := range units {
		switch name := propName(u); name {
		case "UID", "DTSTAMP":
			hasUID = hasUID || name == "UID"
			p.common = append(p.common, u)
		case "DTSTART", "DTEND", "DURATION", "RRULE", "RECURRENCE-ID", "SEQUENCE", "ORGANIZER", "X-PM-CONFERENCE-ID":
			p.sharedSigned = append(p.sharedSigned, u)
		case "STATUS", "TRANSP", "EXDATE":
			p.calSigned = append(p.calSigned, u)
		case "COMMENT":
			p.calEnc = append(p.calEnc, u)
		case "ATTENDEE":
			p.attendees = append(p.attendees, u)
		case "BEGIN":
			first, _, _ := strings.Cut(u, "\r\n")
			if strings.EqualFold(strings.TrimSpace(first[len("BEGIN:"):]), "VALARM") {
				p.personal = append(p.personal, u)
			} else {
				p.sharedEnc = append(p.sharedEnc, u)
			}
		default:
			p.sharedEnc = append(p.sharedEnc, u)
		}
	}
	if !hasUID {
		p.common = append([]string{"UID:" + e.UID}, p.common...)
	}
	return p
}

// cardUnits splits a card into its properties (continuation lines joined back
// with CRLF) and nested components (BEGIN to END, joined with CRLF). LF alone
// is read as CRLF; empty lines are dropped.
func cardUnits(card string) []string {
	var units []string
	depth := 0
	for _, l := range strings.Split(strings.ReplaceAll(card, "\r\n", "\n"), "\n") {
		if l == "" {
			continue
		}
		upper := strings.ToUpper(l)
		switch {
		case depth > 0:
			units[len(units)-1] += "\r\n" + l
			if strings.HasPrefix(upper, "BEGIN:") {
				depth++
			} else if strings.HasPrefix(upper, "END:") {
				depth--
			}
		case (l[0] == ' ' || l[0] == '\t') && len(units) > 0:
			units[len(units)-1] += "\r\n" + l
		default:
			units = append(units, l)
			if strings.HasPrefix(upper, "BEGIN:") {
				depth = 1
			}
		}
	}
	return units
}

// minimalCard is the card of an event added without one: its columns.
func minimalCard(e Event) []string {
	stamp := func(unix int64) string { return time.Unix(unix, 0).UTC().Format("20060102T150405Z") }
	out := []string{"UID:" + e.UID, "DTSTAMP:" + stamp(1700000000)}
	if e.FullDay {
		day := func(unix int64) string { return time.Unix(unix, 0).UTC().Format("20060102") }
		out = append(out, "DTSTART;VALUE=DATE:"+day(e.Start), "DTEND;VALUE=DATE:"+day(e.End))
	} else {
		out = append(out, "DTSTART:"+stamp(e.Start), "DTEND:"+stamp(e.End))
	}
	if e.RRule != "" {
		out = append(out, "RRULE:"+e.RRule)
	}
	if e.RecurrenceID != nil {
		out = append(out, "RECURRENCE-ID:"+stamp(*e.RecurrenceID))
	}
	return out
}

// propName is the upper-cased name of a property line.
func propName(line string) string {
	if i := strings.IndexAny(line, ";:"); i >= 0 {
		line = line[:i]
	}
	return strings.ToUpper(line)
}

// vcalendar wraps a part's lines.
func vcalendar(common, lines []string) string {
	all := []string{"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//pcb//protontest//EN", "BEGIN:VEVENT"}
	all = append(append(append(all, common...), lines...), "END:VEVENT", "END:VCALENDAR")
	return strings.Join(all, "\r\n")
}

// attendeeRows derives the row's Attendees column from the attendee lines:
// X-PM-TOKEN joins them, PARTSTAT gives the status.
func attendeeRows(rowID string, lines []string) []any {
	statuses := map[string]int{"NEEDS-ACTION": 0, "TENTATIVE": 1, "DECLINED": 2, "ACCEPTED": 3}
	out := []any{}
	for i, l := range lines {
		head := strings.NewReplacer("\r\n ", "", "\r\n\t", "").Replace(l)
		if j := strings.IndexByte(head, ':'); j >= 0 {
			head = head[:j]
		}
		token, status := "", 0
		for _, param := range strings.Split(head, ";")[1:] {
			k, v, _ := strings.Cut(param, "=")
			switch strings.ToUpper(k) {
			case "X-PM-TOKEN":
				token = v
			case "PARTSTAT":
				status = statuses[strings.ToUpper(v)]
			}
		}
		if token != "" {
			out = append(out, map[string]any{"ID": fmt.Sprintf("att%d-%s", i, rowID), "Token": token, "Status": status, "Permissions": 3})
		}
	}
	return out
}

// buildParts makes the row's crypto fields and the columns derived from its
// card. Author defaults to the email of the calendar's address (the owner's
// first address until the calendar is materialized). Caller holds f.cal.mu.
func (f *Fake) buildParts(cal *calendarRec, r *eventRec) (map[string]any, error) {
	addrKR, err := signer()
	if err != nil {
		return nil, err
	}
	signKR := addrKR
	if r.ev.ForeignKey {
		if signKR, err = stranger(); err != nil {
			return nil, err
		}
	}
	author := r.ev.Author
	if author == "" {
		author = cal.owner + "@" + f.backend.GetDomain()
		if cal.ready {
			author = cal.address.Email
		}
	}
	var errs []error
	sign := func(text string) string {
		sig, err := signKR.SignDetached(crypto.NewPlainMessage([]byte(text)))
		if err != nil {
			errs = append(errs, err)
			return ""
		}
		armored, err := sig.GetArmored()
		errs = append(errs, err)
		return armored
	}
	clearPart := func(text string) map[string]any {
		return map[string]any{"Type": 2, "Data": text, "Signature": sign(text), "Author": author}
	}
	// sealed encrypts text under sk; broken uses an unrelated session key.
	sealed := func(text string, sk *crypto.SessionKey, broken bool) map[string]any {
		if broken {
			sk, err = crypto.GenerateSessionKey()
			errs = append(errs, err)
		}
		data, err := sk.Encrypt(crypto.NewPlainMessage([]byte(text)))
		errs = append(errs, err)
		return map[string]any{"Type": 3, "Data": base64.StdEncoding.EncodeToString(data), "Signature": sign(text), "Author": author}
	}
	wrap := func(kr *crypto.KeyRing, sk *crypto.SessionKey) string {
		kp, err := kr.EncryptSessionKey(sk)
		errs = append(errs, err)
		return base64.StdEncoding.EncodeToString(kp)
	}

	p := splitCard(r.ev)
	brk := r.ev.BrokenPart
	sharedSK, err := crypto.GenerateSessionKey()
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"Author": author, "SharedKeyPacket": nil, "AddressKeyPacket": nil, "AddressID": nil, "CalendarKeyPacket": nil,
		"Attendees": attendeeRows(r.id, p.attendees), "AttendeesEvents": []any{}, "PersonalEvents": []any{},
	}
	if r.ev.AddressKeyPacket {
		out["AddressKeyPacket"] = wrap(addrKR, sharedSK)
		out["AddressID"] = cal.address.ID
	} else {
		out["SharedKeyPacket"] = wrap(cal.ring, sharedSK)
	}
	out["SharedEvents"] = []any{
		clearPart(vcalendar(p.common, p.sharedSigned)),
		sealed(vcalendar(p.common, p.sharedEnc), sharedSK, brk == SharedEncryptedPart),
	}
	calendarParts := []any{clearPart(vcalendar(p.common, p.calSigned))}
	if len(p.calEnc) > 0 {
		calSK, err := crypto.GenerateSessionKey()
		if err != nil {
			return nil, err
		}
		out["CalendarKeyPacket"] = wrap(cal.ring, calSK)
		calendarParts = append(calendarParts, sealed(vcalendar(p.common, p.calEnc), calSK, brk == CalendarEncryptedPart))
	}
	out["CalendarEvents"] = calendarParts
	if len(p.attendees) > 0 {
		out["AttendeesEvents"] = []any{sealed(vcalendar(p.common, p.attendees), sharedSK, brk == AttendeesPart)}
	}
	if len(p.personal) > 0 {
		text := vcalendar(p.common, p.personal)
		data, err := personalData(cal.ring, text, brk == PersonalPart)
		errs = append(errs, err)
		out["PersonalEvents"] = []any{map[string]any{"Type": 3, "Data": data, "Signature": sign(text), "Author": author, "MemberID": cal.memberID}}
	}
	if err := unsign(out, r.ev.Unsigned); err != nil {
		return nil, err
	}
	return out, errors.Join(errs...)
}

// unsign serves part of the row's crypto fields without a signature: the
// signed bit of its Type cleared and no Signature, as a server that dropped
// it would.
func unsign(out map[string]any, part Part) error {
	at := map[Part]struct {
		field string
		i     int
	}{SharedSignedPart: {"SharedEvents", 0}, SharedEncryptedPart: {"SharedEvents", 1},
		CalendarEncryptedPart: {"CalendarEvents", 1}, AttendeesPart: {"AttendeesEvents", 0}}
	if part == NoPart {
		return nil
	}
	loc, ok := at[part]
	if !ok {
		return fmt.Errorf("protontest: Unsigned cannot take part %d", part)
	}
	parts, _ := out[loc.field].([]any)
	if loc.i >= len(parts) {
		return fmt.Errorf("protontest: the row has no part %d to unsign", part)
	}
	m := parts[loc.i].(map[string]any)
	m["Type"] = m["Type"].(int) &^ 2
	delete(m, "Signature")
	return nil
}

// personalData is a personal part's Data: an armored message encrypted to the
// calendar key, as go-proton-api reads personal parts [inferred: no live
// sample]. broken leaves its key packet out, so nothing opens it.
func personalData(calKR *crypto.KeyRing, text string, broken bool) (string, error) {
	plain := crypto.NewPlainMessage([]byte(text))
	if !broken {
		msg, err := calKR.Encrypt(plain, nil)
		if err != nil {
			return "", err
		}
		return msg.GetArmored()
	}
	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		return "", err
	}
	data, err := sk.Encrypt(plain)
	if err != nil {
		return "", err
	}
	return crypto.NewPGPMessage(data).GetArmored()
}
