package service_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// captchaURL is the verification URL of the CAPTCHAs relogins meet here.
const captchaURL = "https://verify.proton.me/?methods=captcha&token=captcha-a"

// blockSleep is a Sleep that lasts until its context ends: a serve loop
// runs one pass when it starts, then waits.
func blockSleep(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// newServeEnv is a Hub env whose users a and b own a calendar each, and
// whose serve loops run one pass when they start (blockSleep).
func newServeEnv(t *testing.T) *hubEnv {
	t.Helper()
	e := newHubEnv(t)
	e.f.AddCalendar(userA, protontest.Calendar{Name: "A"})
	e.f.AddCalendar(userB, protontest.Calendar{Name: "B"})
	e.sleep = blockSleep
	return e
}

// serveRig runs a Hub's Serve and records what its callbacks get, by
// account.
type serveRig struct {
	h *service.Hub

	mu     sync.Mutex
	passes map[string][]service.WatchPass
	states map[string][]service.ServeState
	poke   chan struct{} // a record was added (capacity 1)
}

func newServeRig(h *service.Hub) *serveRig {
	return &serveRig{h: h, passes: map[string][]service.WatchPass{}, states: map[string][]service.ServeState{}, poke: make(chan struct{}, 1)}
}

func (r *serveRig) note(f func()) {
	r.mu.Lock()
	f()
	r.mu.Unlock()
	select {
	case r.poke <- struct{}{}:
	default:
	}
}

// options are Serve's options with the rig's callbacks.
func (r *serveRig) options(waitForLogin bool) service.HubServeOptions {
	return service.HubServeOptions{
		ServeOptions: service.ServeOptions{WaitForLogin: waitForLogin, LoginPoll: time.Hour},
		OnState: func(id string, s service.ServeState) {
			r.note(func() { r.states[id] = append(r.states[id], s) })
		},
		OnPass: func(id string, p service.WatchPass) {
			r.note(func() { r.passes[id] = append(r.passes[id], p) })
		},
	}
}

// serve runs Serve with o in the background; stop ends it and returns what
// it returned.
func (r *serveRig) serve(t *testing.T, o service.HubServeOptions) (done chan error, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done = make(chan error, 1)
	go func() { done <- r.h.Serve(ctx, o) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(waitLimit):
				t.Error("Hub.Serve did not return after its context ended")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return done, stop
}

// waitFor waits until cond holds, checked at each record.
func (r *serveRig) waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(waitLimit)
	for {
		r.mu.Lock()
		ok := cond()
		r.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-r.poke:
		case <-deadline:
			t.Fatalf("never: %s", what)
		}
	}
}

// okPasses counts id's passes that got through; r.mu held.
func (r *serveRig) okPasses(id string) int {
	n := 0
	for _, p := range r.passes[id] {
		if p.Err == nil {
			n++
		}
	}
	return n
}

