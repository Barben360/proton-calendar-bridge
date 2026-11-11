package calendar_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/gopenpgp/v2/crypto"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/calendar"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

func decrypts(t *testing.T, a *calendar.Access, armored string) {
	t.Helper()
	msg, err := crypto.NewPGPMessageFromArmored(armored)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := a.KR.Decrypt(msg, nil, 0)
	if err != nil {
		t.Fatalf("decrypt with the calendar keyring: %v", err)
	}
	if got := string(plain.GetBinary()); got != "probe" {
		t.Fatalf("decrypted %q, want probe", got)
	}
}

func firstCalendar(t *testing.T, c *api.Client) calendar.Calendar {
	t.Helper()
	cals, err := calendar.List(t.Context(), c)
	if err != nil || len(cals) == 0 {
		t.Fatalf("List: %v (%d)", err, len(cals))
	}
	return cals[0]
}

func TestUnlockDecryptsWithCalendarKey(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{Name: "A"})
	cal := firstCalendar(t, c)
	a, err := calendar.Unlock(t.Context(), c, u, cal)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	decrypts(t, a, f.EncryptToCalendar(id, []byte("probe")))
	if a.CalendarID != id || a.MemberID != cal.MemberID || a.AddressID != cal.AddressID {
		t.Errorf("identity = %+v", a)
	}
	s := a.Settings
	if s.DefaultEventDuration != 30 || s.MakesUserBusy != 1 ||
		len(s.DefaultPartDayNotifications) != 1 || s.DefaultPartDayNotifications[0] != (calendar.Notification{Type: 1, Trigger: "-PT1H"}) ||
		len(s.DefaultFullDayNotifications) != 1 {
		t.Errorf("settings = %+v", s)
	}
}

func TestUnlockResolvesMemberByEmail(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{})
	cal := firstCalendar(t, c)
	u.Addresses[0].Email = strings.ToUpper(u.Addresses[0].Email) // the match ignores case
	a, err := calendar.Unlock(t.Context(), c, u, calendar.Calendar{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.MemberID != cal.MemberID || a.AddressID != cal.AddressID {
		t.Fatalf("member %q/%q, want %q/%q", a.MemberID, a.AddressID, cal.MemberID, cal.AddressID)
	}
}

func TestUnlockNoMemberOfOurs(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{})
	u.Addresses[0].Email = "someone-else@example.org"
	if _, err := calendar.Unlock(t.Context(), c, u, calendar.Calendar{ID: id}); err == nil {
		t.Fatal("want an error when none of our addresses is a member")
	}
}

func TestUnlockPassphraseToSecondAddress(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	f.AddAddress(f.Username, "second@proton.local")
	u := unlockKeys(t, c, f.Password) // again, now that the user has two addresses
	id := f.AddCalendar(f.Username, protontest.Calendar{ToSecondAddress: true})
	// The fake's addresses may share key material; give the first address an
	// unrelated keyring so only the second can decrypt.
	unrelated, err := crypto.GenerateKey("x", "", "x25519", 0)
	if err != nil {
		t.Fatal(err)
	}
	kr, err := crypto.NewKeyRing(unrelated)
	if err != nil {
		t.Fatal(err)
	}
	u2 := *u
	u2.AddrKRs = map[string]*crypto.KeyRing{}
	for k, v := range u.AddrKRs {
		u2.AddrKRs[k] = v
	}
	u2.AddrKRs[u.Addresses[0].ID] = kr
	a, err := calendar.Unlock(t.Context(), c, &u2, firstCalendar(t, c))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	decrypts(t, a, f.EncryptToCalendar(id, []byte("probe")))
	if a.AddressID != u.Addresses[1].ID {
		t.Errorf("AddressID = %q, want the second address %q", a.AddressID, u.Addresses[1].ID)
	}
}

func TestUnlockSkipsUndecryptableKey(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{ExtraLockedKey: true})
	a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// The fake lists the locked key, then a retired key and the primary
	// one, both opening with the passphrase.
	if n := a.KR.CountEntities(); n != 2 {
		t.Fatalf("keyring holds %d keys, want 2 (every key but the locked one)", n)
	}
	// Every listed key is named, the locked one too, with whether it
	// unlocked: the list changes on a rotation and on a reactivation.
	if want := []calendar.KeyState{{ID: "key0"}, {ID: "key1", Unlocked: true}, {ID: "key2", Unlocked: true}}; !slices.Equal(a.Keys, want) {
		t.Errorf("Keys = %+v, want %+v", a.Keys, want)
	}
	decrypts(t, a, f.EncryptToCalendar(id, []byte("probe")))
}

func TestUnlockBrokenPassphraseErrors(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{BrokenPassphrase: true})
	a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
	if err == nil {
		a.Close()
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), id) {
		t.Errorf("error %q does not name the calendar %s", err, id)
	}
	if strings.Contains(err.Error(), "-----BEGIN") || strings.Contains(err.Error(), "PGP MESSAGE") {
		t.Errorf("error leaks armor: %q", err)
	}
}

