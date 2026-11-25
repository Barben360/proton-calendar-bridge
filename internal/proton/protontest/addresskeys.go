package protontest

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ProtonMail/gopenpgp/v2/crypto"
)

// extraAddressKey is the key Options.ExtraAddressKey adds, unlocked: an
// address key that is neither the account's user key nor any address's
// primary one, so a part it signs does not verify as the address's.
var extraAddressKey = sync.OnceValues(func() (*crypto.Key, error) {
	key, err := crypto.GenerateKey("Extra address key", "extra@protontest.invalid", "x25519", 0)
	if err != nil {
		return nil, err
	}
	return warm(key), nil
})

// ExtraAddressKeyRing returns a copy of the unlocked key Options.ExtraAddressKey
// lists on every account's first address.
func (f *Fake) ExtraAddressKeyRing() *crypto.KeyRing {
	f.t.Helper()
	return f.keyCopy(func() (*crypto.KeyRing, error) {
		k, err := extraAddressKey()
		if err != nil {
			return nil, err
		}
		return crypto.NewKeyRing(k)
	})
}

// addExtraAddressKey puts, first in the key list of each address whose
// primary key is signer's (every account's first address), an active key
// that is not primary: extraAddressKey, locked with the passphrase the
// primary key's token holds and listed with that token, so the account's
// key passphrase unlocks it as it unlocks the primary key. body is a GET
// /core/v4/addresses answer.
func addExtraAddressKey(body map[string]any) error {
	sig, err := signer()
	if err != nil {
		return err
	}
	fp := sig.GetKeys()[0].GetFingerprint()
	extra, err := extraAddressKey()
	if err != nil {
		return err
	}
	addrs, _ := body["Addresses"].([]any)
	for _, a := range addrs {
		addr, _ := a.(map[string]any)
		list, _ := addr["Keys"].([]any)
		var primary map[string]any
		for _, k := range list {
			if key, _ := k.(map[string]any); isTrue(key["Primary"]) {
				primary = key
			}
		}
		if primary == nil {
			continue
		}
		armored, _ := primary["PrivateKey"].(string)
		if pk, err := crypto.NewKeyFromArmored(armored); err != nil || pk.GetFingerprint() != fp {
			continue
		}
		token, _ := primary["Token"].(string)
		if token == "" {
			return errors.New("the primary address key has no token")
		}
		// The user key is signer's key (generateKey), so signer opens the token.
		msg, err := crypto.NewPGPMessageFromArmored(token)
		if err != nil {
			return err
		}
		secret, err := sig.Decrypt(msg, nil, 0)
		if err != nil {
			return fmt.Errorf("opening the primary key's token: %w", err)
		}
		locked, err := extra.Lock(secret.GetBinary())
		if err != nil {
			return err
		}
		lockedArmor, err := locked.Armor()
		if err != nil {
			return err
		}
		id, _ := primary["ID"].(string)
		key := map[string]any{"ID": id + "-extra", "PrivateKey": lockedArmor, "Token": token, "Signature": primary["Signature"],
			"Primary": 0, "Active": 1, "Flags": 3}
		addr["Keys"] = append([]any{key}, list...)
	}
	return nil
}

// isTrue reads a JSON boolean that Proton sends as 0/1 (or true/false).
func isTrue(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case float64:
		return b != 0
	}
	return false
}