// last is id's last reported state, "" if none; r.mu held.
func (r *serveRig) last(id string) service.ServeState {
	s := r.states[id]
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

func (r *serveRig) waitPassed(t *testing.T, id string, after int) {
	t.Helper()
	r.waitFor(t, fmt.Sprintf("a pass of %s that gets through (more than %d)", id, after), func() bool { return r.okPasses(id) > after })
}

func (r *serveRig) passCount(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.okPasses(id)
}

func (r *serveRig) waitStopped(t *testing.T, id string) service.AccountInfo {
	t.Helper()
	r.waitFor(t, id+" stopped", func() bool { return r.last(id) == service.ServeStopped })
	info := accountInfo(t, r.h, id)
	if info.Stopped == nil {
		t.Fatalf("%s reported stopped, but its AccountInfo.Stopped is nil", id)
	}
	return info
}

func accountInfo(t *testing.T, h *service.Hub, id string) service.AccountInfo {
	t.Helper()
	for _, a := range h.Accounts() {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("no account %s", id)
	return service.AccountInfo{}
}

// loginBoth logs a and b in on h and returns their local IDs.
func (e *hubEnv) loginBoth(t *testing.T, h *service.Hub) (a, b string) {
	t.Helper()
	e.skip(t, h)
	return e.mustLogin(t, h, userA).Account, e.mustLogin(t, h, userB).Account
}

// damageCache replaces the account's pcb.db with bytes that are no
// database.
func (e *hubEnv) damageCache(t *testing.T, id string) string {
	t.Helper()
	path := cache.Path(store.AccountDir(e.dirs.State, id))
	if err := os.WriteFile(path, []byte("not a database, not at all, not even close........"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// lockFree reports whether path's file lock can be taken now (then it is
// released at once).
func lockFree(t *testing.T, path string) bool {
	t.Helper()
	unlock, ok, err := filelock.TryLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		unlock()
	}
	return ok
}

func syncLock(e *hubEnv, id string) string {
	return filepath.Join(store.AccountDir(e.dirs.State, id), "sync.lock")
}

func health(t *testing.T, h *service.Hub, id string) service.Health {
	t.Helper()
	hl, err := h.Health(t.Context(), id)
	if err != nil {
		t.Fatalf("Health(%s): %v", id, err)
	}
	return hl
}

func TestServeRunsEveryAccount(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	a, b := e.loginBoth(t, h)
	r := newServeRig(h)
	ready := make(chan struct{})
	o := r.options(false)
	o.Ready = func() { close(ready) }
	r.serve(t, o)
	r.waitPassed(t, a, 0)
	r.waitPassed(t, b, 0)
	select {
	case <-ready:
	default:
		t.Error("Ready was not called")
	}
	if lockFree(t, filepath.Join(e.dirs.State, "sync.lock")) {
		t.Error("the root sync.lock is free while Hub.Serve runs")
	}
	for _, id := range []string{a, b} {
		if hl := health(t, h, id); hl.Serve != service.ServeServing {
			t.Errorf("Health(%s).Serve = %q, want serving", id, hl.Serve)
		}
	}
}

func TestOneAccountsCaptchaDoesNotStallTheOther(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	e.skip(t, h)
	e.clk.advance(loginguard.MinInterval + time.Second)
	ra, err := h.Login(t.Context(), service.LoginRequest{Username: userA, Password: e.password(userA), UnattendedRelogin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := ra.Account
	e.mustLogin(t, h, userB)
	// a's session ends; b logs in again, so only a's is dead. a's relogin
	// meets a CAPTCHA, with no SRP: under -race an SRP login inside the
	// wait could outlast it on a busy runner.
	e.f.RevokeSessions()
	h.LocalOf(a).SkipReloginSRP(func(string) (api.Tokens, string, error) {
		return api.Tokens{}, "", service.CaptchaError(captchaURL, "captcha-a")
	})
	b := e.mustLogin(t, h, userB).Account
	e.clk.advance(loginguard.MinInterval + time.Second)
	r := newServeRig(h)
	r.serve(t, r.options(true))
	r.waitPassed(t, b, 0)
	r.waitFor(t, "a waiting for a login", func() bool { return r.last(a) == service.ServeWaitingForLogin })
	info := accountInfo(t, h, a)
	if info.Phase != string(store.PhaseNeedsCaptcha) || info.Stopped != nil {
		t.Errorf("a = %+v, want needs-captcha and not stopped", info)
	}
	if hl := health(t, h, b); hl.Serve != service.ServeServing {
		t.Errorf("Health(b).Serve = %q, want serving", hl.Serve)
	}
}

func TestDamagedCacheParksOnlyThatAccount(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	a, b := e.loginBoth(t, h)
	path := e.damageCache(t, a)
	r := newServeRig(h)
	done, _ := r.serve(t, r.options(true))
	info := r.waitStopped(t, a)
	r.waitPassed(t, b, 0)
	if s := info.Stopped; s.Code != int(exitcode.MustAct) || !strings.Contains(s.Message, "damaged") || !strings.Contains(s.Hint, "pcb.db") || s.At.IsZero() {
		t.Errorf("a.Stopped = %+v, want the damaged cache (exit 10, its hint, a time)", s)
	}
	if hl := health(t, h, a); hl.Serve != service.ServeStopped {
		t.Errorf("Health(a).Serve = %q, want stopped", hl.Serve)
	}
	if info := accountInfo(t, h, b); info.Stopped != nil {
		t.Errorf("b.Stopped = %+v, want nil", info.Stopped)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := h.Restart(t.Context(), a)
	if err != nil {
		t.Fatalf("Restart(a): %v", err)
	}
	if got.ID != a || got.Stopped != nil {
		t.Errorf("Restart(a) = %+v, want a, not stopped", got)
	}
	r.waitPassed(t, a, 0)
	running(t, done)
}

func TestLoginRestartsAParkedAccount(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	a, _ := e.loginBoth(t, h)
	path := e.damageCache(t, a)
	r := newServeRig(h)
	r.serve(t, r.options(true))
	r.waitStopped(t, a)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	e.mustLogin(t, h, userA)
	r.waitPassed(t, a, 0)
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Errorf("a.Stopped = %+v after a login, want nil", info.Stopped)
	}
}

// parkBoth makes a's cache damaged and b's cache a directory, b's served
// only once a stopped: a stops first (exit 10), b second (exit 20).
func parkBoth(t *testing.T, e *hubEnv, h *service.Hub, a, b string) (r *serveRig, release func()) {
	t.Helper()
	e.damageCache(t, a)
	hold := make(chan struct{})
	h.LocalOf(b).BeforeServe(func() {
		select {
		case <-hold:
		case <-t.Context().Done():
		}
	})
	return newServeRig(h), func() {
		path := cache.Path(store.AccountDir(e.dirs.State, b))
		if err := os.RemoveAll(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		close(hold)
	}
}

func TestServeExitsWhenEveryAccountStopped(t *testing.T) {
	t.Parallel()
	t.Run("without WaitForLogin", func(t *testing.T) {
		t.Parallel()
		e := newServeEnv(t)
		h := e.open(t)
		a, b := e.loginBoth(t, h)
		r, release := parkBoth(t, e, h, a, b)
		done, _ := r.serve(t, r.options(false))
		r.waitStopped(t, a)
		running(t, done)
		release()
		select {
		case err := <-done:
			done <- err
			if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "damaged") {
				t.Errorf("Serve = %v, want the first stop's error (a's damaged cache)", err)
			}
		case <-time.After(waitLimit):
			t.Fatal("Serve kept running with every account stopped")
		}
		if s := accountInfo(t, h, b).Stopped; s == nil || s.Code != int(exitcode.Blocked) {
			t.Errorf("b.Stopped = %+v, want exit 20", s)
		}
	})
	t.Run("with WaitForLogin", func(t *testing.T) {
		t.Parallel()
		e := newServeEnv(t)
		h := e.open(t)
		a, b := e.loginBoth(t, h)
		r, release := parkBoth(t, e, h, a, b)
		done, stop := r.serve(t, r.options(true))
		r.waitStopped(t, a)
		release()
		r.waitStopped(t, b)
		// Still serving: a restarted account serves again.
		if err := os.Remove(cache.Path(store.AccountDir(e.dirs.State, a))); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Restart(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		r.waitPassed(t, a, 0)
		running(t, done)
		if err := stop(); err != nil {
			t.Errorf("Serve = %v after its context ended, want nil", err)
		}
	})
}

func TestServeRefusesWithNoLoggedInAccount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, e *hubEnv, h *service.Hub)
	}{
		{"no account", func(*testing.T, *hubEnv, *service.Hub) {}},
		{"signed out", func(t *testing.T, e *hubEnv, h *service.Hub) {
			e.skip(t, h)
			id := e.mustLogin(t, h, userA).Account
			if _, err := h.SignOut(t.Context(), id, service.LogoutRequest{}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newServeEnv(t)
			h := e.open(t)
			tc.setup(t, e, h)
			readied := false
			o := newServeRig(h).options(false)
			o.Ready = func() { readied = true }
			done := make(chan error, 1)
			go func() { done <- h.Serve(t.Context(), o) }()
			select {
			case err := <-done:
				_ = wantExit(t, err, exitcode.MustAct)
			case <-time.After(waitLimit):
				t.Fatal("Serve with no logged-in account did not return")
			}
			if readied {
				t.Error("Ready was called for a refused start")
			}
			if !lockFree(t, filepath.Join(e.dirs.State, "serve.lock")) {
				t.Error("serve.lock is still held after a refused start")
			}
		})
	}
}

func TestAddedAccountStartsServing(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	e.skip(t, h)
	a := e.mustLogin(t, h, userA).Account
	r := newServeRig(h)
	r.serve(t, r.options(true))
	r.waitPassed(t, a, 0)
	b := e.mustLogin(t, h, userB).Account
	r.waitPassed(t, b, 0)
}

// Ruling R18: a removed account's loop ends (its sync.lock is free) before
// Remove signs it out and deletes its directory.
func TestRemovedAccountStopsFirst(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	a, b := e.loginBoth(t, h)
	r := newServeRig(h)
	done, _ := r.serve(t, r.options(true))
	r.waitPassed(t, a, 0)
	r.waitPassed(t, b, 0)
	wiped, free := false, false
	h.LocalOf(a).OnWipe(func() {
		wiped = true
		free = lockFree(t, syncLock(e, a))
	})
	if _, err := h.Remove(t.Context(), a, service.LogoutRequest{}); err != nil {
		t.Fatalf("Remove(a): %v", err)
	}
	if !wiped || !free {
		t.Errorf("Remove's wipe ran %v, a's loop ended before it %v: want both", wiped, free)
	}
	if _, err := os.Stat(store.AccountDir(e.dirs.State, a)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a's directory: %v, want it gone", err)
	}
	if hl := health(t, h, b); hl.Serve != service.ServeServing {
		t.Errorf("Health(b).Serve = %q, want serving", hl.Serve)
	}
	running(t, done)
}

// Design §5.5b: the loop runs while either adapter is on; it pauses only
// once both are off, and either one switched on again resumes it with a
// pass at once.
func TestBothAdaptersOffPausesTheLoop(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	a, b := e.loginBoth(t, h)
	r := newServeRig(h)
	r.serve(t, r.options(true))
	r.waitPassed(t, a, 0)
	r.waitPassed(t, b, 0)
	if lockFree(t, syncLock(e, a)) {
		t.Fatal("a's sync.lock is free while a is served")
	}
	if _, err := h.SetCalDAV(t.Context(), a, false); err != nil {
		t.Fatal(err)
	}
	if lockFree(t, syncLock(e, a)) {
		t.Error("a's loop stopped with CalDAV off and MCP on (its sync.lock is free)")
	}
	if _, err := h.SetMCP(t.Context(), a, false); err != nil {
		t.Fatal(err)
	}
	if !lockFree(t, syncLock(e, a)) {
		t.Error("a's loop still runs with both adapters off (its sync.lock is held)")
	}
	if hl := health(t, h, a); hl.Serve != service.ServeStopped {
		t.Errorf("Health(a).Serve = %q with both adapters off, want stopped", hl.Serve)
	}
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Errorf("a.Stopped = %+v with both adapters off, want nil (paused, not parked)", info.Stopped)
	}
	n := r.passCount(a)
	if _, err := h.SetMCP(t.Context(), a, true); err != nil {
		t.Fatal(err)
	}
	r.waitPassed(t, a, n)
	if _, err := h.SetMCP(t.Context(), a, false); err != nil {
		t.Fatal(err)
	}
	if !lockFree(t, syncLock(e, a)) {
		t.Error("a's loop still runs with both adapters off again")
	}
	n = r.passCount(a)
	if _, err := h.SetCalDAV(t.Context(), a, true); err != nil {
		t.Fatal(err)
	}
	r.waitPassed(t, a, n)
	if hl := health(t, h, b); hl.Serve != service.ServeServing {
		t.Errorf("Health(b).Serve = %q, want serving", hl.Serve)
	}
}

// Design §5.5b: with CalDAV off and MCP on the loop keeps running, so the
// account's writes go through (an MCP write is a PutObject on its API),
// while CalDAV refuses its client (401: no API, no wait, no error).
func TestCalDAVOffMCPOnKeepsSyncing(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	a, _ := e.loginBoth(t, h)
	api, _, err := h.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := api.ShowClient(t.Context(), "default")
	if err != nil || creds.Password == "" {
		t.Fatalf("ShowClient(a) = %v, %v", creds.Password != "", err)
	}
	if _, err := h.SetCalDAV(t.Context(), a, false); err != nil {
		t.Fatal(err)
	}
	r := newServeRig(h)
	r.serve(t, r.options(true))
	r.waitPassed(t, a, 0)
	if lockFree(t, syncLock(e, a)) {
		t.Error("a's loop does not run with CalDAV off and MCP on (its sync.lock is free)")
	}
	if hl := health(t, h, a); hl.Serve != service.ServeServing {
		t.Errorf("Health(a).Serve = %q with CalDAV off and MCP on, want serving", hl.Serve)
	}
	for _, url := range []string{a, ""} {
		if got, _, wait, err := h.Route(t.Context(), url, creds.Password, "192.0.2.1:1"); got != nil || wait != 0 || err != nil {
			t.Errorf("Route(%q) with a's password, CalDAV off = %v, %v, %v; want a refusal (401)", url, got != nil, wait, err)
		}
	}
	cals, err := api.Calendars(t.Context())
	if err != nil || len(cals) != 1 {
		t.Fatalf("Calendars(a) = %+v, %v", cals, err)
	}
	res, err := api.PutObject(t.Context(), cals[0].ID, "mcp@x.ics", newEvent("mcp@x", "Planned by an agent"), service.Precondition{IfNoneMatchAny: true})
	if err != nil || !res.Created {
		t.Fatalf("PutObject with CalDAV off and MCP on = %+v, %v; want created", res, err)
	}
	ag, _, err := h.Agenda(a)
	if err != nil {
		t.Fatal(err)
	}
	start, end := service.EventTime{At: time.Unix(base, 0).UTC()}, service.EventTime{At: time.Unix(base+3600, 0).UTC()}
	summary := "Created through the agenda"
	ref, etag, err := ag.CreateEvent(t.Context(), cals[0].ID, service.EventFields{Summary: &summary, Start: &start, End: &end})
	if err != nil || etag == "" {
		t.Fatalf("Agenda.CreateEvent with CalDAV off and MCP on = %+v, %q, %v", ref, etag, err)
	}
	if d, err := ag.Event(t.Context(), ref); err != nil || d.Data.Summary != summary || d.ETag != etag {
		t.Errorf("Agenda.Event after the create = %+v, %v", d, err)
	}
}

// Design §5.5b, the exit rule: an account with CalDAV off and MCP on is
// served like one with CalDAV on. Without WaitForLogin, its running loop
// keeps Serve up while another account is parked; once its own session
// ends it has stopped for the rule (waiting for a login), and Serve exits
// with that stop, where a paused loop (both off) would keep it running.
func TestServeExitRuleCountsMCPOnAsRunning(t *testing.T) {
	t.Parallel()
	t.Run("served while another is parked", func(t *testing.T) {
		t.Parallel()
		e := newServeEnv(t)
		h := e.open(t)
		a, b := e.loginBoth(t, h)
		if _, err := h.SetCalDAV(t.Context(), b, false); err != nil {
			t.Fatal(err)
		}
		e.damageCache(t, a)
		r := newServeRig(h)
		done, _ := r.serve(t, r.options(false))
		r.waitStopped(t, a)
		r.waitPassed(t, b, 0)
		running(t, done)
	})
	t.Run("its ended session stops it", func(t *testing.T) {
		t.Parallel()
		e := newServeEnv(t)
		h := e.open(t)
		e.skip(t, h)
		a := e.mustLogin(t, h, userA).Account
		if _, err := h.SetCalDAV(t.Context(), a, false); err != nil {
			t.Fatal(err)
		}
		e.f.RevokeSessions()
		r := newServeRig(h)
		done, _ := r.serve(t, r.options(false))
		select {
		case err := <-done:
			done <- err
			_ = wantExit(t, err, exitcode.MustAct)
		case <-time.After(waitLimit):
			t.Fatal("Serve kept running with its only account, CalDAV off and MCP on, waiting for a login")
		}
	})
}

func TestSecondServeIsRefused(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	r := newServeRig(h)
	ready := make(chan struct{})
	o := r.options(true)
	o.Ready = func() { close(ready) }
	_, stop := r.serve(t, o)
	select {
	case <-ready:
	case <-time.After(waitLimit):
		t.Fatal("Hub.Serve with WaitForLogin never got ready")
	}
	pid := strconv.Itoa(os.Getpid())
	raw, err := os.ReadFile(filepath.Join(e.dirs.State, "serve.lock"))
	if err != nil || string(raw) != pid+"\n" {
		t.Errorf("serve.lock = %q, %v, want %q", raw, err, pid+"\n")
	}
	root := filepath.Join(e.dirs.State, "sync.lock")
	if lockFree(t, root) {
		t.Error("the root sync.lock is free while Hub.Serve runs")
	}
	h2 := e.open(t)
	err = h2.Serve(t.Context(), newServeRig(h2).options(true))
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Error() != "pcb is already serving (pid "+pid+")" || ee.Hint != "stop it first, or use pcb status" {
		t.Errorf("second Serve = %q (hint %q)", ee.Error(), ee.Hint)
	}
	if err := stop(); err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}
	if !lockFree(t, root) || !lockFree(t, filepath.Join(e.dirs.State, "serve.lock")) {
		t.Error("Serve returned holding serve.lock or the root sync.lock")
	}
	r2 := newServeRig(h2)
	ready2 := make(chan struct{})
	o2 := r2.options(true)
	o2.Ready = func() { close(ready2) }
	r2.serve(t, o2)
	select {
	case <-ready2:
	case <-time.After(waitLimit):
		t.Fatal("a Serve after the first ended never got ready")
	}
}

// The login guard at the root spaces the unattended relogins of two
// accounts whose sessions ended together: the stamp each attempt took
// (read where its SRP step runs, under the guard's lock) is at least
// loginguard.MinInterval after the previous one. The relogins skip SRP and
// take sessions opened before Serve starts (the guard is the subject, not
// the login): under -race, two SRP logins inside the wait for both to pass
// outlasted it on a busy CI runner.
func TestReloginsOfTwoAccountsAreSpaced(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	var (
		mu     sync.Mutex
		stamps []time.Time
		bad    []string
	)
	stamp := filepath.Join(e.dirs.State, "last-login")
	fresh := map[string]api.Tokens{} // by username, opened after the revoke
	relogin := func(username string) (api.Tokens, string, error) {
		raw, err := os.ReadFile(stamp)
		at, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
		mu.Lock()
		defer mu.Unlock()
		if err != nil || perr != nil {
			bad = append(bad, fmt.Sprintf("%v %v", err, perr))
		} else {
			stamps = append(stamps, at)
		}
		return fresh[username], e.f.UserID(username), nil
	}
	// Every back-off moves the clock; once both relogged, loops wait.
	var settled sync.Once
	quiet := make(chan struct{})
	e.sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case <-quiet:
			<-ctx.Done()
			return ctx.Err()
		default:
		}
		e.clk.advance(d)
		return ctx.Err()
	}
	h := e.open(t)
	e.skip(t, h)
	var ids []string
	for _, u := range []string{userA, userB} {
		e.clk.advance(loginguard.MinInterval + time.Second)
		res, err := h.Login(t.Context(), service.LoginRequest{Username: u, Password: e.password(u), UnattendedRelogin: true})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, res.Account)
		h.LocalOf(res.Account).SkipReloginSRP(relogin)
	}
	e.f.RevokeSessions()
	mu.Lock()
	for _, u := range []string{userA, userB} {
		fresh[u] = e.f.Login(t, u) // SRP, outside the wait
	}
	mu.Unlock()
	e.clk.advance(loginguard.MinInterval + time.Second)
	r := newServeRig(h)
	r.serve(t, r.options(true))
	r.waitFor(t, "both accounts relogged and passing", func() bool {
		if r.okPasses(ids[0]) > 0 && r.okPasses(ids[1]) > 0 {
			settled.Do(func() { close(quiet) })
			return true
		}
		return false
	})
	mu.Lock()
	defer mu.Unlock()
	if len(bad) > 0 {
		t.Fatalf("the root login stamp could not be read at a login: %v", bad)
	}
	if len(stamps) < 2 {
		t.Fatalf("%d relogins, want one per account", len(stamps))
	}
	for i := 1; i < len(stamps); i++ {
		if gap := stamps[i].Sub(stamps[i-1]); gap < loginguard.MinInterval {
			t.Errorf("relogin %d came %v after the previous one, want at least %v", i, gap, loginguard.MinInterval)
		}
	}
}

