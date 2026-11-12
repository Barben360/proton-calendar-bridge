package calendar

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
)

// Notification is a default reminder of a calendar.
type Notification struct {
	Type    int
	Trigger string
}

// Settings are a calendar's defaults, from the bootstrap.
type Settings struct {
	DefaultEventDuration        int
	MakesUserBusy               int
	DefaultPartDayNotifications []Notification
	DefaultFullDayNotifications []Notification
}

// PermissionWrite is the bit of a member's Permissions that allows writing.
// The value comes from caloxide's constants and is not confirmed by Proton's
// own sources [claim: caloxide] (research H); E14 settles it on the live
// account (owner, edit and view shares).
const PermissionWrite = 8

// keyFlagPrimary is the bit of a bootstrap calendar key's Flags that marks the
// key new data is encrypted to (1 active, 2 primary; research B.3).
const keyFlagPrimary = 2

// Access is an unlocked calendar: its keyring (in memory only) and our member.
type Access struct {
	CalendarID, MemberID, AddressID string
	KR                              *crypto.KeyRing
	// Primary is the calendar key whose Flags include 2, alone in its ring
	// (nil when it did not unlock): the only recipient of new key packets.
	Primary *crypto.KeyRing
	// Signer is the primary key of our member's address, alone in its ring
	// (nil when the address has no unlocked primary key): it signs every part.
	// It is a copy: Close clears it without touching the account's keys.
	Signer *crypto.KeyRing
	// Type is the calendar's type, from the caller (the bootstrap has none).
	Type Type
	// Permissions is our member's bitmask in the bootstrap.
	Permissions int
	// PassphraseVerified reports that the member passphrase's signature
	// verifies, over the decrypted passphrase, with our member's address
	// keyring (research G, ruling R17): the calendar key is ours, not one the
	// server substituted. False is not an unlock error: the calendar stays
	// readable.
	PassphraseVerified bool
	// PassphraseChecked reports that the verification ran on good data: a
	// signature was served and our member's address keyring is unlocked.
	// Checked and not verified is a verdict (ruling M4-R39: read only);
	// not checked leaves the write gate undecided.
	PassphraseChecked bool
	Settings          Settings
	// Keys are the calendar keys the bootstrap lists, in its order, and
	// whether each unlocked: they change when the calendar key rotates or a
	// key opens again (reactivation).
	Keys []KeyState
}

// KeyState is one listed calendar key: its ID and whether it unlocked.
type KeyState struct {
	ID       string
	Unlocked bool
}

// Writable reports whether the calendar may be written (ruling M4-R1): a
// normal calendar, our member has the Write bit, the member passphrase is
// verified, and both write keys exist.
func (a *Access) Writable() bool {
	return a != nil && a.Type == Normal && a.Permissions&PermissionWrite != 0 && a.PassphraseVerified &&
		a.Primary != nil && a.Signer != nil
}

// Gate is the write gate's verdict (ruling M4-R39).
type Gate int

// The verdicts.
const (
	// GateUndecided: the data to decide is missing or not checkable yet
	// (the keys, the passphrase signature, a write key): neither writable
	// nor read only (CalDAV: 503 on a write).
	GateUndecided Gate = iota
	// GateOpen: Writable.
	GateOpen
	// GateClosed: read only, decided from good data: the type is not
	// normal, the Write bit is off, or the passphrase signature was checked
	// and does not verify (CalDAV: 403 need-privileges).
	GateClosed
)

// Gate is the write gate's verdict on a (ruling M4-R39); a nil Access
// (keys not unlocked) is undecided unless typ alone closes it.
func (a *Access) Gate(typ Type) Gate {
	switch {
	case typ != Normal:
		return GateClosed
	case a == nil:
		return GateUndecided
	case a.Type != Normal, a.Permissions&PermissionWrite == 0, a.PassphraseChecked && !a.PassphraseVerified:
		return GateClosed
	case a.Writable():
		return GateOpen
	}
	return GateUndecided
}

// Close clears the keyrings' private parameters. It is idempotent and
// nil-safe.
func (a *Access) Close() {
	if a == nil {
		return
	}
	if a.KR != nil {
		a.KR.ClearPrivateParams()
		a.KR = nil
	}
	if a.Primary != nil {
		a.Primary.ClearPrivateParams()
		a.Primary = nil
	}
	if a.Signer != nil {
		a.Signer.ClearPrivateParams()
		a.Signer = nil
	}
}

type bootstrap struct {
	Keys []struct {
		ID, PrivateKey string
		Flags          int
	}
	Passphrase struct {
		MemberPassphrases []struct {
			MemberID              string
			Passphrase, Signature string
		}
	}
	Members          []member
	CalendarSettings Settings
}

