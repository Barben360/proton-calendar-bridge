// Package cards opens a cached Proton event row: it decrypts the row's card
// parts (each a VCALENDAR fragment) and checks their signatures, without
// merging them (that is the iCalendar codec's job). Decrypted text lives in
// memory only and appears in no error: an error names the part kind and the
// event ID, never ciphertext, plaintext or key material.
//
// Keys (M3 research B.1): the shared session key is wrapped to the calendar
// key (SharedKeyPacket) or, on a Proton-to-Proton invitation, to one of our
// address keys (AddressKeyPacket); the calendar session key to the calendar
// key (CalendarKeyPacket). The shared encrypted and attendees parts use the
// shared session key, the calendar encrypted part the calendar one; a
// personal part is an armored message to the calendar key [inferred].
// Plaintext is taken with GetBinary only: GetString folds CRLF and rewrites
// invalid UTF-8.
package cards

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// Kind is a part's slot in the row. Parts are returned in Kind order, which
// is the merge order.
type Kind int

// The part kinds. A cleartext part of a group takes the group's signed kind
// even when unsigned (Part.Signed tells).
const (
	SharedSigned Kind = iota
	SharedEncrypted
	CalendarSigned
	CalendarEncrypted
	Attendees
	Personal
)

func (k Kind) String() string {
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
	case Personal:
		return "personal part"
	}
	return fmt.Sprintf("part kind %d", int(k))
}

// Part is one opened card part.
type Part struct {
	Kind Kind
	Text []byte // the VCALENDAR fragment, byte for byte as signed
	// Signed: the part carries a signature. Verified: it verifies with the
	// key of Author, one of our addresses. Own: Author is one of our
	// addresses (own knows it), signed or not (ruling M3-R36).
	Signed, Verified, Own bool
	Author                string // the part's, else the row's
}

// AttendeeStatus is a row's attendee status, joined to an attendee line by
// Token (its X-PM-TOKEN): 0 needs-action, 1 tentative, 2 declined, 3 accepted.
type AttendeeStatus struct {
	ID, Token string
	Status    int
}

// Event is an opened row: its parts and the plaintext columns the rendering
// needs.
type Event struct {
	Parts []Part
	// Notifications is the row's value verbatim: null (or nil when absent)
	// means the calendar's defaults, [] none.
	Notifications json.RawMessage
	Color         string
	Attendees     []AttendeeStatus
	RecurrenceID  *int64
	// SkippedParts counts the personal parts left out because they do not
	// decrypt (ruling M3-R7): alarms come from Notifications, and another
	// member's personal part may be present.
	SkippedParts int
	// shared and calendar are the session keys the row's parts opened with,
	// for SessionKeys.
	shared, calendar keyUse
}

// keyUse is a row's session key and what proves it ours (SessionKeys).
type keyUse struct {
	key *crypto.SessionKey // nil until an encrypted part opens with it
	// packet: the row carries a key packet for it.
	packet bool
	// texts are the plaintexts of the encrypted parts opened with it,
	// verified how many of them are Verified (which implies Own).
	texts    [][]byte
	verified int
}

// ErrUndecryptable means a part of the row does not decrypt with the keys
// given.
var ErrUndecryptable = errors.New("cannot be decrypted")

// Card types are bit flags.
const (
	typeEncrypted = 1
	typeSigned    = 2
)

type wirePart struct {
	Type                    int
	Data, Signature, Author string
}

type wireRow struct {
	ID, Author                                           string
	SharedKeyPacket, CalendarKeyPacket, AddressKeyPacket string
	AddressID                                            string
	SharedEvents, CalendarEvents                         []wirePart
	AttendeesEvents, PersonalEvents                      []wirePart
	Notifications                                        json.RawMessage
	Color                                                *string
	Attendees                                            []AttendeeStatus
	RecurrenceID                                         *int64
}

