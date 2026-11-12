package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
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

func TestSaveStateWaitsForTheLock(t *testing.T) {
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
	go func() { done <- store.SaveState(dir, store.State{Phase: store.PhaseLoggedOut}) }()
	select {
	case <-busy:
	case err := <-done:
		unlock()
		t.Fatalf("SaveState returned (%v) while state.lock was held", err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "state.json")); !bytes.Equal(now, before) {
		unlock()
		t.Fatalf("state.json written while state.lock was held: %s", now)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st, err := store.LoadState(dir); err != nil || st.Phase != store.PhaseLoggedOut {
		t.Fatalf("after release: %+v, %v", st, err)
	}
}

// A lock held past the timeout fails both writers with ErrStateLock and
// DeadlineExceeded, and nothing is written.
func TestStateLockTimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file locks are Unix only")
	}
	defer store.SetStateLockTimeout(50 * time.Millisecond)()
	dir := filepath.Join(t.TempDir(), "state")
	if err := store.SaveState(dir, store.State{Phase: store.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	unlock, ok, err := filelock.TryLock(filepath.Join(dir, "state.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	called := false
	err = store.UpdateState(dir, func(s *store.State) error { called = true; s.Phase = store.PhaseNeedsLogin; return nil })
	if !errors.Is(err, store.ErrStateLock) || !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("UpdateState: err = %v, fn called = %v", err, called)
	}
	err = store.SaveState(dir, store.State{Phase: store.PhaseLoggedOut})
	if !errors.Is(err, store.ErrStateLock) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SaveState: err = %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "state.json")); !bytes.Equal(before, after) {
		t.Fatalf("state.json written without the lock: %s", after)
	}
}

// Within one process the wait ends with the caller's context, or the
// timeout, rather than queueing behind the holder.
func TestStateLockWaitHonoursContext(t *testing.T) {
	defer store.SetStateLockTimeout(50 * time.Millisecond)()
	dir := filepath.Join(t.TempDir(), "state")
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- store.UpdateState(dir, func(*store.State) error {
			close(entered)
			<-release
			return store.ErrStateUnchanged
		})
	}()
	<-entered
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	results := make(chan error, 2)
	go func() {
		results <- store.UpdateStateContext(cancelled, dir, func(*store.State) error { return errors.New("fn ran") })
	}()
	go func() {
		results <- store.SaveStateContext(context.Background(), dir, store.State{Phase: store.PhaseReady})
	}()
	var errs []error
	for range 2 {
		select {
		case err := <-results:
			errs = append(errs, err)
		case <-time.After(5 * time.Second): // a safety net, not a synchronisation
			close(release)
			t.Fatal("a writer kept waiting behind the holder past its context and timeout")
		}
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	var canceled, timedOut bool
	for _, err := range errs {
		if !errors.Is(err, store.ErrStateLock) {
			t.Errorf("err = %v, want ErrStateLock", err)
		}
		canceled = canceled || errors.Is(err, context.Canceled)
		timedOut = timedOut || errors.Is(err, context.DeadlineExceeded)
	}
	if !canceled || !timedOut {
		t.Errorf("errs = %v, want one Canceled and one DeadlineExceeded", errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state.json written: %v", err)
	}
}

func TestGenerationOutOfRangeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		gen uint64
		ok  bool
	}{{store.MaxSecretsGeneration, true}, {store.MaxSecretsGeneration + 1, false}, {math.MaxUint64, false}} {
		raw := fmt.Sprintf(`{"phase":"logged-out","secrets_generation":%d}`, tc.gen)
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := store.LoadState(dir)
		switch {
		case tc.ok && (err != nil || st.SecretsGeneration != tc.gen):
			t.Errorf("generation %d: %+v, %v", tc.gen, st, err)
		case !tc.ok && !errors.Is(err, store.ErrGenerationOutOfRange):
			t.Errorf("generation %d: err = %v, want ErrGenerationOutOfRange", tc.gen, err)
		}
	}
	// UpdateState never hands it to fn, so the next generation cannot wrap.
	called := false
	if err := store.UpdateState(dir, func(*store.State) error { called = true; return nil }); !errors.Is(err, store.ErrGenerationOutOfRange) || called {
		t.Fatalf("UpdateState: err = %v, fn called = %v", err, called)
	}
}

// A state.json with a field this build does not know was written by a newer
// pcb (state-I2): it stays refused, as its own error, never as a malformed
// file; the other refusals are not it.
func TestStateWithUnknownFieldsIsNewer(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		newer   bool
	}{
		"unknown field":         {`{"phase":"ready","added_later":1}`, true},
		"unknown nested field":  {`{"phase":"ready","captcha":{"url":"u","token":"t","kind":"x"}}`, true},
		"garbage":               {"{not json", false},
		"unknown phase":         {`{"phase":"banana"}`, false},
		"unknown field + error": {`{"phase":"ready","added_later":1,"unattended":"yes"}`, false},
		"trailing data":         {`{"phase":"ready","x":1} {"phase":"ready"}`, false},
	} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := store.LoadState(dir)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if got := errors.Is(err, store.ErrNewerState); got != tc.newer {
			t.Errorf("%s: errors.Is(ErrNewerState) = %v, want %v (%v)", name, got, tc.newer, err)
		}
	}
}
