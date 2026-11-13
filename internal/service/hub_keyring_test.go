package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// fakeKeyring is an in-memory service.Keyring: absent stands for no Secret
// Service on the bus, locked for a locked default collection.
type fakeKeyring struct {
	mu     sync.Mutex
	absent bool
	locked bool
	items  map[string][]byte
	asked  int // calls of any method
	sets   int
}

func (k *fakeKeyring) setLocked(v bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.locked = v
}

func (k *fakeKeyring) counts() (asked, sets, items int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.asked, k.sets, len(k.items)
}

func (k *fakeKeyring) gate() error {
	k.asked++
	switch {
	case k.absent:
		return seal.ErrNoService
	case k.locked:
		return seal.ErrLocked
	}
	return nil
}

func attrKey(attrs map[string]string) string {
	b, _ := json.Marshal(attrs)
	return string(b)
}

func (k *fakeKeyring) Get(_ context.Context, attrs map[string]string) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.gate(); err != nil {
		return nil, err
	}
	v, ok := k.items[attrKey(attrs)]
	if !ok {
		return nil, seal.ErrNoKey
	}
	return bytes.Clone(v), nil
}

func (k *fakeKeyring) Set(_ context.Context, _ string, attrs map[string]string, secret []byte) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := k.gate(); err != nil {
		return err
	}
	if k.items == nil {
		k.items = map[string][]byte{}
	}
	k.sets++
	k.items[attrKey(attrs)] = bytes.Clone(secret)
	return nil
}

func (k *fakeKeyring) Locked(context.Context) (bool, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked++
	if k.absent {
		return false, seal.ErrNoService
	}
	return k.locked, nil
}

var _ service.Keyring = (*fakeKeyring)(nil)
var _ service.Keyring = seal.Keyring(nil) // the same method set both ways

func layoutSealer(t *testing.T, e *hubEnv) string {
	t.Helper()
	info, err := store.ReadLayout(e.dirs.State)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sealer
}

func accountStatus(t *testing.T, h *service.Hub, id string) service.Status {
	t.Helper()
	api, _, err := h.Account(id)
	if err != nil {
		t.Fatal(err)
	}
	st, err := api.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestFreshStatePicksTheUnlockedKeyring(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	e.keyring = &fakeKeyring{}
	h := e.open(t)
	if got := layoutSealer(t, e); got != "secret-service" {
		t.Fatalf("layout sealer = %q, want secret-service", got)
	}
	e.skip(t, h)
	id := e.mustLogin(t, h, userA).Account
	if _, err := os.Stat(e.cfg.Secrets.KeyFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the key file exists after a login (%v), want none: the key is in the keyring", err)
	}
	if _, sets, items := e.keyring.(*fakeKeyring).counts(); items != 1 || sets == 0 {
		t.Errorf("keyring holds %d items after %d sets, want the one sealing key", items, sets)
	}
	st := accountStatus(t, h, id)
	if st.Secrets != "sealed" || st.Sealer != "secret-service" || st.KeyFile != "" {
		t.Errorf("Status = secrets %q sealer %q key_file %q, want sealed, secret-service, none", st.Secrets, st.Sealer, st.KeyFile)
	}
}

func TestFreshStateWithoutKeyringUsesKeyFile(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t) // no Secret Service
	h := e.open(t)
	if got := layoutSealer(t, e); got != "keyfile" {
		t.Fatalf("layout sealer = %q, want keyfile", got)
	}
	e.skip(t, h)
	id := e.mustLogin(t, h, userA).Account
	if _, err := os.Stat(e.cfg.Secrets.KeyFile); err != nil {
		t.Errorf("no key file after a login: %v", err)
	}
	st := accountStatus(t, h, id)
	if st.Sealer != "keyfile" || st.KeyFile != e.cfg.Secrets.KeyFile {
		t.Errorf("Status sealer %q key_file %q, want keyfile and %q", st.Sealer, st.KeyFile, e.cfg.Secrets.KeyFile)
	}
}

func TestFreshStateWithLockedKeyringUsesKeyFile(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	e.keyring = &fakeKeyring{locked: true}
	e.open(t)
	if got := layoutSealer(t, e); got != "keyfile" {
		t.Errorf("layout sealer = %q, want keyfile: auto does not pick a locked keyring", got)
	}
}

func TestBackendKeyFileNeverAsksTheKeyring(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	e.cfg.Secrets.Backend = "keyfile"
	e.keyring = &fakeKeyring{}
	e.open(t)
	if got := layoutSealer(t, e); got != "keyfile" {
		t.Errorf("layout sealer = %q, want keyfile", got)
	}
	if asked, _, _ := e.keyring.(*fakeKeyring).counts(); asked != 0 {
		t.Errorf("the keyring was asked %d times, want 0", asked)
	}
}

