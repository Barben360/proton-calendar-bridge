package protontest

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // TZIDs resolve on any host, as in the bridge's embedded database

	"github.com/ProtonMail/go-proton-api/server/backend"
	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// The fake serves real card parts, built the way the M3 research (B.1) reads
// Proton's: each part is a complete VCALENDAR holding one VEVENT, its lines
// CRLF-separated with no CRLF after the last one [inferred]; cleartext parts
// carry a detached signature, encrypted parts a SEIPD packet under a session
// key whose PKESK is the row's key packet, and a detached signature over the
// plaintext. Every signature is made by the key of the calendar's address
// (AddressKeyRing, or OtherAddressKeyRing for a ToSecondAddress calendar), or
// by an unrelated key with Event.ForeignKey.

// Part names a part that Event.BrokenPart makes undecryptable, or that
// Event.Unsigned serves without a signature.
type Part int

// The parts Event.BrokenPart can break: the encrypted ones (a row has a
// PersonalPart with Event.PersonalAlarms only). Event.Unsigned takes any but
// PersonalPart, and SharedSignedPart (the cleartext one).
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

// signer is the unlocked key of every account's first address (and of every
// user key): with fastKeys (see borrow) the backend locks one precomputed key
// for each, and FastGenerateKey hands that same key out locked with a
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
	return crypto.NewKeyRing(warm(key))
})

// warm serializes a key shared across goroutines once, before it is shared:
// go-crypto caches a signature's subpackets on its first serialization (a
// write), so concurrent first copies of a shared key race.
func warm(key *crypto.Key) *crypto.Key {
	_, _ = key.Serialize()
	return key
}

// otherAddressKey is the key of every address but an account's first one
// (see generateKey), unlocked: a part it signs is signed by the wrong
// address when the calendar belongs to the first address.
var otherAddressKey = sync.OnceValues(func() (*crypto.Key, error) {
	key, err := crypto.GenerateKey("Other address", "other@protontest.invalid", "x25519", 0)
	if err != nil {
		return nil, err
	}
	return warm(key), nil
})

// otherSigner is otherAddressKey in a keyring.
func otherSigner() (*crypto.KeyRing, error) {
	k, err := otherAddressKey()
	if err != nil {
		return nil, err
	}
	return crypto.NewKeyRing(k)
}

// stranger is an unrelated key, for Event.ForeignKey.
var stranger = sync.OnceValues(func() (*crypto.KeyRing, error) {
	key, err := crypto.GenerateKey("Stranger", "stranger@protontest.invalid", "x25519", 0)
	if err != nil {
		return nil, err
	}
	return crypto.NewKeyRing(key)
})

// AddressKeyRing returns a copy of the unlocked key of every account's first
// address: the key that signs the parts of its calendars and that an address
// key packet is encrypted to.
func (f *Fake) AddressKeyRing() *crypto.KeyRing {
	f.t.Helper()
	return f.keyCopy(signer)
}

// OtherAddressKeyRing returns a copy of the unlocked key of every other
// address (each account's second one, and AddAddress's): the key of a
// ToSecondAddress calendar.
func (f *Fake) OtherAddressKeyRing() *crypto.KeyRing {
	f.t.Helper()
	return f.keyCopy(otherSigner)
}

