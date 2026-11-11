package service

import (
	"slices"
	"testing"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
)

// Ruling M3-R16: each address key ID is tagged with whether its key is in
// the address's unlocked keyring, so a reactivated key changes the tags.
func TestAddressKeyTagsMarkUnlockedKeys(t *testing.T) {
	gen := func() (*crypto.Key, []byte) {
		k, err := crypto.GenerateKey("a", "a@x", "x25519", 0)
		if err != nil {
			t.Fatal(err)
		}
		armored, err := k.Armor()
		if err != nil {
			t.Fatal(err)
		}
		return k, []byte(armored)
	}
	open, openArmor := gen()
	_, lockedArmor := gen()
	kr, err := crypto.NewKeyRing(open)
	if err != nil {
		t.Fatal(err)
	}
	u := &keys.Unlocked{
		Addresses: []gpa.Address{{ID: "addr1", Keys: gpa.Keys{{ID: "k2", PrivateKey: lockedArmor}, {ID: "k1", PrivateKey: openArmor}}}},
		AddrKRs:   map[string]*crypto.KeyRing{"addr1": kr},
	}
	if got, want := addressKeyTags(u), []string{"k1:unlocked", "k2:locked"}; !slices.Equal(got, want) {
		t.Errorf("addressKeyTags = %v, want %v", got, want)
	}
}
