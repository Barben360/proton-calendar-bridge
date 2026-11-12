package service

import (
	"encoding/json"
	"slices"
	"testing"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
)

// Ruling M3-R16: each address key ID is tagged with whether its key is in
// the address's unlocked keyring, so a reactivated key changes the tags. The
// fixture has the shape go-proton-api produces: the address is JSON-decoded
// from armored keys, and the decode stores each key in binary.
func TestAddressKeyTagsMarkUnlockedKeys(t *testing.T) {
	gen := func() (*crypto.Key, string) {
		k, err := crypto.GenerateKey("a", "a@x", "x25519", 0)
		if err != nil {
			t.Fatal(err)
		}
		armored, err := k.Armor()
		if err != nil {
			t.Fatal(err)
		}
		return k, armored
	}
	open, openArmor := gen()
	_, lockedArmor := gen()
	kr, err := crypto.NewKeyRing(open)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"ID": "addr1", "Keys": []map[string]any{
		{"ID": "k2", "PrivateKey": lockedArmor, "Active": 1}, {"ID": "k1", "PrivateKey": openArmor, "Active": 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var addr gpa.Address
	if err := json.Unmarshal(raw, &addr); err != nil {
		t.Fatal(err)
	}
	if _, err := crypto.NewKey(addr.Keys[0].PrivateKey); err != nil {
		t.Fatalf("precondition: go-proton-api does not store binary keys: %v", err)
	}
	u := &keys.Unlocked{Addresses: []gpa.Address{addr}, AddrKRs: map[string]*crypto.KeyRing{"addr1": kr}}
	if got, want := addressKeyTags(u), []string{"k1:unlocked", "k2:locked"}; !slices.Equal(got, want) {
		t.Errorf("addressKeyTags = %v, want %v", got, want)
	}
}

// SPEC §5.3, rulings M3-R33 and M3-R36: an own-authored part marks the
// event when its signature fails, or when it is a shared or calendar part
// without one; an attendees or personal part only when signed and failing;
// a part authored by an address not ours never (no key to check it in v1).
func TestUnverifiedOnlyForOwnAuthoredParts(t *testing.T) {
	ok := cards.Part{Kind: cards.SharedSigned, Own: true, Signed: true, Verified: true}
	for _, c := range []struct {
		name string
		part cards.Part
		want bool
	}{
		{"own, verified", ok, false},
		{"own, bad signature", cards.Part{Kind: cards.CalendarSigned, Own: true, Signed: true}, true},
		{"own shared signed part, no signature", cards.Part{Kind: cards.SharedSigned, Own: true}, true},
		{"own shared encrypted part, no signature", cards.Part{Kind: cards.SharedEncrypted, Own: true}, true},
		{"own calendar signed part, no signature", cards.Part{Kind: cards.CalendarSigned, Own: true}, true},
		{"own calendar encrypted part, no signature", cards.Part{Kind: cards.CalendarEncrypted, Own: true}, true},
		{"own attendees part, no signature", cards.Part{Kind: cards.Attendees, Own: true}, false},
		{"own personal part, no signature", cards.Part{Kind: cards.Personal, Own: true}, false},
		{"own attendees part, bad signature", cards.Part{Kind: cards.Attendees, Own: true, Signed: true}, true},
		{"own personal part, bad signature", cards.Part{Kind: cards.Personal, Own: true, Signed: true}, true},
		{"a stranger's, bad signature", cards.Part{Kind: cards.SharedSigned, Signed: true}, false},
		{"a stranger's, no signature", cards.Part{Kind: cards.SharedSigned}, false},
	} {
		if got := unverifiedParts([]cards.Part{ok, c.part}); got != c.want {
			t.Errorf("%s: unverified = %v, want %v", c.name, got, c.want)
		}
	}
	if unverifiedParts(nil) {
		t.Error("no parts: unverified")
	}
}

// A placeholder's RRULE comes from a plaintext column: one content line,
// without a "RRULE:" prefix should Proton's column carry one.
func TestPlaceholderRRuleIsOneValue(t *testing.T) {
	for in, want := range map[string]string{
		"FREQ=WEEKLY;BYDAY=FR":   "FREQ=WEEKLY;BYDAY=FR",
		"RRULE:FREQ=DAILY":       "FREQ=DAILY",
		"rrule:FREQ=DAILY":       "FREQ=DAILY",
		"FREQ=DAILY\r\nX-EVIL:1": "FREQ=DAILYX-EVIL:1",
		"":                       "",
	} {
		if got := placeholderRRule(in); got != want {
			t.Errorf("placeholderRRule(%q) = %q, want %q", in, got, want)
		}
	}
}
