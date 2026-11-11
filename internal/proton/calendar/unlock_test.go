package calendar_test

import (
	"strings"
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
	if n := a.KR.CountEntities(); n != 1 {
		t.Fatalf("keyring holds %d keys, want 1", n)
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
