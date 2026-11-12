package store_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
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

func TestUpdateStateWaitsForTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file locks are Unix only")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady, AccountID: "uid1"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := filelock.TryLock(filepath.Join(dir, "state.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	busy := make(chan struct{}, 1)
	defer store.OnStateLockBusy(func() { busy <- struct{}{} })()
	done := make(chan error, 1)
	go func() {
		done <- store.UpdateState(dir, func(s *store.State) error {
			s.Phase, s.Reason = store.PhaseNeedsLogin, "session ended"
			return nil
		})
	}()
	select {
	case <-busy:
	case err := <-done:
		unlock()
		t.Fatalf("UpdateState returned (%v) while state.lock was held", err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "state.json")); !bytes.Equal(now, before) {
		unlock()
		t.Fatalf("state.json written while state.lock was held: %s", now)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st, err := store.LoadState(dir); err != nil || st.Phase != store.PhaseNeedsLogin || st.AccountID != "uid1" {
		t.Fatalf("after release: %+v, %v", st, err)
	}
}

func TestUpdateStateUnchangedWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	err := store.UpdateState(dir, func(s *store.State) error {
		s.Phase = store.PhaseNeedsLogin
		return store.ErrStateUnchanged
	})
	after, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("err = %v; state.json %s, want %s", err, after, before)
	}
	boom := errors.New("boom")
	if err := store.UpdateState(dir, func(s *store.State) error { s.Phase = store.PhaseNeedsLogin; return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "state.json")); !bytes.Equal(before, after) {
		t.Fatalf("state.json written after fn failed: %s", after)
	}
	// A malformed state.json is never handed to fn.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := store.UpdateState(dir, func(*store.State) error { called = true; return nil }); err == nil || called {
		t.Fatalf("malformed: err = %v, fn called = %v", err, called)
	}
}

func TestSaveStateKeepsTheGeneration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady, SecretsGeneration: 4}); err != nil {
		t.Fatal(err)
	}
	// A writer built its State without the generation (a login, a logout).
	if err := store.SaveState(dir, store.State{Phase: store.PhaseLoggedOut}); err != nil {
		t.Fatal(err)
	}
	if st, err := store.LoadState(dir); err != nil || st.SecretsGeneration != 4 || st.Phase != store.PhaseLoggedOut {
		t.Fatalf("got %+v, %v; want logged-out, generation 4", st, err)
	}
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady, SecretsGeneration: 6}); err != nil {
		t.Fatal(err)
	}
	if st, _ := store.LoadState(dir); st.SecretsGeneration != 6 {
		t.Fatalf("generation %d, want 6", st.SecretsGeneration)
	}
	// Through the sealed store: a login's SaveState after the secrets moved on.
	root := t.TempDir()
	f := fixture{state: filepath.Join(root, "state"), key: filepath.Join(root, "k")}
	f.secrets = store.NewSecrets(f.state, &seal.KeyFile{Path: f.key})
	put(t, f.secrets, fullAccount("userA"))
	put(t, f.secrets, fullAccount("userA"))
	if err := store.SaveState(f.state, store.State{Phase: store.PhaseReady, AccountID: "userA"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.secrets.Load(); err != nil {
		t.Fatalf("Load after SaveState: %v", err)
	}
	if m := mirrorOf(t, f); m != 2 {
		t.Fatalf("mirror %d, want 2", m)
	}
}