func TestExistingStateKeepsItsKeyFile(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t) // M6-like: keyfile
	e.skip(t, h)
	a := e.mustLogin(t, h, userA).Account
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	kr := &fakeKeyring{}
	e.keyring = kr
	h2 := e.open(t)
	if got := layoutSealer(t, e); got != "keyfile" {
		t.Errorf("layout sealer = %q, want keyfile", got)
	}
	if asked, _, _ := kr.counts(); asked != 0 {
		t.Errorf("an existing layout probed the keyring %d times, want 0", asked)
	}
	e.skip(t, h2)
	b := e.mustLogin(t, h2, userB).Account
	for _, id := range []string{a, b} {
		if st := accountStatus(t, h2, id); st.Secrets != "sealed" || st.Sealer != "keyfile" {
			t.Errorf("account %s: secrets %q sealer %q, want sealed, keyfile", id, st.Secrets, st.Sealer)
		}
	}
	if _, sets, items := kr.counts(); sets != 0 || items != 0 {
		t.Errorf("the keyring got %d sets and holds %d items, want none", sets, items)
	}
}

func TestBackendSecretServiceWithoutKeyringIsMustAct(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	e.cfg.Secrets.Backend = "secret-service"
	_, err := service.OpenHub(e.options())
	if got := exitcode.From(err); got != exitcode.MustAct {
		t.Fatalf("OpenHub = %v (exit %d), want exit %d", err, got, exitcode.MustAct)
	}
	if !errors.Is(err, seal.ErrNoService) && !bytes.Contains([]byte(err.Error()), []byte("Secret Service")) &&
		!bytes.Contains([]byte(err.Error()), []byte("keyring")) {
		t.Errorf("error %q does not name the keyring", err)
	}
	if _, serr := os.Stat(filepath.Join(e.dirs.State, "layout.json")); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("a refused creation left layout.json (%v)", serr)
	}
}

