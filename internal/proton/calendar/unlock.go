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

// Access is an unlocked calendar: its keyring (in memory only) and our member.
type Access struct {
	CalendarID, MemberID, AddressID string
	KR                              *crypto.KeyRing
	Settings                        Settings
}

// Close clears the keyring's private parameters. It is idempotent and
// nil-safe.
func (a *Access) Close() {
	if a == nil || a.KR == nil {
		return
	}
	a.KR.ClearPrivateParams()
	a.KR = nil
}

type bootstrap struct {
	Keys []struct{ PrivateKey string }
	// Passphrase.MemberPassphrases[].Signature is deliberately not decoded.
	Passphrase struct {
		MemberPassphrases []struct {
			MemberID   string
			Passphrase string
		}
	}
	Members          []member
	CalendarSettings Settings
}

// Unlock fetches the calendar's bootstrap and unlocks its keys: our member's
// passphrase is decrypted with the address keyrings, in address order, and
// opens every calendar key it can (at least one must).
//
// TODO(E10, M4): the member passphrase signature is NOT verified here; this
// is read-only use. It must be settled before anything is encrypted to a
// calendar key (M4).
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
	passphrase, err := decryptPassphrase(u, memberID, addressID, b)
	if err != nil {
		return nil, fmt.Errorf("calendar %s: %w", cal.ID, err)
	}
	defer clear(passphrase)
	var kr *crypto.KeyRing
	for _, k := range b.Keys {
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
	}
	if kr == nil {
		return nil, fmt.Errorf("calendar %s: the passphrase unlocks no calendar key", cal.ID)
	}
	return &Access{CalendarID: cal.ID, MemberID: memberID, AddressID: addressID, KR: kr, Settings: b.CalendarSettings}, nil
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
func decryptPassphrase(u *keys.Unlocked, memberID, addressID string, b bootstrap) ([]byte, error) {
	for _, e := range b.Passphrase.MemberPassphrases {
		if e.MemberID != memberID {
			continue
		}
		msg, err := crypto.NewPGPMessageFromArmored(e.Passphrase)
		if err != nil {
			return nil, errors.New("the member passphrase is not valid PGP")
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
			return plain.GetBinary(), nil
		}
		return nil, errors.New("no address key decrypts the member passphrase")
	}
	return nil, errors.New("no passphrase for our member")
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
