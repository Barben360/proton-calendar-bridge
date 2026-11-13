package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	mu      sync.Mutex
	absent  bool
	locked  bool
	items   map[string][]byte
	failSet error // Set's error when it is not locked or absent
	asked   int   // calls of any method
	sets    int
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
	if k.failSet != nil {
		return k.failSet
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
	if !strings.Contains(err.Error(), seal.ErrNoService.Error()) {
		t.Errorf("error %q does not name the probe's error %q", err, seal.ErrNoService)
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

func serveUntilWaiting(t *testing.T, h *service.Hub, a string, waitForLogin bool, waiting chan struct{}) (*serveRig, chan error) {
	t.Helper()
	r := newServeRig(h)
	done, _ := r.serve(t, r.options(waitForLogin))
	select {
	case <-waiting:
	case <-time.After(waitLimit):
		t.Fatal("the loop never waited for the keyring")
	}
	r.waitFor(t, "a waiting", func() bool { return r.last(a) == service.ServeWaitingForLogin })
	return r, done
}

// Without WaitForLogin (pcb serve's own exit rule) a locked keyring is no
// reason to refuse to start or to exit: Serve keeps running, and the
// account serves once the keyring is unlocked.
func TestLockedKeyringDoesNotEndServeWithoutWaitForLogin(t *testing.T) {
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
	kr.setLocked(true)
	r, done := serveUntilWaiting(t, h, a, false, waiting)
	running(t, done) // neither anyLoggedIn at the start nor allStopped afterwards ended it
	kr.setLocked(false)
	retry <- struct{}{}
	r.waitPassed(t, a, 0)
	running(t, done)
}

// pcb sync --watch keeps retrying with its back-off when the keyring locks
// mid-watch: it neither ends nor claims that pcb retries every 30 s.
func TestWatchSyncSurvivesALockedKeyring(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	kr := &fakeKeyring{}
	e.keyring = kr
	sleeping, release := make(chan struct{}, 8), make(chan struct{})
	e.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case sleeping <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h := e.open(t)
	e.skip(t, h)
	a := e.mustLogin(t, h, userA).Account
	api, _, err := h.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	passes := make(chan service.WatchPass, 8)
	done := make(chan error, 1)
	go func() {
		done <- api.WatchSync(ctx, service.SyncRequest{}, 0, func(p service.WatchPass) { passes <- p })
	}()
	wait := func(c chan struct{}, what string) {
		t.Helper()
		select {
		case <-c:
		case err := <-done:
			t.Fatalf("WatchSync returned %v before %s", err, what)
		case <-time.After(waitLimit):
			t.Fatal("timeout: " + what)
		}
	}
	wait(sleeping, "its first sleep")
	if p := <-passes; p.Err != nil {
		t.Fatalf("first pass: %v", p.Err)
	}
	kr.setLocked(true)
	release <- struct{}{} // the next pass meets the locked keyring
	wait(sleeping, "backing off after the locked pass")
	if p := <-passes; p.Err == nil {
		t.Fatal("the pass under a locked keyring succeeded")
	}
	select {
	case err := <-done:
		t.Fatalf("WatchSync returned %v on a locked keyring, want it to keep retrying", err)
	default:
	}
	kr.setLocked(false)
	release <- struct{}{}
	if p := <-passes; p.Err != nil {
		t.Errorf("pass after the unlock: %v", p.Err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("WatchSync after cancel = %v, want nil", err)
	}
}

// The wording of the other failures of a secret-service state names the
// desktop keyring, not a key file.
func TestSecretServiceStateWordsNameTheKeyring(t *testing.T) {
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
	noKeyFile := func(what string, err error) {
		t.Helper()
		if strings.Contains(err.Error(), "key file") || strings.Contains(exitcode.HintOf(err), "key file") {
			t.Errorf("%s: %q, hint %q name a key file", what, err, exitcode.HintOf(err))
		}
	}
	// A client's password that cannot be sealed.
	kr.mu.Lock()
	kr.items = nil // sealing then has to create the key, which the keyring refuses
	kr.failSet = errors.New("the keyring refused")
	kr.mu.Unlock()
	_, err = api.AddClient(t.Context(), "phone")
	if err == nil || !strings.Contains(err.Error(), "sealed with the desktop keyring") || exitcode.HintOf(err) != "check that the desktop keyring is running and unlocked (pcb doctor)" {
		t.Errorf("AddClient = %v, hint %q", err, exitcode.HintOf(err))
	}
	kr.mu.Lock()
	kr.failSet = nil
	kr.mu.Unlock()
	if _, err := api.AddClient(t.Context(), "laptop"); err != nil {
		t.Fatal(err)
	}
	// Its copy no longer opens: the key was replaced.
	kr.mu.Lock()
	for k := range kr.items {
		kr.items[k] = bytes.Repeat([]byte{9}, 32)
	}
	kr.mu.Unlock()
	_, err = api.ShowClient(t.Context(), "laptop")
	if err == nil || !strings.Contains(err.Error(), "does not open with the desktop keyring") ||
		!strings.Contains(exitcode.HintOf(err), "pcb client rotate") {
		t.Errorf("ShowClient (key replaced) = %v, hint %q", err, exitcode.HintOf(err))
	} else {
		noKeyFile("show, key replaced", err)
	}
	st := accountStatus(t, h, a)
	if st.Secrets != "unreadable" || !strings.Contains(st.Reason, "desktop keyring") || strings.Contains(st.Reason, "key file") {
		t.Errorf("Status = %q / %q, want unreadable naming the desktop keyring", st.Secrets, st.Reason)
	}
	// The key is gone altogether.
	kr.mu.Lock()
	kr.items = nil
	kr.mu.Unlock()
	_, err = api.ShowClient(t.Context(), "laptop")
	if err == nil || !strings.Contains(err.Error(), "the desktop keyring has no key for it") ||
		!strings.Contains(exitcode.HintOf(err), "run pcb logout, then pcb login") {
		t.Errorf("ShowClient (key gone) = %v, hint %q", err, exitcode.HintOf(err))
	} else {
		noKeyFile("show, key gone", err)
	}
}

// Ruling R15: on a keyring state where no Secret Service answers (an SSH
// session, a service outside the desktop session), the error names the
// keyring (exit 20) and never sends the person to pcb logout; Status keeps
// the phase and says keyring-unavailable, with its own alert and no
// needs-login (a logout would discard a good session).
func TestKeyringUnavailableIsNotANeedsLogin(t *testing.T) {
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
	kr.mu.Lock()
	kr.absent = true
	kr.mu.Unlock()

	_, err = api.Sync(t.Context(), service.SyncRequest{})
	if got := exitcode.From(err); got != exitcode.Blocked || err.Error() != "the desktop keyring is not available" {
		t.Errorf("Sync = %v (exit %d), want exit 20 %q", err, got, "the desktop keyring is not available")
	}
	hint := exitcode.HintOf(err)
	if !strings.Contains(hint, "desktop session") || strings.Contains(hint, "logout") || strings.Contains(hint, "permissions") {
		t.Errorf("hint = %q, want it to send the person into the desktop session, never to logout or permissions", hint)
	}

	st := accountStatus(t, h, a)
	if st.Secrets != "keyring-unavailable" || st.Phase != "ready" {
		t.Errorf("Status = secrets %q phase %q, want keyring-unavailable and the phase kept (ready)", st.Secrets, st.Phase)
	}
	var codes []string
	for _, al := range ctl.Alerts(ctl.AlertInput{Account: a, Login: st, Now: e.clk.Now()}) {
		codes = append(codes, al.Code)
	}
	if !slices.Contains(codes, ctl.CodeKeyringUnavailable) || slices.Contains(codes, ctl.CodeNeedsLogin) {
		t.Errorf("alerts = %v, want %s and no %s", codes, ctl.CodeKeyringUnavailable, ctl.CodeNeedsLogin)
	}
}

// Ruling R16: on a keyring state, a login whose seal could not succeed (the
// keyring locked, or no Secret Service) is refused before SRP: Proton sees
// no auth call, the login guard's slot is not spent, and the add leaves no
// account behind.
func TestLoginChecksTheKeyringBeforeSRP(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		set  func(k *fakeKeyring)
		code exitcode.Code
		msg  string
	}{
		{"locked", func(k *fakeKeyring) { k.locked = true }, exitcode.Blocked, "the desktop keyring is locked"},
		{"no service", func(k *fakeKeyring) { k.absent = true }, exitcode.Blocked, "the desktop keyring is not available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newHubEnv(t)
			kr := &fakeKeyring{}
			e.keyring = kr
			h := e.open(t) // a new state: the keyring is chosen
			if got := layoutSealer(t, e); got != "secret-service" {
				t.Fatalf("layout sealer = %q, want secret-service", got)
			}
			kr.mu.Lock()
			tc.set(kr)
			kr.mu.Unlock()
			_, err := e.login(t, h, userA)
			if got := exitcode.From(err); got != tc.code || err.Error() != tc.msg {
				t.Errorf("Login = %v (exit %d), want exit %d %q", err, got, tc.code, tc.msg)
			}
			if n := e.f.Calls("", "/auth/v4"); n != 0 {
				t.Errorf("Proton saw %d /auth/v4 calls, want 0: the login must stop before SRP", n)
			}
			if n := len(h.Accounts()); n != 0 {
				t.Errorf("%d accounts after the refused add, want 0", n)
			}
			// The keyring back, a login at once is not refused by the guard:
			// the refused one spent no slot.
			kr.mu.Lock()
			kr.locked, kr.absent = false, false
			kr.mu.Unlock()
			if _, err := h.Login(t.Context(), service.LoginRequest{Username: userA, Password: e.password(userA)}); err != nil {
				t.Errorf("a login right after the refused one: %v", err)
			}
		})
	}
}
