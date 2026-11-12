package cards

// Sealing is Open's inverse (research B.2, B.3): it turns per-part
// iCalendar fragments into the parts the sync endpoint stores. A signed part
// is the fragment itself with a detached binary-mode signature over its exact
// bytes; an encrypted part is a SEIPD packet of the fragment under a session
// key, base64, with a detached signature over the plaintext. The shared
// session key covers the shared encrypted and attendees parts, the calendar
// one the calendar encrypted part. Every part is signed by the primary key of
// our member's address. A create mints the session keys and wraps each to
// the primary calendar key only; an update reuses the row's keys and sends no
// key packet (research B.5). Errors name the part kind, never its text or a
// key.

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/events"
)

// ErrUntrustedSessionKey means a row's session key is not proven ours: a
// part encrypted under it is not signed by one of our addresses, or none is.
// Reusing it could encrypt an update to a key the server chose (research G).
var ErrUntrustedSessionKey = errors.New("session key not proven ours")

// SessionKeys returns the row's session keys for an update to reuse (see
// Seal). A key is returned only when the row's encrypted parts under it
// (shared: the shared encrypted and attendees parts; calendar: the calendar
// encrypted part) are all Own and Verified, and there is at least one;
// otherwise it is nil and err wraps ErrUntrustedSessionKey, naming the key.
// Each key is judged alone, so a trusted one is returned beside an error
// about the other. A row with no key packet for a key gives nil and no error
// for it: there is nothing to reuse, and Seal mints one when a part needs it.
func (e Event) SessionKeys() (shared, calendar *crypto.SessionKey, err error) {
	shared, errShared := e.shared.trustedKey("shared")
	calendar, errCal := e.calendar.trustedKey("calendar")
	return shared, calendar, errors.Join(errShared, errCal)
}

func (u keyUse) trustedKey(name string) (*crypto.SessionKey, error) {
	switch {
	case !u.packet:
		return nil, nil
	case u.key == nil || u.parts == 0 || u.trusted != u.parts:
		return nil, fmt.Errorf("the %s session key: %w", name, ErrUntrustedSessionKey)
	}
	return u.key, nil
}

// SealKeys are the keys Seal uses. Signer is the primary key of our member's
// address alone in its ring (it signs every part). CalendarPrimary is the
// primary calendar key alone in its ring: the recipient of new key packets,
// needed only when a key is minted. Shared and Calendar are the row's session
// keys on an update (from Event.SessionKeys); nil mints a fresh AES-256 key
// when a part needs it and returns its key packet.
type SealKeys struct {
	Signer, CalendarPrimary *crypto.KeyRing
	Shared, Calendar        *crypto.SessionKey
}

// sealGroup is one of the Event's part lists: its signed and encrypted
// kinds (-1: none) and the session key of the encrypted one.
type sealGroup struct {
	list           *events.PartList
	signed, sealed ical.PartKind
	key            **crypto.SessionKey
}

const noKind ical.PartKind = -1