func TestUnlockUnknownCalendar(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{})
	cal := firstCalendar(t, c)
	f.FailNext("GET", "/calendar/v2/"+id+"/bootstrap", 422, 2501, 0)
	_, err := calendar.Unlock(t.Context(), c, u, cal)
	if !api.IsCode(err, 2501) {
		t.Fatalf("err = %v, want code 2501", err)
	}
	if _, err := calendar.Unlock(t.Context(), c, u, calendar.Calendar{ID: "nope"}); !api.IsCode(err, 2501) {
		t.Fatalf("a really unknown calendar: err = %v, want code 2501", err)
	}
}

func TestUnlockEscapesTheCalendarID(t *testing.T) {
	t.Parallel()
	_, c, u := session(t)
	// "a/b" must stay one path segment; unescaped it would be another route.
	_, err := calendar.Unlock(t.Context(), c, u, calendar.Calendar{ID: "a/b"})
	if !api.IsCode(err, 2501) {
		t.Fatalf("err = %v, want the fake's 2501 for an unknown calendar", err)
	}
}

func TestAccessCloseIsNilSafe(t *testing.T) {
	t.Parallel()
	var a *calendar.Access
	a.Close()
	(&calendar.Access{}).Close()
}

func TestUnlockChoosesOurMemberPassphrase(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{ForeignMemberFirst: true})
	a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	decrypts(t, a, f.EncryptToCalendar(id, []byte("probe")))
}

func TestUnlockUnknownMemberNeverFallsBack(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	// Without the foreign entry the only entry is decryptable by us: a
	// fallback to "the first entry" would succeed here.
	id := f.AddCalendar(f.Username, protontest.Calendar{})
	a, err := calendar.Unlock(t.Context(), c, u, calendar.Calendar{ID: id, MemberID: "no-such-member"})
	if err == nil {
		a.Close()
		t.Fatal("want an error: another member's entry must never be used")
	}
}

func TestUnlockResolvesSecondAddressMember(t *testing.T) {
	t.Parallel()
	f, c, _ := session(t)
	f.AddAddress(f.Username, "second@proton.local")
	u := unlockKeys(t, c, f.Password)
	id := f.AddCalendar(f.Username, protontest.Calendar{ToSecondAddress: true, ForeignMemberFirst: true})
	cal := firstCalendar(t, c)
	a, err := calendar.Unlock(t.Context(), c, u, calendar.Calendar{ID: id}) // no member given
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.MemberID != cal.MemberID || a.AddressID != u.Addresses[1].ID {
		t.Fatalf("member %q address %q, want %q and the second address %q", a.MemberID, a.AddressID, cal.MemberID, u.Addresses[1].ID)
	}
}

func fingerprints(kr *crypto.KeyRing) []string {
	var out []string
	for _, k := range kr.GetKeys() {
		out = append(out, k.GetFingerprint())
	}
	return out
}