// adapterSwitch is one of an account's adapter switches: how to turn it off
// through the Hub, and its value in a state.json.
type adapterSwitch struct {
	name string
	off  func(ctx context.Context, h *service.Hub, id string) error
	of   func(st store.State) *bool
}

var (
	caldavSwitch = adapterSwitch{"CalDAV",
		func(ctx context.Context, h *service.Hub, id string) error {
			_, err := h.SetCalDAV(ctx, id, false)
			return err
		},
		func(st store.State) *bool { return st.CalDAV }}
	mcpSwitch = adapterSwitch{"MCP",
		func(ctx context.Context, h *service.Hub, id string) error {
			_, err := h.SetMCP(ctx, id, false)
			return err
		},
		func(st store.State) *bool { return st.MCP }}
)

// Ruling R19: a switch made while an unattended relogin runs is kept: the
// relogin's state write reads the switch again under state.lock.
func TestCalDAVSwitchDuringAReloginIsKept(t *testing.T) {
	t.Parallel()
	switchDuringARelogin(t, caldavSwitch)
}

// Design §5.5b: the MCP switch is kept as the CalDAV one is (ruling R19).
func TestMCPSwitchDuringAReloginIsKept(t *testing.T) {
	t.Parallel()
	switchDuringARelogin(t, mcpSwitch)
}

