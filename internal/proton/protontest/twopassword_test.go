package protontest_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// keyMaterial fetches the user, addresses and salts of tok's account through
// the fake, the salts behind the locked scope gained with f.Password.
func keyMaterial(t *testing.T, f *protontest.Fake, tok api.Tokens) (gpa.User, []gpa.Address, gpa.Salts) {
	t.Helper()
	c := api.New(tok, api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 2 * time.Minute}, // SRP checks run server-side, slow under -race
	})
	m := auth.NewManager(f.URL, "Other")
	t.Cleanup(m.Close)
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	salts, err := keys.FetchSalts(t.Context(), c, auth.Scope{M: m, C: c, Username: f.Username, Password: []byte(f.Password)})
	if err != nil {
		t.Fatalf("FetchSalts: %v", err)
	}
	return user, addrs, salts
}

// opens reports whether pass opens the keys of every address.
func opens(t *testing.T, user gpa.User, addrs []gpa.Address, salts gpa.Salts, pass string) bool {
	t.Helper()
	kp, err := keys.DeriveKeyPass(user, addrs, salts, []byte(pass))
	if errors.Is(err, keys.ErrKeysLocked) {
		return false
	}
	if err != nil || kp == nil {
		t.Fatalf("DeriveKeyPass = %v, %v; want a passphrase or ErrKeysLocked", kp != nil, err)
	}
	u, err := keys.Unlock(user, addrs, kp)
	if err != nil {
		t.Fatalf("Unlock with the derived passphrase: %v", err)
	}
	defer u.Close()
	if len(u.AddrKRs) != len(addrs) {
		t.Fatalf("%d of %d address keyrings open", len(u.AddrKRs), len(addrs))
	}
	return true
}

// TestTwoPasswordFakeSplitsThePasswords: the login password drives SRP and
// the scope unlock, the mailbox password alone opens the keys.
func TestTwoPasswordFakeSplitsThePasswords(t *testing.T) {
	t.Parallel()
	const mailbox = "the-mailbox-password-1"
	f := protontest.New(t, protontest.Options{TwoPassword: true, MailboxPassword: mailbox, LockedSalts: true})
	if f.MailboxPassword != mailbox {
		t.Fatalf("Fake.MailboxPassword = %q, want %q", f.MailboxPassword, mailbox)
	}
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	c, a, err := m.NewClientWithLogin(t.Context(), f.Username, []byte(f.Password))
	if err != nil {
		t.Fatalf("login with the login password: %v", err)
	}
	c.Close()
	if a.PasswordMode != gpa.TwoPasswordMode {
		t.Fatalf("PasswordMode = %d, want %d", a.PasswordMode, gpa.TwoPasswordMode)
	}
	user, addrs, salts := keyMaterial(t, f, api.Tokens{UID: a.UID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken})
	if opens(t, user, addrs, salts, f.Password) {
		t.Fatal("the login password opens the keys of a two-password account")
	}
	if !opens(t, user, addrs, salts, mailbox) {
		t.Fatal("the mailbox password does not open the keys")
	}
}

// TestTwoPasswordFakeDefaultsTheMailboxPassword: without one, a throwaway
// mailbox password distinct from the login password opens the keys, of every
// account of the fake.
func TestTwoPasswordFakeDefaultsTheMailboxPassword(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{TwoPassword: true})
	if f.MailboxPassword == "" || f.MailboxPassword == f.Password {
		t.Fatalf("Fake.MailboxPassword = %q, want a value other than the login password %q", f.MailboxPassword, f.Password)
	}
	user, addrs, salts := keyMaterial(t, f, f.Login(t, f.Username))
	if opens(t, user, addrs, salts, f.Password) || !opens(t, user, addrs, salts, f.MailboxPassword) {
		t.Fatal("the keys do not open with the default mailbox password alone")
	}
	f.AddUser("second2pw", "second2pw-pass")
	tok := f.Login(t, "second2pw")
	c := api.New(tok, api.Options{BaseURL: f.URL, Limiter: rate.NewLimiter(rate.Inf, 1)})
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	var out struct{ KeySalts gpa.Salts }
	if err := c.Get(t.Context(), "/core/v4/keys/salts", nil, &out); err != nil {
		t.Fatalf("salts: %v", err)
	}
	if opens(t, user, addrs, out.KeySalts, "second2pw-pass") || !opens(t, user, addrs, out.KeySalts, f.MailboxPassword) {
		t.Fatal("an added account's keys do not open with the mailbox password alone")
	}
}

// TestOnePasswordFakeUnchanged: without TwoPassword, a MailboxPassword is
// ignored and the login password opens the keys.
func TestOnePasswordFakeUnchanged(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{MailboxPassword: "ignored-mailbox-password"})
	if f.MailboxPassword != "" {
		t.Fatalf("Fake.MailboxPassword = %q on a single-password fake, want empty", f.MailboxPassword)
	}
	user, addrs, salts := keyMaterial(t, f, f.Login(t, f.Username))
	if !opens(t, user, addrs, salts, f.Password) {
		t.Fatal("the login password does not open the keys of a single-password account")
	}
	if opens(t, user, addrs, salts, "ignored-mailbox-password") {
		t.Fatal("the ignored mailbox password opens the keys")
	}
}