// Would fail if Primary took any key but the one flagged 2 (say the first or
// the last unlocked one), or the whole keyring.
func TestPrimaryKeyIsTheFlaggedOne(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	id := f.AddCalendar(f.Username, protontest.Calendar{ExtraLockedKey: true})
	a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.Primary == nil || a.Primary.CountEntities() != 1 {
		t.Fatalf("Primary = %v, want a ring of exactly one key", a.Primary)
	}
	want := fingerprints(f.CalendarKeyRing(id))
	if got := fingerprints(a.Primary); !slices.Equal(got, want) {
		t.Errorf("Primary = %v, want the flagged key %v", got, want)
	}
	if retired := fingerprints(f.RetiredCalendarKeyRing(id)); slices.Equal(fingerprints(a.Primary), retired) {
		t.Error("Primary is the retired key")
	}
	decrypts(t, a, f.EncryptToCalendar(id, []byte("probe")))
	msg, err := crypto.NewPGPMessageFromArmored(f.EncryptToCalendar(id, []byte("probe")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Primary.Decrypt(msg, nil, 0); err != nil {
		t.Errorf("a message to the primary key does not open with Primary: %v", err)
	}
}

// Would fail if verification used no signature, the wrong plaintext, or never
// ran (PassphraseVerified false), or if Signer were another address's key.
func TestPassphraseSignatureVerifiedWithOurAddressKey(t *testing.T) {
	t.Parallel()
	t.Run("first address", func(t *testing.T) {
		t.Parallel()
		f, c, u := session(t)
		f.AddCalendar(f.Username, protontest.Calendar{})
		a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if !a.PassphraseVerified {
			t.Fatal("PassphraseVerified = false for a genuine signature")
		}
		if got, want := fingerprints(a.Signer), fingerprints(f.AddressKeyRing()); !slices.Equal(got, want) {
			t.Errorf("Signer = %v, want the first address's primary key %v", got, want)
		}
	})
	t.Run("second address", func(t *testing.T) {
		t.Parallel()
		f, c, _ := session(t)
		f.AddAddress(f.Username, "second@proton.local")
		u := unlockKeys(t, c, f.Password)
		f.AddCalendar(f.Username, protontest.Calendar{ToSecondAddress: true})
		a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if !a.PassphraseVerified {
			t.Fatal("PassphraseVerified = false: the second address's key signed it")
		}
		want, first := fingerprints(f.OtherAddressKeyRing()), fingerprints(f.AddressKeyRing())
		if slices.Equal(want, first) {
			t.Fatal("precondition: the two addresses share a key")
		}
		if got := fingerprints(a.Signer); !slices.Equal(got, want) || a.Signer.CountEntities() != 1 {
			t.Errorf("Signer = %v, want the member's address key %v alone", got, want)
		}
	})
	t.Run("signer signs and the address key verifies", func(t *testing.T) {
		t.Parallel()
		f, c, u := session(t)
		f.AddCalendar(f.Username, protontest.Calendar{})
		a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
		if err != nil {
			t.Fatal(err)
		}
		msg := crypto.NewPlainMessage([]byte("x"))
		sig, err := a.Signer.SignDetached(msg)
		if err != nil {
			t.Fatalf("Signer cannot sign: %v", err)
		}
		if err := f.AddressKeyRing().VerifyDetached(msg, sig, crypto.GetUnixTime()); err != nil {
			t.Errorf("signature does not verify with the address key: %v", err)
		}
		// Close clears the copy only: the account's key still signs.
		a.Close()
		if _, err := u.AddrKRs[a.AddressID].SignDetached(msg); err != nil {
			t.Errorf("closing the calendar broke the shared address key: %v", err)
		}
	})
}

// Would fail if a bad signature were an unlock error, or if it left the
// calendar writable.
func TestBadPassphraseSignatureLeavesTheCalendarReadOnly(t *testing.T) {
	t.Parallel()
	f, c, u := sessionWith(t, protontest.Options{BadPassphraseSignature: true})
	id := f.AddCalendar(f.Username, protontest.Calendar{})
	a, err := calendar.Unlock(t.Context(), c, u, firstCalendar(t, c))
	if err != nil {
		t.Fatalf("a bad signature must not fail the unlock: %v", err)
	}
	defer a.Close()
	if a.PassphraseVerified || a.Writable() {
		t.Errorf("verified %v, writable %v; want neither", a.PassphraseVerified, a.Writable())
	}
	decrypts(t, a, f.EncryptToCalendar(id, []byte("probe"))) // still readable
}

// Would fail if Writable ignored the type, the Write bit (8), or the
// verification, or accepted any non-zero permission.
func TestWritableNeedsTheWriteBit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		typ   int
		perms int
		bad   bool
		want  bool
	}{
		{"owner", 0, 0, false, true}, // the fake serves 127
		{"write bit alone", 0, 8, false, true},
		{"view only", 0, 2 | 4, false, false},
		{"availability only", 0, 1, false, false},
		{"admin without write", 0, 16 | 32, false, false},
		{"write but a bad signature", 0, 8, true, false},
		{"write but a subscribed calendar", 1, 8, false, false},
		{"write but holidays", 2, 127, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, c, u := sessionWith(t, protontest.Options{BadPassphraseSignature: tc.bad})
			f.AddCalendar(f.Username, protontest.Calendar{Type: tc.typ, Permissions: tc.perms})
			cal := firstCalendar(t, c)
			a, err := calendar.Unlock(t.Context(), c, u, cal)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if got := a.Writable(); got != tc.want {
				t.Errorf("Writable = %v, want %v (type %d, perms %d, verified %v)", got, tc.want, tc.typ, a.Permissions, a.PassphraseVerified)
			}
			if tc.perms != 0 && a.Permissions != tc.perms {
				t.Errorf("Permissions = %d, want the bootstrap's %d", a.Permissions, tc.perms)
			}
		})
	}
}

// Unlocking many calendars at once copies the one shared address key for
// each Signer: under -race this fails if the copy is not serialised (the
// go-crypto serialisation cache is written on a shared key's first copy).
func TestConcurrentUnlocksShareTheAddressKeySafely(t *testing.T) {
	t.Parallel()
	f, c, u := session(t)
	const n = 8
	for range n {
		f.AddCalendar(f.Username, protontest.Calendar{})
	}
	cals, err := calendar.List(t.Context(), c)
	if err != nil || len(cals) != n {
		t.Fatalf("List: %v (%d)", err, len(cals))
	}
	var wg sync.WaitGroup
	signers := make([]*calendar.Access, n)
	for i, cal := range cals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := calendar.Unlock(t.Context(), c, u, cal)
			if err != nil {
				t.Error(err)
				return
			}
			signers[i] = a
		}()
	}
	wg.Wait()
	for i, a := range signers {
		if a == nil || a.Signer == nil || !a.Writable() {
			t.Fatalf("calendar %d: not writable after a concurrent unlock", i)
		}
		a.Close()
	}
}
