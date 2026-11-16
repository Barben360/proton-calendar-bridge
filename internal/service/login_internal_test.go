package service

import (
	"path/filepath"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// M5-Rpre4: on expiry the password the pending login held is zeroed in
// place, not merely dropped.
func TestPendingPasswordIsZeroedOnExpiry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 11, 10, 0, 7, 0, time.UTC)
	f := protontest.New(t, protontest.Options{TwoFA: true, Now: func() time.Time { return now }})
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(dirs)
	cfg.Proton.BaseURL = f.URL
	var fire func()
	l, err := NewLocal(Options{Dirs: dirs, Config: cfg, Now: func() time.Time { return now },
		AfterFunc: func(_ time.Duration, fn func()) func() bool { fire = fn; return func() bool { return true } }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	if res, err := l.Login(t.Context(), LoginRequest{Username: f.Username, Password: f.Password}); err != nil || res.State != StateNeedsTOTP {
		t.Fatalf("Login = %+v, %v", res, err)
	}
	l.mu.Lock()
	pw := l.pending.password
	l.mu.Unlock()
	if string(pw) != f.Password {
		t.Fatalf("the pending login holds %d bytes, not the password", len(pw))
	}
	if fire == nil {
		t.Fatal("no expiry timer was armed")
	}
	fire()
	for i, b := range pw {
		if b != 0 {
			t.Fatalf("byte %d of the password is still set after the expiry", i)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pending != nil || l.waiting.Load() != nil {
		t.Error("the expired login is still pending")
	}
}

// release drops what a held login fetched for the derivation, with its
// passwords.
func TestReleaseDropsFetchedKeys(t *testing.T) {
	t.Parallel()
	pl := &pendingLogin{
		password: []byte("pw"), mailbox: []byte("mb"),
		salts: gpa.Salts{{ID: "k", KeySalt: "s"}},
		user:  gpa.User{ID: "u", Name: "n"},
		addrs: []gpa.Address{{ID: "a"}},
	}
	pl.release()
	if pl.salts != nil || pl.user.ID != "" || pl.user.Name != "" || pl.addrs != nil {
		t.Errorf("after release: salts %v, user %q, addrs %v; want all dropped", pl.salts, pl.user.ID, pl.addrs)
	}
}

// A two-password login held for its mailbox password no longer holds the
// login password (nothing needs it once the keys are fetched), unless an
// unattended relogin will seal it.
func TestMailboxHoldDropsTheLoginPassword(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		unattended bool
	}{{"attended", false}, {"unattended", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := protontest.New(t, protontest.Options{TwoPassword: true})
			root := t.TempDir()
			dirs := paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
			cfg := config.Defaults(dirs)
			cfg.Proton.BaseURL = f.URL
			l, err := NewLocal(Options{Dirs: dirs, Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = l.Close() })
			res, err := l.Login(t.Context(), LoginRequest{Username: f.Username, Password: f.Password, UnattendedRelogin: tc.unattended})
			if err != nil || res.State != StateNeedsMailboxPassword {
				t.Fatalf("Login = %+v, %v; want needs-mailbox-password", res, err)
			}
			l.mu.Lock()
			held := string(l.pending.password)
			l.mu.Unlock()
			if tc.unattended != (held == f.Password) {
				t.Errorf("the held login keeps its password = %v; want %v", held == f.Password, tc.unattended)
			}
			if !tc.unattended && held != string(make([]byte, len(f.Password))) {
				t.Errorf("the login password is not zeroed in place: %q", held)
			}
			if res, err = l.SubmitMailboxPassword(t.Context(), f.MailboxPassword); err != nil || res.State != StateReady {
				t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
			}
			accs, err := l.secrets.Load()
			if err != nil {
				t.Fatal(err)
			}
			a := accs[f.UserID(f.Username)]
			if tc.unattended != (a.Relogin != nil && a.Relogin.Password == f.Password) {
				t.Errorf("sealed Relogin = %+v; want the login password sealed = %v", a.Relogin, tc.unattended)
			}
		})
	}
}