// Seal makes the wire parts of parts, fragments by kind (the personal part
// is never written). A kind absent from parts is not emitted; each list is
// non-nil, [] when empty. Each session key is handled alone: given, it is
// reused and no key packet is made; nil, a fresh AES-256 key is minted only
// when a part is encrypted under it, wrapped to k.CalendarPrimary into
// SharedKeyPacket or CalendarKeyPacket, and cleared once used. Seal refuses
// a signer or recipient ring that does not hold exactly one key: gopenpgp
// signs with the first key of a ring and wraps to every key of one.
func Seal(parts map[ical.PartKind][]byte, k SealKeys) (events.WireEvent, error) {
	for kind, text := range parts {
		if kind < ical.SharedSigned || kind > ical.Attendees {
			return events.WireEvent{}, fmt.Errorf("sealing: %s cannot be written", Kind(kind))
		}
		if len(text) == 0 {
			return events.WireEvent{}, fmt.Errorf("sealing the %s: empty fragment", kind)
		}
	}
	if k.Signer == nil || len(k.Signer.GetKeys()) != 1 {
		return events.WireEvent{}, errors.New("sealing: the signer must be one key")
	}
	shared, sharedKP, err := sessionKey(k.Shared, k.CalendarPrimary, has(parts, ical.SharedEncrypted, ical.Attendees))
	if err != nil {
		return events.WireEvent{}, fmt.Errorf("sealing: the shared session key: %w", err)
	}
	if k.Shared == nil && shared != nil {
		defer clear(shared.Key)
	}
	calendar, calKP, err := sessionKey(k.Calendar, k.CalendarPrimary, has(parts, ical.CalendarEncrypted))
	if err != nil {
		return events.WireEvent{}, fmt.Errorf("sealing: the calendar session key: %w", err)
	}
	if k.Calendar == nil && calendar != nil {
		defer clear(calendar.Key)
	}

	we := events.WireEvent{
		SharedKeyPacket: sharedKP, CalendarKeyPacket: calKP,
		SharedEventContent: events.PartList{}, CalendarEventContent: events.PartList{}, AttendeesEventContent: events.PartList{},
	}
	for _, g := range []sealGroup{
		{&we.SharedEventContent, ical.SharedSigned, ical.SharedEncrypted, &shared},
		{&we.CalendarEventContent, ical.CalendarSigned, ical.CalendarEncrypted, &calendar},
		{&we.AttendeesEventContent, noKind, ical.Attendees, &shared},
	} {
		if text, ok := parts[g.signed]; ok && g.signed != noKind {
			sig, err := signDetached(k.Signer, text)
			if err != nil {
				return events.WireEvent{}, fmt.Errorf("sealing the %s: %w", g.signed, err)
			}
			*g.list = append(*g.list, events.Part{Type: events.PartSigned, Data: string(text), Signature: sig})
		}
		if text, ok := parts[g.sealed]; ok {
			p, err := encryptPart(*g.key, k.Signer, text)
			if err != nil {
				return events.WireEvent{}, fmt.Errorf("sealing the %s: %w", g.sealed, err)
			}
			*g.list = append(*g.list, p)
		}
	}
	return we, nil
}

// has reports whether parts holds any of kinds.
func has(parts map[ical.PartKind][]byte, kinds ...ical.PartKind) bool {
	for _, k := range kinds {
		if _, ok := parts[k]; ok {
			return true
		}
	}
	return false
}

// sessionKey returns given (no key packet), or, when needed, a fresh
// AES-256 key and its key packet to recipient, base64; nil when neither.
func sessionKey(given *crypto.SessionKey, recipient *crypto.KeyRing, needed bool) (*crypto.SessionKey, string, error) {
	switch {
	case given != nil:
		return given, "", nil
	case !needed:
		return nil, "", nil
	case recipient == nil || len(recipient.GetKeys()) != 1:
		return nil, "", errors.New("the recipient must be the primary calendar key alone")
	}
	sk, err := crypto.GenerateSessionKey()
	if err != nil {
		return nil, "", errors.New("minting failed")
	}
	kp, err := recipient.EncryptSessionKey(sk)
	if err != nil {
		clear(sk.Key)
		return nil, "", errors.New("wrapping it to the calendar key failed")
	}
	return sk, base64.StdEncoding.EncodeToString(kp), nil
}

// signDetached is an armored detached binary-mode signature of the exact
// bytes of text (NewPlainMessage: binary; a string message would be text
// mode, canonicalising line ends).
func signDetached(signer *crypto.KeyRing, text []byte) (string, error) {
	sig, err := signer.SignDetached(crypto.NewPlainMessage(text))
	if err != nil {
		return "", errors.New("signing failed")
	}
	armored, err := sig.GetArmored()
	if err != nil {
		return "", errors.New("armoring the signature failed")
	}
	return armored, nil
}

// encryptPart is text encrypted under sk (one SEIPD packet, not compressed),
// base64, with a detached signature over text.
func encryptPart(sk *crypto.SessionKey, signer *crypto.KeyRing, text []byte) (events.Part, error) {
	if sk == nil {
		return events.Part{}, errors.New("no session key") // unreachable: sessionKey made one
	}
	data, err := sk.Encrypt(crypto.NewPlainMessage(text))
	if err != nil {
		return events.Part{}, errors.New("encrypting failed")
	}
	sig, err := signDetached(signer, text)
	if err != nil {
		return events.Part{}, err
	}
	return events.Part{Type: events.PartEncrypted, Data: base64.StdEncoding.EncodeToString(data), Signature: sig}, nil
}