// Unlock fetches the calendar's bootstrap and unlocks its keys: our member's
// passphrase is decrypted with the address keyrings, in address order, and
// opens every calendar key it can (at least one must). The signature of that
// passphrase is verified with the keys of our member's address (research G,
// ruling R17): a failure leaves PassphraseVerified false, never an error.
// cal.Type is kept in Access.Type for the write gate.
func Unlock(ctx context.Context, c *api.Client, u *keys.Unlocked, cal Calendar) (*Access, error) {
	var b bootstrap
	if err := c.Get(ctx, "/calendar/v2/"+url.PathEscape(cal.ID)+"/bootstrap", nil, &b); err != nil {
		return nil, fmt.Errorf("fetching the bootstrap of calendar %s: %w", cal.ID, err)
	}
	memberID, addressID := cal.MemberID, cal.AddressID
	if memberID == "" {
		for _, m := range b.Members {
			if ownsEmail(u, m.Email) {
				memberID, addressID = m.ID, m.AddressID
				break
			}
		}
	}
	if memberID == "" {
		return nil, fmt.Errorf("calendar %s: none of our addresses is a member", cal.ID)
	}
	passphrase, sig, err := decryptPassphrase(u, memberID, addressID, b)
	if err != nil {
		return nil, fmt.Errorf("calendar %s: %w", cal.ID, err)
	}
	defer clear(passphrase)
	var kr *crypto.KeyRing
	var flagged []*crypto.Key // unlocked keys flagged primary
	states := make([]KeyState, 0, len(b.Keys))
	for _, k := range b.Keys {
		states = append(states, KeyState{ID: k.ID})
		key, err := crypto.NewKeyFromArmored(k.PrivateKey)
		if err != nil {
			continue
		}
		unlocked, err := key.Unlock(passphrase)
		if err != nil {
			continue
		}
		if kr == nil {
			kr, err = crypto.NewKeyRing(unlocked)
		} else {
			err = kr.AddKey(unlocked)
		}
		if err != nil {
			if kr != nil {
				kr.ClearPrivateParams()
			}
			return nil, fmt.Errorf("calendar %s: building the keyring failed", cal.ID)
		}
		states[len(states)-1].Unlocked = true
		if k.Flags&keyFlagPrimary != 0 {
			flagged = append(flagged, unlocked)
		}
	}
	if kr == nil {
		return nil, fmt.Errorf("calendar %s: the passphrase unlocks no calendar key", cal.ID)
	}
	// Exactly one unlocked key flagged primary (ruling M4-R4): none or two is
	// an anomaly, and the calendar then fails closed (no Primary, not writable).
	var primary *crypto.KeyRing
	if len(flagged) == 1 {
		var perr error
		if primary, perr = crypto.NewKeyRing(flagged[0]); perr != nil {
			primary = nil
		}
	}
	a := &Access{CalendarID: cal.ID, MemberID: memberID, AddressID: addressID, KR: kr, Primary: primary,
		Type: cal.Type, Settings: b.CalendarSettings, Keys: states}
	for _, m := range b.Members {
		if m.ID == memberID {
			a.Permissions = m.Permissions
		}
	}
	a.PassphraseChecked = u.AddrKRs[addressID] != nil && sig != ""
	a.PassphraseVerified = verifyPassphrase(u.AddrKRs[addressID], passphrase, sig)
	a.Signer = primarySigner(u, addressID)
	return a, nil
}

// verifyPassphrase checks the detached armored signature sig over the
// decrypted passphrase with addrKR; any failure, a missing signature
// included, is false.
func verifyPassphrase(addrKR *crypto.KeyRing, passphrase []byte, sig string) bool {
	if addrKR == nil || sig == "" {
		return false
	}
	s, err := crypto.NewPGPSignatureFromArmored(sig)
	if err != nil {
		return false
	}
	return addrKR.VerifyDetached(crypto.NewPlainMessage(passphrase), s, crypto.GetUnixTime()) == nil
}

// primarySigner returns a ring holding a copy of the primary key of address
// addressID, matched by fingerprint among the unlocked keys of its keyring;
// nil when there is none. keys.Unlock serialised each address key once, so
// the copy writes no cache and concurrent unlocks do not race.
func primarySigner(u *keys.Unlocked, addressID string) *crypto.KeyRing {
	kr := u.AddrKRs[addressID]
	if kr == nil {
		return nil
	}
	var primaryFP string
	for _, a := range u.Addresses {
		if a.ID != addressID {
			continue
		}
		for _, k := range a.Keys {
			if !k.Primary {
				continue
			}
			// go-proton-api decodes the armor: PrivateKey is the binary key.
			if key, err := crypto.NewKey(k.PrivateKey); err == nil {
				primaryFP = key.GetFingerprint()
			}
		}
	}
	if primaryFP == "" {
		return nil
	}
	for _, k := range kr.GetKeys() {
		if k.GetFingerprint() != primaryFP {
			continue
		}
		c, err := k.Copy()
		if err != nil {
			return nil
		}
		ring, err := crypto.NewKeyRing(c)
		if err != nil {
			return nil
		}
		return ring
	}
	return nil
}

func ownsEmail(u *keys.Unlocked, email string) bool {
	for _, a := range u.Addresses {
		if strings.EqualFold(a.Email, email) {
			return true
		}
	}
	return false
}

// decryptPassphrase decrypts the entry of memberID (never another member's)
// with the member's address keyring first, then every other address keyring in
// address order, since the passphrase may be encrypted to any of them. Library errors are dropped so
// no armor or key detail reaches the message.
func decryptPassphrase(u *keys.Unlocked, memberID, addressID string, b bootstrap) (plain []byte, sig string, err error) {
	for _, e := range b.Passphrase.MemberPassphrases {
		if e.MemberID != memberID {
			continue
		}
		msg, err := crypto.NewPGPMessageFromArmored(e.Passphrase)
		if err != nil {
			return nil, "", errors.New("the member passphrase is not valid PGP")
		}
		for _, a := range addressOrder(u.Addresses, addressID) {
			kr, ok := u.AddrKRs[a.ID]
			if !ok {
				continue
			}
			plain, err := kr.Decrypt(msg, nil, 0)
			if err != nil {
				continue
			}
			return plain.GetBinary(), e.Signature, nil
		}
		return nil, "", errors.New("no address key decrypts the member passphrase")
	}
	return nil, "", errors.New("no passphrase for our member")
}

// addressOrder returns addrs with the address first listed first, the rest in
// their order.
func addressOrder(addrs []gpa.Address, first string) []gpa.Address {
	out := make([]gpa.Address, 0, len(addrs))
	for _, a := range addrs {
		if a.ID == first {
			out = append(out, a)
		}
	}
	for _, a := range addrs {
		if a.ID != first {
			out = append(out, a)
		}
	}
	return out
}
