package service

import (
	"path/filepath"
	"testing"
	"time"

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
