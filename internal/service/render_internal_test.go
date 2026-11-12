package service

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"
	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/ical"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/cards"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
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

// A recurring timed placeholder keeps the row's local time across a DST
// change, in the row's tzdata zone; any other row stays in UTC (audit
// read-M3).
func TestPlaceholderRecurringUsesTheRowZone(t *testing.T) {
	paris, _ := time.LoadLocation("Europe/Paris")
	at := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 9, 0, 0, 0, paris).Unix() }
	row := func(start int64, rrule, raw string) *cache.StoredRow {
		return &cache.StoredRow{ID: "r", UID: "u@x", Start: start, End: start + 3600, RRule: rrule, Raw: json.RawMessage(raw)}
	}
	const parisRaw = `{"StartTimezone":"Europe/Paris","EndTimezone":"Europe/Paris"}`
	render := func(r *cache.StoredRow) string { return string(ical.Object(new(placeholder(r)), nil)) }
	// 09:00 Paris is 08:00Z in winter and 07:00Z in summer: same local time.
	for _, start := range []int64{at(2026, time.January, 12), at(2026, time.July, 13)} {
		got := render(row(start, "FREQ=WEEKLY;BYDAY=MO", parisRaw))
		for _, want := range []string{"DTSTART;TZID=Europe/Paris:", "T090000\r\n", "DTEND;TZID=Europe/Paris:", "T100000\r\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("start %d: missing %q in\n%s", start, want, got)
			}
		}
		if strings.Contains(got, "DTSTART:") {
			t.Errorf("start %d: still a UTC DTSTART:\n%s", start, got)
		}
	}
	// 00:30 Paris on a Monday is Sunday 23:30Z: BYDAY=MO stays right in the zone.
	mon := time.Date(2026, time.July, 13, 0, 30, 0, 0, paris).Unix()
	if got := render(row(mon, "FREQ=WEEKLY;BYDAY=MO", parisRaw)); !strings.Contains(got, "DTSTART;TZID=Europe/Paris:20260713T003000\r\n") {
		t.Errorf("midnight series:\n%s", got)
	}
	// UTC as before: a single event, an unknown zone, no zone, UTC itself, an injection.
	for name, r := range map[string]*cache.StoredRow{
		"single":  row(at(2026, time.July, 13), "", parisRaw),
		"unknown": row(at(2026, time.July, 13), "FREQ=DAILY", `{"StartTimezone":"Mars/Olympus"}`),
		"none":    row(at(2026, time.July, 13), "FREQ=DAILY", `{}`),
		"utc":     row(at(2026, time.July, 13), "FREQ=DAILY", `{"StartTimezone":"UTC"}`),
		"local":   row(at(2026, time.July, 13), "FREQ=DAILY", `{"StartTimezone":"Local"}`),
		"inject":  row(at(2026, time.July, 13), "FREQ=DAILY", `{"StartTimezone":"Europe/Paris\r\nX-EVIL:1"}`),
	} {
		if got := render(r); !strings.Contains(got, "DTSTART:20260713T070000Z\r\n") || strings.Contains(got, "TZID") {
			t.Errorf("%s: want a UTC placeholder:\n%s", name, got)
		}
	}
}