// Open decrypts and verifies the parts of raw, a row as Proton serves it. cal
// is the calendar's keyring, addrs our address keyrings by address ID (needed
// only for an AddressKeyPacket), own returns our keyring for an email that is
// one of our addresses and nil otherwise (nil own: nothing is verified). A
// signature that does not verify is not an error (Verified false); a part
// that does not decrypt is (ErrUndecryptable, naming the part kind), except a
// personal part, which is skipped and counted (Event.SkippedParts).
func Open(raw json.RawMessage, cal *crypto.KeyRing, addrs map[string]*crypto.KeyRing, own func(email string) *crypto.KeyRing) (Event, error) {
	var w wireRow
	if err := json.Unmarshal(raw, &w); err != nil {
		// The decoder's text may quote the input: dropped.
		return Event{}, errors.New("decoding an event row: not a JSON object of the expected shape")
	}
	if w.ID == "" {
		return Event{}, errors.New("decoding an event row: no event ID")
	}
	o := opener{row: &w, cal: cal, addrs: addrs}
	ev := Event{Notifications: w.Notifications, Attendees: w.Attendees, RecurrenceID: w.RecurrenceID}
	if w.Color != nil {
		ev.Color = *w.Color
	}
	ev.shared.packet = w.SharedKeyPacket != "" || w.AddressKeyPacket != ""
	ev.calendar.packet = w.CalendarKeyPacket != ""
	groups := []struct {
		parts      []wirePart
		clear, enc Kind
		key        func(Kind) (*crypto.SessionKey, error)
		use        *keyUse
	}{
		{w.SharedEvents, SharedSigned, SharedEncrypted, o.sharedKey, &ev.shared},
		{w.CalendarEvents, CalendarSigned, CalendarEncrypted, o.calendarKey, &ev.calendar},
		{w.AttendeesEvents, Attendees, Attendees, o.sharedKey, &ev.shared},
		{w.PersonalEvents, Personal, Personal, nil, nil},
	}
	for _, g := range groups {
		for _, wp := range g.parts {
			var use *keyUse // set for a part opened with a session key
			p := Part{Kind: g.clear, Author: wp.Author, Signed: wp.Type&typeSigned != 0}
			if p.Author == "" {
				p.Author = w.Author
			}
			switch {
			case wp.Type&typeEncrypted == 0:
				p.Text = []byte(wp.Data)
			case g.key == nil: // personal: skipped, never fatal (M3-R7)
				p.Kind = g.enc
				text, err := o.armored(wp.Data)
				if err != nil {
					ev.SkippedParts++
					continue
				}
				p.Text = text
			default:
				p.Kind = g.enc
				sk, err := g.key(p.Kind)
				if err != nil {
					return Event{}, err
				}
				text, err := decryptData(sk, wp.Data)
				if err != nil {
					return Event{}, o.fail(p.Kind, err)
				}
				p.Text = text
				use = g.use
				use.key = sk
			}
			var kr *crypto.KeyRing
			if own != nil {
				kr = own(p.Author)
			}
			p.Own = kr != nil
			if p.Signed && p.Own {
				p.Verified = verify(p.Text, wp.Signature, kr)
			}
			if use != nil {
				use.texts = append(use.texts, p.Text)
				if p.Verified { // verified only with an own key: Own too
					use.verified++
				}
			}
			ev.Parts = append(ev.Parts, p)
		}
	}
	slices.SortStableFunc(ev.Parts, func(a, b Part) int { return int(a.Kind) - int(b.Kind) })
	return ev, nil
}

// opener holds one row's keys and its session keys, opened once.
type opener struct {
	row           *wireRow
	cal           *crypto.KeyRing
	addrs         map[string]*crypto.KeyRing
	shared, calSK *crypto.SessionKey
}

// fail is the error for a part of kind that does not open; reason is ours,
// never the library's text.
func (o *opener) fail(kind Kind, reason error) error {
	return fmt.Errorf("event %q: %s: %w: %w", o.row.ID, kind, reason, ErrUndecryptable)
}

var (
	errNoKeyPacket  = errors.New("no key packet")
	errKeyPacket    = errors.New("its key packet does not open")
	errData         = errors.New("its data does not decrypt")
	errNoCalendarKR = errors.New("no calendar key")
)

// sharedKey opens the shared session key: from the address key packet, with
// the row's address keyring first and then any other, and failing that from
// the shared key packet with the calendar keyring.
func (o *opener) sharedKey(kind Kind) (*crypto.SessionKey, error) {
	if o.shared != nil {
		return o.shared, nil
	}
	err := errNoKeyPacket
	if o.row.AddressKeyPacket != "" {
		// The row's address first only saves attempts: the result is the
		// same in any order, so no test can tell (and under the fake's
		// fastKeys every address has the same key anyway).
		var rings []*crypto.KeyRing
		if kr := o.addrs[o.row.AddressID]; kr != nil {
			rings = append(rings, kr)
		}
		for id, kr := range o.addrs {
			if id != o.row.AddressID && kr != nil {
				rings = append(rings, kr)
			}
		}
		if o.shared, err = openKeyPacket(o.row.AddressKeyPacket, rings); err == nil {
			return o.shared, nil
		}
	}
	if o.row.SharedKeyPacket != "" {
		if o.cal == nil {
			return nil, o.fail(kind, errNoCalendarKR)
		}
		if o.shared, err = openKeyPacket(o.row.SharedKeyPacket, []*crypto.KeyRing{o.cal}); err == nil {
			return o.shared, nil
		}
	}
	return nil, o.fail(kind, err)
}

// calendarKey opens the calendar session key with the calendar keyring.
func (o *opener) calendarKey(kind Kind) (*crypto.SessionKey, error) {
	if o.calSK != nil {
		return o.calSK, nil
	}
	if o.cal == nil {
		return nil, o.fail(kind, errNoCalendarKR)
	}
	sk, err := openKeyPacket(o.row.CalendarKeyPacket, []*crypto.KeyRing{o.cal})
	if err != nil {
		return nil, o.fail(kind, err)
	}
	o.calSK = sk
	return sk, nil
}

func openKeyPacket(b64 string, rings []*crypto.KeyRing) (*crypto.SessionKey, error) {
	if b64 == "" {
		return nil, errNoKeyPacket
	}
	if len(rings) == 0 {
		return nil, errKeyPacket
	}
	kp, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errKeyPacket
	}
	for _, kr := range rings {
		if sk, err := kr.DecryptSessionKey(kp); err == nil {
			return sk, nil
		}
	}
	return nil, errKeyPacket
}

func decryptData(sk *crypto.SessionKey, b64 string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errData
	}
	plain, err := sk.Decrypt(data)
	if err != nil {
		return nil, errData
	}
	return plain.GetBinary(), nil
}

// armored decrypts a personal part's armored message with the calendar
// keyring.
func (o *opener) armored(data string) ([]byte, error) {
	if o.cal == nil {
		return nil, errNoCalendarKR
	}
	msg, err := crypto.NewPGPMessageFromArmored(data)
	if err != nil {
		return nil, errData
	}
	plain, err := o.cal.Decrypt(msg, nil, 0)
	if err != nil {
		return nil, errData
	}
	return plain.GetBinary(), nil
}
