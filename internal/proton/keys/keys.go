// Package keys turns a logged-in session and the login password into
// unlocked Proton keys: it fetches the user, addresses and key salts
// (elevating the session scope only when Proton demands it), derives and
// checks the salted key passphrase, and unlocks the user and address keys.
// Unlocked keys live in memory only; the salted passphrase is returned to the
// caller for sealing and appears in no error.
package keys

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

// ErrKeysLocked means the password or passphrase does not unlock any address
// key.
var ErrKeysLocked = errors.New("the key passphrase does not unlock any address key")

// lockTimeout bounds the Lock call that follows an elevation, which must run
// even when the caller's context is already cancelled.
const lockTimeout = 10 * time.Second

// Elevator gains and drops the locked scope (satisfied by auth.Scope).
type Elevator interface {
	Unlock(ctx context.Context) error
	Lock(ctx context.Context) error
}

// Fetch returns the user and the addresses, sorted by Order.
func Fetch(ctx context.Context, c *api.Client) (gpa.User, []gpa.Address, error) {
	var u struct{ User gpa.User }
	if err := c.Get(ctx, "/core/v4/users", nil, &u); err != nil {
		return gpa.User{}, nil, fmt.Errorf("fetching the user: %w", err)
	}
	var a struct{ Addresses []gpa.Address }
	if err := c.Get(ctx, "/core/v4/addresses", nil, &a); err != nil {
		return gpa.User{}, nil, fmt.Errorf("fetching the addresses: %w", err)
	}
	slices.SortStableFunc(a.Addresses, func(x, y gpa.Address) int { return cmp.Compare(x.Order, y.Order) })
	return u.User, a.Addresses, nil
}

// FetchSalts returns the key salts. Only when Proton answers code 9101
// (insufficient scope) does it call e.Unlock, retry once and e.Lock again; a
// Lock failure is returned only when the fetch succeeded.
func FetchSalts(ctx context.Context, c *api.Client, e Elevator) (gpa.Salts, error) {
	salts, err := getSalts(ctx, c)
	if !api.IsCode(err, api.CodeInsufficientScope) {
		return salts, err
	}
	if err := e.Unlock(ctx); err != nil {
		return nil, fmt.Errorf("gaining the scope for the key salts: %w", err)
	}
	salts, err = getSalts(ctx, c)
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockTimeout)
	defer cancel()
	if lerr := e.Lock(lctx); lerr != nil && err == nil {
		return salts, fmt.Errorf("dropping the scope after the key salts: %w", lerr)
	}
	return salts, err
}

func getSalts(ctx context.Context, c *api.Client) (gpa.Salts, error) {
	var out struct{ KeySalts gpa.Salts }
	if err := c.Get(ctx, "/core/v4/keys/salts", nil, &out); err != nil {
		return nil, fmt.Errorf("fetching the key salts: %w", err)
	}
	return out.KeySalts, nil
}

// DeriveKeyPass derives the salted key passphrase from password and the salt
// of the primary user key (the first key when none is primary), and checks it
// by unlocking the keys: at least one address keyring must open, else
// ErrKeysLocked and a nil passphrase. The result is a secret.
func DeriveKeyPass(user gpa.User, addrs []gpa.Address, salts gpa.Salts, password []byte) ([]byte, error) {
	if len(user.Keys) == 0 {
		return nil, errors.New("the user has no key")
	}
	primary := user.Keys[0].ID
	for _, k := range user.Keys {
		if k.Primary {
			primary = k.ID
			break
		}
	}
	pass, err := salts.SaltForKey(password, primary)
	if err != nil {
		return nil, fmt.Errorf("deriving the key passphrase: %w", err)
	}
	if len(pass) == 0 {
		return nil, errors.New("deriving the key passphrase: empty result")
	}
	u, err := Unlock(user, addrs, pass)
	if err != nil {
		clear(pass)
		return nil, err
	}
	u.Close()
	return pass, nil
}

// Unlocked holds the unlocked key material of one session, in memory only.
type Unlocked struct {
	User      gpa.User
	Addresses []gpa.Address
	// AddrKRs maps an address ID to its unlocked keyring; addresses whose keys
	// do not unlock are absent.
	AddrKRs map[string]*crypto.KeyRing
}

// Unlock opens the user keys with keyPass, then the address keys. It returns
// ErrKeysLocked when no address keyring opens. Call Close when done.
func Unlock(user gpa.User, addrs []gpa.Address, keyPass []byte) (*Unlocked, error) {
	userKR, addrKRs, err := gpa.Unlock(user, addrs, keyPass, nil)
	if err != nil {
		// The library's text is dropped on purpose: it is not ours to vouch as secret-free.
		return nil, fmt.Errorf("%w: no address key opened", ErrKeysLocked)
	}
	userKR.ClearPrivateParams()
	// go-crypto writes a cache on a key's first Serialize (so on its first
	// Copy): do it once here, before the keys are shared, so no later copy
	// writes concurrently (ruling M4-R4).
	for _, kr := range addrKRs {
		for _, k := range kr.GetKeys() {
			_, _ = k.Serialize()
		}
	}
	return &Unlocked{User: user, Addresses: addrs, AddrKRs: addrKRs}, nil
}

// Close clears the private parameters of every keyring and drops them. It is
// idempotent and nil-safe.
func (u *Unlocked) Close() {
	if u == nil {
		return
	}
	for _, kr := range u.AddrKRs {
		kr.ClearPrivateParams()
	}
	u.AddrKRs = nil
}