func switchDuringARelogin(t *testing.T, sw adapterSwitch) {
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	e.clk.advance(loginguard.MinInterval + time.Second)
	res, err := h.Login(t.Context(), service.LoginRequest{Username: userA, Password: e.password(userA), UnattendedRelogin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Account
	e.f.RevokeSessions()
	// The relogin takes a session opened now instead of SRP: under -race
	// an SRP login inside the wait for its key calls could outlast it.
	tok := e.f.Login(t, userA)
	h.LocalOf(a).SkipReloginSRP(func(u string) (api.Tokens, string, error) { return tok, e.f.UserID(u), nil })
	e.clk.advance(loginguard.MinInterval + time.Second)
	entered, release := make(chan struct{}), make(chan struct{})
	h.LocalOf(a).SetKeyPassHook(func([]byte) {
		close(entered)
		<-release
	})
	acct, _, err := h.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := acct.Calendars(t.Context())
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the relogin never reached its key calls")
	}
	if err := sw.off(t.Context(), h, a); err != nil {
		t.Fatal(err)
	}
	setAddress(t, e, a, "A@x")
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Calendars after the relogin: %v", err)
	}
	st, err := store.LoadState(store.AccountDir(e.dirs.State, a))
	if err != nil {
		t.Fatal(err)
	}
	if on := sw.of(st); on == nil || *on || st.Phase != store.PhaseReady || st.LastRelogin.IsZero() || st.Address != "A@x" {
		t.Errorf("state = %s %v, phase %q, last relogin %v, address %q; want %[1]s off and the address A@x kept by a completed relogin",
			sw.name, on, st.Phase, st.LastRelogin, st.Address)
	}
}