func (f *Fake) keyCopy(ring func() (*crypto.KeyRing, error)) *crypto.KeyRing {
	f.t.Helper()
	kr, err := ring()
	if err != nil {
		f.t.Fatalf("protontest: address key: %v", err)
	}
	c, err := kr.Copy()
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// memberKey is the key of the calendar's address: the account's second
// address with ToSecondAddress, else its first.
func memberKey(cal *calendarRec) (*crypto.KeyRing, error) {
	if cal.def.ToSecondAddress {
		return otherSigner()
	}
	return signer()
}

// CalendarKeyRing returns a copy of the calendar's primary key, unlocked:
// the key new rows' key packets go to (bootstrap Flags 3).
func (f *Fake) CalendarKeyRing(calID string) *crypto.KeyRing {
	f.t.Helper()
	return f.calendarKeyCopy(calID, (*calendarRec).primaryKey)
}

// RetiredCalendarKeyRing returns a copy of the calendar's retired key,
// unlocked: listed with Flags 1, it opens but takes no new key packet.
func (f *Fake) RetiredCalendarKeyRing(calID string) *crypto.KeyRing {
	f.t.Helper()
	return f.calendarKeyCopy(calID, func(c *calendarRec) *crypto.Key { return c.retired })
}

func (f *Fake) calendarKeyCopy(calID string, pick func(*calendarRec) *crypto.Key) *crypto.KeyRing {
	f.t.Helper()
	f.cal.mu.Lock()
	defer f.cal.mu.Unlock()
	rec := f.cal.byID[calID]
	if rec == nil {
		f.t.Fatalf("protontest: unknown calendar %q", calID)
	}
	key, err := pick(rec).Copy()
	if err != nil {
		f.t.Fatal(err)
	}
	kr, err := crypto.NewKeyRing(key)
	if err != nil {
		f.t.Fatal(err)
	}
	return kr
}

// cardParts are the content lines of each part, a folded property or a
// nested component kept as one entry, in the card's order.
type cardParts struct {
	common                             []string // UID and DTSTAMP: in every part
	sharedSigned, sharedEnc, calSigned []string
	calEnc, attendees, personal        []string
}

// splitCard routes e's card (or a minimal one from its columns) per the
// write table (research B.1): structural properties to the shared signed
// part, EXDATE to both signed parts, STATUS and TRANSP to the calendar
// signed part, COMMENT to the calendar encrypted part, ATTENDEE to the
// attendees part, everything else to the shared encrypted part. DURATION is
// served as DTEND. A VALARM goes to no part (alarms are the row's
// Notifications, research D), or to the personal part with
// Event.PersonalAlarms.
func splitCard(e Event) (cardParts, error) {
	var p cardParts
	units := cardUnits(e.Card)
	if e.Card == "" {
		units = minimalCard(e)
	}
	units, err := durationToEnd(units)
	if err != nil {
		return cardParts{}, err
	}
	hasUID := false
	for _, u := range units {
		switch name := propName(u); name {
		case "UID", "DTSTAMP":
			hasUID = hasUID || name == "UID"
			p.common = append(p.common, u)
		case "DTSTART", "DTEND", "RRULE", "RECURRENCE-ID", "SEQUENCE", "ORGANIZER", "X-PM-CONFERENCE-ID":
			p.sharedSigned = append(p.sharedSigned, u)
		case "EXDATE":
			p.sharedSigned = append(p.sharedSigned, u)
			p.calSigned = append(p.calSigned, u)
		case "STATUS", "TRANSP":
			p.calSigned = append(p.calSigned, u)
		case "COMMENT":
			p.calEnc = append(p.calEnc, u)
		case "ATTENDEE":
			p.attendees = append(p.attendees, u)
		case "BEGIN":
			first, _, _ := strings.Cut(u, "\r\n")
			switch {
			case !strings.EqualFold(strings.TrimSpace(first[len("BEGIN:"):]), "VALARM"):
				p.sharedEnc = append(p.sharedEnc, u)
			case e.PersonalAlarms:
				p.personal = append(p.personal, u)
			}
		default:
			p.sharedEnc = append(p.sharedEnc, u)
		}
	}
	if !hasUID {
		p.common = append([]string{"UID:" + e.UID}, p.common...)
	}
	return p, nil
}

// durationToEnd replaces a DURATION with the DTEND it means, in DTSTART's
// form: a DATE, a UTC time or a local time of the same TZID (days and weeks
// nominal, the rest exact, as RFC 5545 counts them).
func durationToEnd(units []string) ([]string, error) {
	start, dur := -1, -1
	for i, u := range units {
		switch propName(u) {
		case "DTSTART":
			start = i
		case "DURATION":
			dur = i
		case "DTEND":
			return units, nil
		}
	}
	if dur < 0 {
		return units, nil
	}
	if start < 0 {
		return nil, errors.New("protontest: a DURATION without DTSTART")
	}
	p, err := parseProperty(units[start])
	if err != nil {
		return nil, err
	}
	t, date, _, err := instant(p, p.value)
	if err != nil {
		return nil, err
	}
	d, err := parseProperty(units[dur])
	if err != nil {
		return nil, err
	}
	days, exact, err := parseDuration(d.value)
	if err != nil {
		return nil, err
	}
	end := t.AddDate(0, 0, days).Add(exact)
	out := slices.Clone(units)
	switch tzid, local := p.params["TZID"]; {
	case date:
		out[dur] = "DTEND;VALUE=DATE:" + end.Format(dateLayout)
	case local:
		out[dur] = "DTEND;TZID=" + tzid + ":" + end.Format(localLayout)
	default:
		out[dur] = "DTEND:" + end.UTC().Format(utcLayout)
	}
	return out, nil
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

// The iCalendar date and date-time forms.
const (
	dateLayout  = "20060102"
	localLayout = "20060102T150405"
	utcLayout   = "20060102T150405Z"
)

// property is a content line, unfolded: its upper-cased name, its parameters
// (names upper-cased, values unquoted) and its value.
type property struct {
	name   string
	params map[string]string
	value  string
}

// parseProperty reads one property (a unit of cardUnits). A ':' or ';' in a
// quoted parameter value does not end the name or the parameter.
func parseProperty(unit string) (property, error) {
	line := strings.NewReplacer("\r\n ", "", "\r\n\t", "").Replace(unit)
	var fields []string
	quoted, from := false, 0
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '"':
			quoted = !quoted
		case c == ';' && !quoted:
			fields, from = append(fields, line[from:i]), i+1
		case c == ':' && !quoted:
			fields = append(fields, line[from:i])
			p := property{name: strings.ToUpper(fields[0]), params: map[string]string{}, value: line[i+1:]}
			for _, f := range fields[1:] {
				k, v, ok := strings.Cut(f, "=")
				if !ok {
					return property{}, fmt.Errorf("a parameter without value in %s", p.name)
				}
				p.params[strings.ToUpper(k)] = strings.Trim(v, `"`)
			}
			if p.name == "" {
				return property{}, errors.New("a property without name")
			}
			return p, nil
		}
	}
	return property{}, errors.New("a content line without ':'")
}

