package store_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

func TestStateMissingIsLoggedOut(t *testing.T) {
	got, err := store.LoadState(t.TempDir())
	if err != nil || got.Phase != store.PhaseLoggedOut {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestStateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	at := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	want := store.State{
		AccountID: "uid1", Phase: store.PhaseNeedsCaptcha, Reason: "captcha",
		Captcha:    &store.Captcha{URL: "https://verify.proton.me/?x", Token: "tok", At: at},
		Unattended: true, LastLogin: at,
	}
	if err := store.SaveState(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadState(dir)
	if err != nil || got.Captcha == nil || got.Captcha.Token != "tok" || !got.LastLogin.Equal(at) ||
		!got.Unattended || got.Phase != want.Phase || got.AccountID != "uid1" {
		t.Fatalf("got %+v, %v", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	for _, k := range []string{`"account_id"`, `"phase"`, `"last_login"`, `"unattended"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("missing %s in %s", k, raw)
		}
	}
	if strings.Contains(string(raw), "last_relogin") || strings.Contains(string(raw), "0001-01-01") {
		t.Errorf("zero time written: %s", raw)
	}
}

func TestStateMalformedFailsClosed(t *testing.T) {
	for name, content := range map[string]string{
		"garbage":       "{not json",
		"unknown phase": `{"phase":"banana"}`,
		"empty phase":   `{}`,
		"trailing data": `{"phase":"ready"} {"phase":"ready"}`,
		"unknown field": `{"phase":"ready","password":"x"}`,
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadState(dir); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestStateMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{filepath.Join(dir, "state.json"): 0o600, dir: 0o700} {
		if info, _ := os.Stat(path); info.Mode().Perm() != want {
			t.Errorf("%s mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
}