func TestUnknownSealerIsMustActUpgrade(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dirs.State, "accounts"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dirs.State, "layout.json"), []byte(`{"version":2,"sealer":"vault"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := service.OpenHub(e.options())
	if got := exitcode.From(err); got != exitcode.MustAct {
		t.Fatalf("OpenHub = %v (exit %d), want exit %d", err, got, exitcode.MustAct)
	}
	if h := exitcode.HintOf(err); h != "upgrade pcb" {
		t.Errorf("hint = %q, want %q", h, "upgrade pcb")
	}
}

// A locked desktop keyring is waited for, not parked: the account waits,
// Status and the alerts say why, Proton is not called, and once it is
// unlocked the account serves with nothing re-sealed.
func TestLockedKeyringWaitsAndRecovers(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	kr := &fakeKeyring{}
	e.keyring = kr
	waiting, retry := make(chan struct{}, 1), make(chan struct{})
	e.sleep = func(ctx context.Context, d time.Duration) error {
		if d == service.KeyringRetry {
			select {
			case waiting <- struct{}{}:
			default:
			}
			select {
			case <-retry:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return blockSleep(ctx, d)
	}
	h := e.open(t)
	e.skip(t, h)
	a := e.mustLogin(t, h, userA).Account
	before, err := store.LoadState(store.AccountDir(e.dirs.State, a))
	if err != nil {
		t.Fatal(err)
	}
	_, setsBefore, _ := kr.counts()
	authBefore := e.f.Calls("", "/auth/v4")

	kr.setLocked(true)
	r := newServeRig(h)
	done, _ := r.serve(t, r.options(true))
	select {
	case <-waiting:
	case <-time.After(waitLimit):
		t.Fatal("the loop never waited for the keyring")
	}
	r.waitFor(t, "a waiting", func() bool { return r.last(a) == service.ServeWaitingForLogin })
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Fatalf("a.Stopped = %+v while the keyring is locked, want waiting (not parked)", info.Stopped)
	}
	running(t, done)
	st := accountStatus(t, h, a)
	if st.Secrets != "locked" {
		t.Errorf("Status.Secrets = %q, want locked", st.Secrets)
	}
	var alert *ctl.Alert
	for _, al := range ctl.Alerts(ctl.AlertInput{Account: a, Login: st, Now: e.clk.Now()}) {
		if al.Code == ctl.CodeKeyringLocked {
			alert = &al
		}
	}
	if alert == nil {
		t.Errorf("no %s alert while the keyring is locked", ctl.CodeKeyringLocked)
	}
	if n := e.f.Calls("", "/auth/v4") - authBefore; n != 0 {
		t.Errorf("Proton saw %d /auth/v4 calls while the keyring is locked, want 0", n)
	}

	kr.setLocked(false)
	e.clk.advance(service.KeyringRetry)
	select {
	case retry <- struct{}{}:
	case <-time.After(waitLimit):
		t.Fatal("the retry wait ended before it was released")
	}
	r.waitPassed(t, a, 0)
	after, err := store.LoadState(store.AccountDir(e.dirs.State, a))
	if err != nil {
		t.Fatal(err)
	}
	if after.SecretsGeneration != before.SecretsGeneration {
		t.Errorf("secrets generation %d -> %d: something was re-sealed", before.SecretsGeneration, after.SecretsGeneration)
	}
	if _, sets, _ := kr.counts(); sets != setsBefore {
		t.Errorf("the keyring got %d more sets, want none", sets-setsBefore)
	}
	if st := accountStatus(t, h, a); st.Secrets != "sealed" {
		t.Errorf("Status.Secrets = %q once unlocked, want sealed", st.Secrets)
	}
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Errorf("a.Stopped = %+v once served, want nil", info.Stopped)
	}
}

// A keyring that locks while an account serves is waited for too: the next
// pass fails, the account waits (no park, no Restart), and it serves again
// 30 s later without a new login.
func TestKeyringLockedWhileServingWaitsAndRecovers(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	kr := &fakeKeyring{}
	e.keyring = kr
	passSleep, nextPass := make(chan struct{}, 8), make(chan struct{})
	waiting, retry := make(chan struct{}, 1), make(chan struct{})
	var retrying atomic.Bool
	e.sleep = func(ctx context.Context, d time.Duration) error {
		gate, signal := nextPass, passSleep
		if retrying.Load() { // the pass interval may equal the retry: tell them apart by phase
			if d != service.KeyringRetry {
				t.Errorf("sleep of %v while waiting for the keyring", d)
			}
			gate, signal = retry, waiting
		}
		select {
		case signal <- struct{}{}:
		default:
		}
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := e.open(t)
	e.skip(t, h)
	a := e.mustLogin(t, h, userA).Account
	r := newServeRig(h)
	done, _ := r.serve(t, r.options(true))
	r.waitPassed(t, a, 0)
	select {
	case <-passSleep: // the loop now sleeps until the next pass
	case <-time.After(waitLimit):
		t.Fatal("the loop never slept between passes")
	}
	authBefore := e.f.Calls("", "/auth/v4")
	kr.setLocked(true)
	retrying.Store(true)
	nextPass <- struct{}{}
	select {
	case <-waiting:
	case <-time.After(waitLimit):
		t.Fatal("the loop never waited for the keyring after a failed pass")
	}
	r.waitFor(t, "a waiting", func() bool { return r.last(a) == service.ServeWaitingForLogin })
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Fatalf("a.Stopped = %+v, want waiting (not parked)", info.Stopped)
	}
	running(t, done)
	st := accountStatus(t, h, a)
	if st.Secrets != "locked" {
		t.Errorf("Status.Secrets = %q, want locked", st.Secrets)
	}
	var found bool
	for _, al := range ctl.Alerts(ctl.AlertInput{Account: a, Login: st, Now: e.clk.Now()}) {
		found = found || al.Code == ctl.CodeKeyringLocked
	}
	if !found {
		t.Errorf("no %s alert", ctl.CodeKeyringLocked)
	}
	kr.setLocked(false)
	passes := r.passCount(a)
	retrying.Store(false)
	retry <- struct{}{}
	r.waitPassed(t, a, passes)
	if n := e.f.Calls("", "/auth/v4") - authBefore; n != 0 {
		t.Errorf("Proton saw %d /auth/v4 calls across the lock, want 0", n)
	}
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Errorf("a.Stopped = %+v once served, want nil", info.Stopped)
	}
}

// What a lost or foreign key says depends on where the key lives.
func TestSecretServiceStateNamesTheKeyring(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	kr := &fakeKeyring{}
	e.keyring = kr
	h := e.open(t)
	e.skip(t, h)
	a := e.mustLogin(t, h, userA).Account
	api, _, err := h.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		tamper func()
		msg    string
	}{
		{"key replaced", func() {
			kr.mu.Lock()
			for k := range kr.items {
				kr.items[k] = bytes.Repeat([]byte{7}, 32)
			}
			kr.mu.Unlock()
		}, "the stored secrets cannot be opened with the key in the desktop keyring"},
		{"key gone", func() { kr.mu.Lock(); kr.items = nil; kr.mu.Unlock() }, "the sealing key is not in the desktop keyring"},
	} {
		tc.tamper()
		if _, err := api.Sync(t.Context(), service.SyncRequest{}); err == nil {
			t.Fatalf("%s: Sync succeeded", tc.name)
		} else if got := exitcode.From(err); got != exitcode.MustAct || err.Error() != tc.msg || exitcode.HintOf(err) != "pcb logout then pcb login" {
			t.Errorf("%s: %v (exit %d, hint %q), want exit 10, %q, hint %q", tc.name, err, got, exitcode.HintOf(err), tc.msg, "pcb logout then pcb login")
		}
	}
}