// instant reads one value of a DTSTART-like property p: the instant, whether
// it is a DATE (midnight UTC, as Proton stores full-day times) and its zone
// name ("UTC" for a UTC time or a DATE). A floating time and a TZID the tz
// database lacks are errors, as Proton refuses them (research I).
func instant(p property, value string) (t time.Time, date bool, zone string, err error) {
	tzid, local := p.params["TZID"]
	switch {
	case p.params["VALUE"] == "DATE":
		t, err = time.Parse(dateLayout, value)
		return t, true, "UTC", err
	case local:
		if tzid == "" || tzid == "Local" {
			return time.Time{}, false, "", fmt.Errorf("TZID %q", tzid)
		}
		loc, lerr := time.LoadLocation(tzid)
		if lerr != nil {
			return time.Time{}, false, "", fmt.Errorf("TZID %q is not in the tz database", tzid)
		}
		t, err = time.ParseInLocation(localLayout, value, loc)
		return t, false, tzid, err
	case strings.HasSuffix(value, "Z"):
		t, err = time.Parse(utcLayout, value)
		return t, false, "UTC", err
	}
	return time.Time{}, false, "", fmt.Errorf("a floating %s", p.name)
}

var durationRE = regexp.MustCompile(`^([+-]?)P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// parseDuration reads an iCalendar duration: its nominal days (weeks
// included) and its exact part.
func parseDuration(s string) (days int, exact time.Duration, err error) {
	m := durationRE.FindStringSubmatch(s)
	if m == nil || s == "P" || strings.HasSuffix(s, "T") {
		return 0, 0, fmt.Errorf("duration %q", s)
	}
	n := func(i int) int { v, _ := strconv.Atoi(m[i]); return v }
	days = 7*n(2) + n(3)
	exact = time.Duration(n(4))*time.Hour + time.Duration(n(5))*time.Minute + time.Duration(n(6))*time.Second
	if m[1] == "-" {
		days, exact = -days, -exact
	}
	return days, exact, nil
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

// attendeeID is the ID of the row's attendee with token: stable across the
// row's edits.
func attendeeID(rowID, token string) string {
	h := sha256.Sum256([]byte(rowID + "\x00" + token))
	return "att" + hex.EncodeToString(h[:8])
}

// attendeeRows derives the row's Attendees column from the attendee lines:
// X-PM-TOKEN joins them, PARTSTAT gives the status.
func attendeeRows(rowID string, lines []string) []any {
	statuses := map[string]int{"NEEDS-ACTION": 0, "TENTATIVE": 1, "DECLINED": 2, "ACCEPTED": 3}
	out := []any{}
	for _, l := range lines {
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
			out = append(out, map[string]any{"ID": attendeeID(rowID, token), "Token": token, "Status": status, "Permissions": 3})
		}
	}
	return out
}

// author is the email the calendar's parts are signed as: the calendar
// address's, the owner's first address until the calendar is materialized.
// Caller holds f.cal.mu.
func (f *Fake) author(cal *calendarRec) string {
	switch {
	case cal.ready:
		return cal.address.Email
	case cal.def.ToSecondAddress:
		return secondAddress(cal.owner, f.backend.GetDomain())
	}
	return cal.owner + "@" + f.backend.GetDomain()
}

// buildParts makes the row's crypto fields and the columns derived from its
// card. Author defaults to the calendar's (see author). Caller holds
// f.cal.mu.
func (f *Fake) buildParts(cal *calendarRec, r *eventRec) (map[string]any, error) {
	addrKR, err := memberKey(cal)
	if err != nil {
		return nil, err
	}
	calKR, err := cal.primaryRing()
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
		author = f.author(cal)
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

	p, err := splitCard(r.ev)
	if err != nil {
		return nil, err
	}
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
		out["SharedKeyPacket"] = wrap(calKR, sharedSK)
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
		out["CalendarKeyPacket"] = wrap(calKR, calSK)
		calendarParts = append(calendarParts, sealed(vcalendar(p.common, p.calEnc), calSK, brk == CalendarEncryptedPart))
	}
	out["CalendarEvents"] = calendarParts
	if len(p.attendees) > 0 {
		out["AttendeesEvents"] = []any{sealed(vcalendar(p.common, p.attendees), sharedSK, brk == AttendeesPart)}
	}
	if len(p.personal) > 0 {
		text := vcalendar(p.common, p.personal)
		data, err := personalData(calKR, text, brk == PersonalPart)
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