// Ruling R19, the CAPTCHA side: a relogin that meets a CAPTCHA keeps a
// switch made while it ran.
func TestCalDAVSwitchDuringACaptchaReloginIsKept(t *testing.T) {
	t.Parallel()
	switchDuringACaptchaRelogin(t, caldavSwitch)
}

// Design §5.5b: the MCP switch is kept by a CAPTCHA relogin too.
func TestMCPSwitchDuringACaptchaReloginIsKept(t *testing.T) {
	t.Parallel()
	switchDuringACaptchaRelogin(t, mcpSwitch)
}

func switchDuringACaptchaRelogin(t *testing.T, sw adapterSwitch) {
	e := newHubEnv(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var freed sync.Once
	free := func() { freed.Do(func() { close(release) }) }
	t.Cleanup(free) // a relogin still held when the test stops early
	h := e.open(t)
	e.skip(t, h)
	e.clk.advance(loginguard.MinInterval + time.Second)
	res, err := h.Login(t.Context(), service.LoginRequest{Username: userA, Password: e.password(userA), UnattendedRelogin: true})
	if err != nil {
		t.Fatal(err)
	}
	a := res.Account
	e.f.RevokeSessions()
	// The relogin is held where its SRP step runs, then meets a CAPTCHA,
	// with no SRP: under -race an SRP login inside the wait for it could
	// outlast it on a busy runner.
	var held sync.Once
	h.LocalOf(a).SkipReloginSRP(func(string) (api.Tokens, string, error) {
		held.Do(func() {
			close(entered)
			<-release
		})
		return api.Tokens{}, "", service.CaptchaError(captchaURL, "captcha-a")
	})
	e.clk.advance(loginguard.MinInterval + time.Second)
	acct, _, err := h.Account(a)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := acct.Calendars(t.Context())
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("the relogin never reached its SRP step")
	}
	if err := sw.off(t.Context(), h, a); err != nil {
		t.Fatal(err)
	}
	setAddress(t, e, a, "A@x")
	free()
	_ = wantExit(t, <-done, exitcode.MustAct)
	st, err := store.LoadState(store.AccountDir(e.dirs.State, a))
	if err != nil {
		t.Fatal(err)
	}
	if on := sw.of(st); on == nil || *on || st.Phase != store.PhaseNeedsCaptcha || st.Address != "A@x" {
		t.Errorf("state = %s %v, phase %q, address %q; want %[1]s off and the address A@x kept by the CAPTCHA relogin", sw.name, on, st.Phase, st.Address)
	}
}

// Without WaitForLogin, an account waiting for a login has stopped for the
// exit rule: with one account whose session ends, Serve returns that stop
// (exit 10), as the single-account Serve does.
func TestServeWithoutTheFlagExitsWhenTheOnlyAccountNeedsALogin(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	e.skip(t, h)
	a := e.mustLogin(t, h, userA).Account
	e.f.RevokeSessions()
	r := newServeRig(h)
	done, _ := r.serve(t, r.options(false))
	select {
	case err := <-done:
		done <- err
		if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "session") || !strings.Contains(ee.Hint, "pcb login") {
			t.Errorf("Serve = %v (hint %q), want the ended session", err, ee.Hint)
		}
	case <-time.After(waitLimit):
		t.Fatal("Serve kept running with its only account waiting for a login")
	}
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Errorf("a.Stopped = %+v, want nil: a login fixes it", info.Stopped)
	}
}

// setAddress writes the account's address into its state.json, as a
// backfill or another casing would.
func setAddress(t *testing.T, e *hubEnv, id, address string) {
	t.Helper()
	if err := store.UpdateState(store.AccountDir(e.dirs.State, id), func(s *store.State) error {
		s.Address = address
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A Restart that lands while a loop ends on a park, once the park is
// published, starts a new loop: the account serves again, not parked.
func TestRestartWhileALoopParksServesAgain(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	h := e.open(t)
	a, _ := e.loginBoth(t, h)
	path := e.damageCache(t, a)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.OnPark(func(id string) {
		if id != a {
			return
		}
		once.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-t.Context().Done():
			}
		})
	})
	r := newServeRig(h)
	r.serve(t, r.options(true))
	select {
	case <-entered:
	case <-time.After(waitLimit):
		t.Fatal("a's loop never parked")
	}
	r.mu.Lock()
	last := r.last(a)
	r.mu.Unlock()
	if last != service.ServeStopped {
		// Reported later, it could follow the new loop's serving.
		t.Errorf("a's last state at its park = %q, want stopped reported before the park", last)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err := h.Restart(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if got.Stopped != nil {
		t.Errorf("Restart(a) = %+v, want not stopped", got)
	}
	r.waitPassed(t, a, 0)
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Errorf("a.Stopped = %+v after the Restart, want nil", info.Stopped)
	}
	r.waitFor(t, "a serving after the Restart", func() bool { return r.last(a) == service.ServeServing })
}

// Ruling R23: another pcb holding an account's sync.lock (an offline sync
// or a watch) is no park: the loop takes it again every SyncLockRetry and
// serves once it is free.
func TestHeldSyncLockIsRetriedNotParked(t *testing.T) {
	t.Parallel()
	e := newServeEnv(t)
	waiting, retry := make(chan struct{}, 1), make(chan struct{})
	e.sleep = func(ctx context.Context, d time.Duration) error {
		if d == service.SyncLockRetry {
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
	unlock, ok, err := filelock.TryLock(syncLock(e, a))
	if err != nil || !ok {
		t.Fatalf("TryLock(a's sync.lock) = %v, %v", ok, err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	r := newServeRig(h)
	done, _ := r.serve(t, r.options(false))
	select {
	case <-waiting:
	case <-time.After(waitLimit):
		t.Fatal("the loop never waited to take the sync.lock again")
	}
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Fatalf("a.Stopped = %+v while another holds its sync.lock, want nil", info.Stopped)
	}
	running(t, done)
	unlock()
	locked = false
	select {
	case retry <- struct{}{}:
	case <-time.After(waitLimit):
		t.Fatal("the retry wait ended before it was released")
	}
	r.waitPassed(t, a, 0)
	if info := accountInfo(t, h, a); info.Stopped != nil {
		t.Errorf("a.Stopped = %+v once served, want nil", info.Stopped)
	}
}

// serve.lock records the holder's kind beside its PID; a second Serve names
// both, and for an agent session (kind mcp) says to end it.
func TestServeLockNamesTheKind(t *testing.T) {
	t.Parallel()
	pid := strconv.Itoa(os.Getpid())
	for _, tc := range []struct{ kind, hint string }{
		{"mcp", "end the agent session (pcb mcp), then try again"},
		{"serve", "stop it first, or use pcb status"},
	} {
		e := newServeEnv(t)
		h := e.open(t)
		ready := make(chan struct{})
		o := newServeRig(h).options(true)
		o.Kind, o.Ready = tc.kind, func() { close(ready) }
		_, stop := newServeRig(h).serve(t, o)
		select {
		case <-ready:
		case <-time.After(waitLimit):
			t.Fatalf("kind %s: Hub.Serve never got ready", tc.kind)
		}
		raw, err := os.ReadFile(filepath.Join(e.dirs.State, "serve.lock"))
		if want := pid + " " + tc.kind + "\n"; err != nil || string(raw) != want {
			t.Errorf("serve.lock = %q, %v, want %q", raw, err, want)
		}
		h2 := e.open(t)
		o2 := newServeRig(h2).options(true)
		o2.Kind = "ui"
		ee := wantExit(t, h2.Serve(t.Context(), o2), exitcode.Blocked)
		if want := "pcb is already serving (pid " + pid + ", kind " + tc.kind + ")"; ee.Error() != want || ee.Hint != tc.hint {
			t.Errorf("second Serve = %q (hint %q), want %q (hint %q)", ee.Error(), ee.Hint, want, tc.hint)
		}
		if err := stop(); err != nil {
			t.Fatalf("Serve = %v", err)
		}
	}
}

// A serve.lock of an older pcb holds the PID alone: it is still named,
// with no kind. A kind that is not a plain word is not repeated.
func TestServeLockReadsTheOldFormat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ content, want string }{
		{"4242\n", "pcb is already serving (pid 4242)"},
		{"4242 \x1b[31mred\n", "pcb is already serving (pid 4242)"},
		{"4242 mcp extra\n", "pcb is already serving (pid 4242)"},
		{"", "pcb is already serving"},
	} {
		e := newServeEnv(t)
		path := filepath.Join(e.dirs.State, "serve.lock")
		if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
			t.Fatal(err)
		}
		unlock, ok, err := filelock.TryLock(path)
		if err != nil || !ok {
			t.Fatalf("cannot take serve.lock: %v %v", ok, err)
		}
		if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		h := e.open(t)
		o := newServeRig(h).options(true)
		o.Kind = "serve"
		ee := wantExit(t, h.Serve(t.Context(), o), exitcode.Blocked)
		if ee.Error() != tc.want || ee.Hint != "stop it first, or use pcb status" {
			t.Errorf("%q: Serve = %q (hint %q), want %q", tc.content, ee.Error(), ee.Hint, tc.want)
		}
		unlock()
	}
}
