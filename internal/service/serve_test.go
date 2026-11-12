package service_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// Ready tells pcb serve when to bind its listener: never when Serve cannot
// run (logged out, the sync lock taken), and before the first pass once it
// holds the lock and its cache handle.
func TestServeCallsReadyOnlyOnceItRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	var readies atomic.Int32
	o := service.ServeOptions{Ready: func() { readies.Add(1) }}

	err := e.svc.Serve(t.Context(), o)
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "pcb login") {
		t.Errorf("logged out: hint %q", ee.Hint)
	}
	if n := readies.Load(); n != 0 {
		t.Fatalf("Ready called %d times while logged out", n)
	}

	e.signIn(t)
	if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := filelock.TryLock(filepath.Join(e.dirs.State, "sync.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	err = e.svc.Serve(t.Context(), o)
	unlock()
	if ee := wantExit(t, err, exitcode.Blocked); !strings.Contains(ee.Error(), "a sync is already running") {
		t.Errorf("lock taken: %v", err)
	}
	if n := readies.Load(); n != 0 {
		t.Fatalf("Ready called %d times while the lock was taken", n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var readyAtFirstPass int32 = -1
	o.OnPass = func(service.WatchPass) {
		if readyAtFirstPass < 0 {
			readyAtFirstPass = readies.Load()
		}
		cancel()
	}
	if err := e.svc.Serve(ctx, o); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if readyAtFirstPass != 1 || readies.Load() != 1 {
		t.Errorf("Ready calls at the first pass %d, in all %d; want 1 and 1", readyAtFirstPass, readies.Load())
	}
}

// idleRig is a logged-in service whose Sleep moves the fake clock by what it
// is asked to wait, except a sleep of IdleInterval: that one is reported on
// idle and lasts until its context ends (a wake), so a test sees the loop
// asleep at the slow cadence. Each pass waits in OnPass until the test
// resumes it.
type idleRig struct {
	*env
	idle   chan struct{} // one send per idle sleep
	passes chan atPass
	resume chan struct{}
	pw     string // a CalDAV client's password
	work   string // the calendar Work
	// hold makes every sleep but the active one wait like the idle sleep,
	// until its context ends or release is sent; held is its length and
	// cut whether its context had ended when it returned.
	hold    atomic.Bool
	release chan struct{}
	held    atomic.Int64
	cut     atomic.Bool
}

// atPass is a pass and the fake time its callback ran at.
type atPass struct {
	service.WatchPass
	At time.Time
}

func newIdleRig(t *testing.T) *idleRig {
	t.Helper()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.signIn(t)
	r := &idleRig{env: e, idle: make(chan struct{}), passes: make(chan atPass), resume: make(chan struct{}), release: make(chan struct{}), work: work}
	svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: func(ctx context.Context, d time.Duration) error {
		if d == service.IdleInterval || (r.hold.Load() && d != service.ActiveInterval) {
			r.held.Store(int64(d))
			select {
			case r.idle <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-ctx.Done():
				r.cut.Store(true)
				return ctx.Err()
			case <-r.release:
				r.cut.Store(ctx.Err() != nil)
				return nil
			}
		}
		e.clk.advance(d)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	e.svc = svc
	sec, err := svc.AddClient(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	r.pw = sec.Password
	return r
}

// serve runs Serve in the background, each pass paused in its callback
// (waitPass, resume); the returned stop ends it.
func (r *idleRig) serve(t *testing.T, o service.ServeOptions) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	o.OnPass = func(p service.WatchPass) {
		select {
		case r.passes <- atPass{p, r.clk.Now()}:
		case <-ctx.Done():
			return
		}
		select {
		case <-r.resume:
		case <-ctx.Done():
		}
	}
	go func() { done <- r.svc.Serve(ctx, o) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve = %v", err)
		}
	}
}

// waitPass returns the next pass, paused in its callback.
func (r *idleRig) waitPass(t *testing.T) atPass {
	t.Helper()
	select {
	case p := <-r.passes:
		return p
	case <-time.After(waitLimit):
		t.Fatal("no pass ran")
	}
	return atPass{}
}

// waitIdle waits for the loop to sleep at the idle cadence.
func (r *idleRig) waitIdle(t *testing.T) {
	t.Helper()
	select {
	case <-r.idle:
	case <-time.After(waitLimit):
		t.Fatal("the loop never went to sleep at the idle cadence")
	}
}

// goIdle runs passes (the clock jumping 34 minutes after the first) until
// the loop sleeps at the idle cadence.
func (r *idleRig) goIdle(t *testing.T) {
	t.Helper()
	r.waitPass(t)
	r.clk.advance(service.IdleAfter - time.Minute)
	r.resume <- struct{}{}
	for r.waitPass(t).Next != service.IdleInterval {
		r.resume <- struct{}{}
	}
	r.resume <- struct{}{}
	r.waitIdle(t)
}

// With no authenticated request, the passes stay 30 s apart until 35 minutes
// after Serve began (Thunderbird's 30-minute refresh keeps its cadence),
// then wait 5 minutes.
func TestIdleServeSlowsToFiveMinutes(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	start := r.clk.Now()
	stop := r.serve(t, service.ServeOptions{})
	defer stop()
	p1 := r.waitPass(t)
	if p1.Next != service.ActiveInterval {
		t.Fatalf("first pass waits %v, want %v", p1.Next, service.ActiveInterval)
	}
	r.clk.advance(service.IdleAfter - time.Minute) // 34 minutes without a client
	r.resume <- struct{}{}
	p2 := r.waitPass(t) // 34:30 after the sleep
	if p2.At.Sub(start) != 34*time.Minute+30*time.Second || p2.Next != service.ActiveInterval {
		t.Fatalf("pass at %v waits %v, want the active %v before 35 minutes", p2.At.Sub(start), p2.Next, service.ActiveInterval)
	}
	r.resume <- struct{}{}
	p3 := r.waitPass(t) // exactly 35:00
	if p3.At.Sub(start) != service.IdleAfter || p3.Next != service.IdleInterval {
		t.Fatalf("pass at %v waits %v, want %v from %v on", p3.At.Sub(start), p3.Next, service.IdleInterval, service.IdleAfter)
	}
	if h, err := r.svc.Health(t.Context()); err != nil || !h.Idle || h.IntervalSeconds != 300 {
		t.Errorf("Health: %+v, %v; want idle at 300 s", h, err)
	}
	r.resume <- struct{}{}
	r.waitIdle(t)
}

// A request that CheckClient accepts ends an idle sleep: a pass starts at
// once and the cadence is the active one again.
func TestAuthenticatedRequestWakesAnIdleLoop(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	stop := r.serve(t, service.ServeOptions{})
	defer stop()
	r.goIdle(t)
	if ok, _ := r.svc.CheckClient(t.Context(), r.pw, "192.0.2.1:1000"); !ok {
		t.Fatal("the client was refused")
	}
	p := r.waitPass(t) // the pass the wake started
	if p.Err != nil || p.Next != service.ActiveInterval {
		t.Errorf("pass after the wake: err %v, next %v; want a good pass and %v", p.Err, p.Next, service.ActiveInterval)
	}
	if h, err := r.svc.Health(t.Context()); err != nil || h.Idle || h.IntervalSeconds != 30 {
		t.Errorf("Health after the wake: %+v, %v; want active at 30 s", h, err)
	}
	// Active again: a second request wakes nothing.
	if ok, _ := r.svc.CheckClient(t.Context(), r.pw, "192.0.2.1:1001"); !ok {
		t.Fatal("the client was refused")
	}
	if n := r.svc.WakesSent(); n != 1 {
		t.Errorf("%d wakes sent, want 1 (the second request found the loop active)", n)
	}
}

// A set Interval replaces the active cadence, and the idle one is the
// longer of it and 5 minutes: it never speeds up when idle.
func TestSetIntervalReplacesTheActiveCadence(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	stop := r.serve(t, service.ServeOptions{Interval: 10 * time.Minute})
	defer stop()
	// Passes 10 minutes apart: the 5th is at 40 minutes, idle.
	for i := range 6 {
		if p := r.waitPass(t); p.Next != 10*time.Minute {
			t.Fatalf("pass %d waits %v, want 10m0s idle or not", i+1, p.Next)
		}
		r.resume <- struct{}{}
	}
}

// A password CheckClient refuses is no activity: it does not wake the loop
// nor push the idle time back, from a remote that fails or one that is
// blocked.
func TestRefusedPasswordIsNotActivity(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	stop := r.serve(t, service.ServeOptions{})
	defer stop()
	r.goIdle(t)
	var blocked bool
	for range 7 { // a remote with 5 failures a minute is refused unexamined
		ok, wait := r.svc.CheckClient(t.Context(), "not-a-password", "192.0.2.9:1")
		if ok {
			t.Fatal("a wrong password was accepted")
		}
		blocked = blocked || wait > 0
	}
	if !blocked {
		t.Fatal("the remote was never blocked: the test does not reach that path")
	}
	if n := r.svc.WakesSent(); n != 0 {
		t.Errorf("%d wakes sent for refused passwords", n)
	}
	if h, err := r.svc.Health(t.Context()); err != nil || !h.Idle {
		t.Errorf("Health after refusals: %+v, %v; want still idle", h, err)
	}
}

// waitWakeTaken waits for the loop to take the wake sent to it, failing the
// test past waitLimit.
func (r *idleRig) waitWakeTaken(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for r.svc.WakePending() {
		if time.Now().After(deadline) {
			t.Fatal("the loop never took the wake")
		}
		runtime.Gosched()
	}
}

// failEvents makes the next n listings of calendar id's events answer 400.
func (r *idleRig) failEvents(id string, n int) {
	for range n {
		r.f.FailNext(http.MethodGet, "/calendar/v1/"+id+"/events", http.StatusBadRequest, 2000, 0)
	}
}

// Ruling M5-R1: a pass where some calendar synced and another failed keeps
// the cadence: it goes idle after 35 minutes, and a request wakes it.
func TestPartialPassKeepsTheCadence(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	bad := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Bad"})
	r.failEvents(bad, 30)
	stop := r.serve(t, service.ServeOptions{})
	defer stop()
	p := r.waitPass(t)
	if exitcode.From(p.Err) != exitcode.Partial || p.Next != service.ActiveInterval {
		t.Fatalf("first pass err %v next %v; want partial and %v", p.Err, p.Next, service.ActiveInterval)
	}
	r.clk.advance(service.IdleAfter - time.Minute)
	r.resume <- struct{}{}
	for p = r.waitPass(t); p.Next != service.IdleInterval; p = r.waitPass(t) {
		if exitcode.From(p.Err) != exitcode.Partial || p.Next != service.ActiveInterval {
			t.Fatalf("pass err %v next %v; want a partial pass at %v until idle", p.Err, p.Next, service.ActiveInterval)
		}
		r.resume <- struct{}{}
	}
	if exitcode.From(p.Err) != exitcode.Partial {
		t.Fatalf("idle pass err %v, want partial", p.Err)
	}
	r.resume <- struct{}{}
	r.waitIdle(t)
	if ok, _ := r.svc.CheckClient(t.Context(), r.pw, "192.0.2.1:1000"); !ok {
		t.Fatal("the client was refused")
	}
	p = r.waitPass(t) // the wake started it
	if exitcode.From(p.Err) != exitcode.Partial || p.Next != service.ActiveInterval {
		t.Errorf("pass after the wake: err %v next %v; want partial and %v", p.Err, p.Next, service.ActiveInterval)
	}
}

// A pass where no calendar synced backs off (doubling), and a wake, even
// one that finds the loop idle, does not shorten the back-off.
func TestWakeDuringBackOffDoesNotShortenIt(t *testing.T) {
	t.Parallel()
	r := newIdleRig(t)
	r.failEvents(r.work, 30)
	stop := r.serve(t, service.ServeOptions{})
	defer stop()
	p1 := r.waitPass(t)
	if exitcode.From(p1.Err) != exitcode.Partial || p1.Next < 27*time.Second || p1.Next > 33*time.Second {
		t.Fatalf("pass 1 err %v next %v; want the only calendar failing and a 30 s back-off ±10%%", p1.Err, p1.Next)
	}
	r.clk.advance(service.IdleAfter - time.Minute)
	r.resume <- struct{}{}
	p2 := r.waitPass(t)
	if p2.Next < 54*time.Second || p2.Next > 66*time.Second {
		t.Fatalf("pass 2 waits %v, want the doubled back-off (60 s ±10%%)", p2.Next)
	}
	r.clk.advance(time.Minute) // past the idle time
	r.hold.Store(true)
	r.resume <- struct{}{}
	r.waitIdle(t) // asleep in the back-off
	if got := time.Duration(r.held.Load()); got != p2.Next {
		t.Fatalf("slept %v, want the reported %v", got, p2.Next)
	}
	if ok, _ := r.svc.CheckClient(t.Context(), r.pw, "192.0.2.1:1000"); !ok {
		t.Fatal("the client was refused")
	}
	if n := r.svc.WakesSent(); n != 1 {
		t.Fatalf("%d wakes sent, want 1: the loop was idle", n)
	}
	r.waitWakeTaken(t)
	// A second wake, sent once idle again, is taken only if the loop is still
	// listening after the first one: a loop that ended its sleep on the first
	// no longer takes any.
	r.clk.advance(service.IdleAfter)
	if ok, _ := r.svc.CheckClient(t.Context(), r.pw, "192.0.2.1:1001"); !ok {
		t.Fatal("the client was refused")
	}
	r.waitWakeTaken(t)
	r.release <- struct{}{} // the back-off ends by itself
	if r.cut.Load() {
		t.Error("the wake ended the back-off")
	}
	if p3 := r.waitPass(t); p3.Next < 108*time.Second || p3.Next > 132*time.Second {
		t.Errorf("pass 3 waits %v, want the back-off to go on doubling (120 s ±10%%)", p3.Next)
	}
}

// waitRig is a service whose sync loop sleeps until tick or its context
// ends, so a test runs one pass at a time; it counts the login checks of a
// Serve waiting for a login (polls).
type waitRig struct {
	*env
	tick   chan struct{}
	states chan service.ServeState
	passes chan service.WatchPass
	polls  atomic.Int64
	work   string // the calendar Work
	// atState, when set, runs in OnState before the state is sent.
	atState func(service.ServeState)
}

func newWaitRig(t *testing.T) *waitRig {
	t.Helper()
	e := newEnv(t, protontest.Options{})
	work := e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	r := &waitRig{env: e, tick: make(chan struct{}), states: make(chan service.ServeState, 16), passes: make(chan service.WatchPass, 16), work: work}
	e.svc = r.openTicked(t)
	e.svc.OnLoginCheck(func() { r.polls.Add(1) })
	return r
}

// openTicked opens a service on the rig's directories whose sleeps last
// until tick or their context ends.
func (r *waitRig) openTicked(t *testing.T) *service.Local {
	t.Helper()
	svc, err := service.NewLocal(service.Options{Dirs: r.dirs, Config: r.cfg, Now: r.clk.Now, Sleep: func(ctx context.Context, _ time.Duration) error {
		select {
		case <-r.tick:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// serve runs Serve in the background with o, its states and passes sent to
// the rig's channels; stop ends it and returns what it returned.
func (r *waitRig) serve(t *testing.T, o service.ServeOptions) (done chan error, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done = make(chan error, 1)
	o.OnState = func(s service.ServeState) {
		if r.atState != nil {
			r.atState(s)
		}
		r.states <- s
	}
	o.OnPass = func(p service.WatchPass) { r.passes <- p }
	go func() { done <- r.svc.Serve(ctx, o) }()
	var once sync.Once
	var err error
	stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(waitLimit):
				t.Error("Serve did not return after its context ended")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = stop() })
	return done, stop
}

// wantState waits for Serve to report want.
func (r *waitRig) wantState(t *testing.T, want service.ServeState) {
	t.Helper()
	select {
	case s := <-r.states:
		if s != want {
			t.Fatalf("Serve reported %q, want %q", s, want)
		}
	case <-time.After(waitLimit):
		t.Fatalf("Serve never reported %q", want)
	}
}

// wantPass waits for the next pass and fails the test if it failed.
func (r *waitRig) wantPass(t *testing.T) service.WatchPass {
	t.Helper()
	select {
	case p := <-r.passes:
		return p
	case <-time.After(waitLimit):
		t.Fatal("no pass ran")
	}
	return service.WatchPass{}
}

// running fails the test if Serve returned.
func running(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		done <- err
		t.Fatalf("Serve returned %v, want it still waiting", err)
	default:
	}
}

// Logged out with WaitForLogin, Serve is ready at once and waits: the reads
// answer ErrWaitingForLogin, and an in-process login starts serving with
// no poll (the poll interval is an hour).
func TestServeWaitsForALogin(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	var readies atomic.Int32
	done, stop := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour, Ready: func() { readies.Add(1) }})
	r.wantState(t, service.ServeWaitingForLogin)
	if n := readies.Load(); n != 1 {
		t.Fatalf("Ready called %d times while waiting, want 1", n)
	}
	if h := r.health(t); h.Serve != service.ServeWaitingForLogin || h.Since == nil {
		t.Errorf("Health while waiting: %+v", h)
	}
	_, err := r.svc.Collections(t.Context())
	if ee := wantExit(t, err, exitcode.Blocked); !errors.Is(err, service.ErrWaitingForLogin) || ee.Hint != "run pcb login" {
		t.Errorf("Collections while waiting = %v (hint %q), want ErrWaitingForLogin, hint run pcb login", err, ee.Hint)
	}
	checks := r.polls.Load()

	r.signIn(t)
	r.wantState(t, service.ServeServing)
	if p := r.wantPass(t); p.Err != nil || len(p.Result.Calendars) != 1 {
		t.Fatalf("first pass after the login: %+v", p)
	}
	if n := r.polls.Load() - checks; n != 1 {
		t.Errorf("%d login checks after the login, want the 1 its wake started", n)
	}
	if h := r.health(t); h.Serve != service.ServeServing || len(h.Calendars) != 1 {
		t.Errorf("Health once serving: %+v", h)
	}
	if colls, err := r.svc.Collections(t.Context()); err != nil || len(colls) != 1 {
		t.Errorf("Collections once serving = %v, %v", colls, err)
	}
	running(t, done)
	if err := stop(); err != nil {
		t.Errorf("Serve after its context ended = %v, want nil", err)
	}
	if h := r.health(t); h.Serve != service.ServeStopped {
		t.Errorf("Health after Serve = %q, want stopped", h.Serve)
	}
}

// Every read and write answers ErrWaitingForLogin while Serve waits.
func TestEveryReadAndWriteWaitsForTheLogin(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour})
	r.wantState(t, service.ServeWaitingForLogin)
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"Objects":       func() error { _, _, err := r.svc.Objects(ctx, "cal"); return err },
		"ObjectsByUID":  func() error { _, err := r.svc.ObjectsByUID(ctx, "cal", []string{"a"}); return err },
		"ObjectsByName": func() error { _, err := r.svc.ObjectsByName(ctx, "cal", []string{"a.ics"}); return err },
		"Changes":       func() error { _, err := r.svc.Changes(ctx, "cal", ""); return err },
		"PutObject": func() error {
			_, err := r.svc.PutObject(ctx, "cal", "a.ics", []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"), service.Precondition{})
			return err
		},
		"DeleteObject": func() error { return r.svc.DeleteObject(ctx, "cal", "a.ics", service.Precondition{}) },
	} {
		if err := call(); !errors.Is(err, service.ErrWaitingForLogin) || exitcode.From(err) != exitcode.Blocked {
			t.Errorf("%s while waiting = %v, want ErrWaitingForLogin (exit 20)", name, err)
		}
	}
}

// Another process's login (a second Local on the same directories) is
// noticed by the poll: the wait between checks is LoginPoll, and serving
// starts at the first check after the login, the one that poll ends.
func TestServeNoticesALoginFromAnotherProcess(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	waits, fire := make(chan time.Duration, 4), make(chan time.Time)
	r.svc.SetLoginPollTimer(func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	})
	r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: 7 * time.Second})
	r.wantState(t, service.ServeWaitingForLogin)
	select {
	case d := <-waits:
		if d != 7*time.Second {
			t.Fatalf("the wait between login checks is %v, want LoginPoll (7s)", d)
		}
	case <-time.After(waitLimit):
		t.Fatal("Serve never waited for its poll")
	}
	other := r.open(t)
	if _, err := other.Login(t.Context(), r.req()); err != nil {
		t.Fatal(err)
	}
	checks := r.polls.Load()
	fire <- time.Now() // the poll ends
	r.wantState(t, service.ServeServing)
	if n := r.polls.Load() - checks; n != 1 {
		t.Errorf("serving started after %d checks once the poll ended, want exactly 1", n)
	}
	if p := r.wantPass(t); p.Err != nil {
		t.Errorf("first pass: %v", p.Err)
	}
}

// Proton ends the session and no unattended relogin is set: with
// WaitForLogin, Serve marks the login needs-login and waits instead of
// returning.
func TestSessionEndWaitsInsteadOfExiting(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour})
	r.wantState(t, service.ServeServing)
	r.wantPass(t)
	r.f.RevokeSessions()
	r.tick <- struct{}{}
	if p := r.wantPass(t); exitcode.From(p.Err) != exitcode.MustAct {
		t.Fatalf("pass after the revocation: %v, want a MustAct", p.Err)
	}
	r.wantState(t, service.ServeWaitingForLogin)
	if h := r.health(t); h.Serve != service.ServeWaitingForLogin {
		t.Errorf("Health.Serve = %q, want waiting", h.Serve)
	}
	if got := r.phase(t); got != "needs-login" {
		t.Errorf("phase = %q, want needs-login", got)
	}
	running(t, done)
	if _, err := r.svc.Collections(t.Context()); !errors.Is(err, service.ErrWaitingForLogin) {
		t.Errorf("Collections after the session end = %v, want ErrWaitingForLogin", err)
	}
}

// Without WaitForLogin, M3 stays: logged out is exit 10 with Ready never
// called, and a session end returns its MustAct.
func TestServeWithoutTheFlagKeepsM3(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	var readies atomic.Int32
	err := r.svc.Serve(t.Context(), service.ServeOptions{Ready: func() { readies.Add(1) }})
	if ee := wantExit(t, err, exitcode.MustAct); ee.Hint != "run pcb login" {
		t.Errorf("logged out: %v, hint %q", err, ee.Hint)
	}
	if n := readies.Load(); n != 0 {
		t.Fatalf("Ready called %d times while logged out", n)
	}

	r.signIn(t)
	done, _ := r.serve(t, service.ServeOptions{})
	r.wantState(t, service.ServeServing)
	r.wantPass(t)
	r.f.RevokeSessions()
	r.tick <- struct{}{}
	r.wantPass(t)
	select {
	case err := <-done:
		done <- err // for the cleanup's stop
		if ee := wantExit(t, err, exitcode.MustAct); ee.Hint != "run pcb login" {
			t.Errorf("session end: %v, hint %q", err, ee.Hint)
		}
	case <-time.After(waitLimit):
		t.Fatal("Serve without WaitForLogin did not return at the session end")
	}
	select {
	case s := <-r.states:
		t.Errorf("Serve without WaitForLogin reported %q after the session end", s)
	default:
	}
}

// Ruling M5-P10: Logout during Serve stops the loop and closes the cache
// before it wipes; Serve then waits (WaitForLogin) or returns MustAct.
func TestLogoutDuringServeClosesTheCacheFirst(t *testing.T) {
	t.Parallel()
	t.Run("waits", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.signIn(t)
		done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour})
		r.wantState(t, service.ServeServing)
		r.wantPass(t)
		var atWipe service.ServeState
		var dbAtWipe error
		r.svc.OnWipe(func() {
			h, err := r.svc.Health(context.Background())
			if err != nil {
				t.Errorf("Health at the wipe: %v", err)
			}
			atWipe = h.Serve
			_, dbAtWipe = os.Stat(cache.Path(r.dirs.State))
		})
		if _, err := r.svc.Logout(t.Context(), service.LogoutRequest{}); err != nil {
			t.Fatal(err)
		}
		if atWipe != service.ServeWaitingForLogin || dbAtWipe != nil {
			t.Errorf("at the wipe: Serve %q, pcb.db %v; want the loop stopped and the cache still there to wipe", atWipe, dbAtWipe)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if _, err := os.Stat(cache.Path(r.dirs.State) + suffix); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("pcb.db%s after Logout: %v, want absent", suffix, err)
			}
		}
		if h := r.health(t); h.Serve != service.ServeWaitingForLogin {
			t.Errorf("Health.Serve after Logout = %q, want waiting", h.Serve)
		}
		r.wantState(t, service.ServeWaitingForLogin)
		running(t, done)

		// The next login serves again.
		r.clk.advance(time.Minute)
		r.signIn(t)
		r.wantState(t, service.ServeServing)
		if p := r.wantPass(t); p.Err != nil {
			t.Errorf("pass after the new login: %v", p.Err)
		}
	})
	t.Run("returns without the flag", func(t *testing.T) {
		t.Parallel()
		r := newWaitRig(t)
		r.signIn(t)
		done, _ := r.serve(t, service.ServeOptions{})
		r.wantState(t, service.ServeServing)
		r.wantPass(t)
		if _, err := r.svc.Logout(t.Context(), service.LogoutRequest{}); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			done <- err // for the cleanup's stop
			if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "not logged in to Proton" {
				t.Errorf("Serve after Logout = %v", err)
			}
		case <-time.After(waitLimit):
			t.Fatal("Serve did not return after Logout")
		}
		if _, err := os.Stat(cache.Path(r.dirs.State)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("pcb.db after Logout: %v", err)
		}
	})
}

// Ruling M5-R8: under WaitForLogin, every stop a login fixes goes back to
// waiting: not logged in (another process logged out), the login marked
// needs-login or needs-captcha (by another process), the sealed secrets
// rolled back. (A session Proton ended: TestSessionEndWaitsInsteadOfExiting;
// a Logout in this process: TestLogoutDuringServeClosesTheCacheFirst.)
func TestLoginFixableStopsWait(t *testing.T) {
	t.Parallel()
	setPhase := func(t *testing.T, r *waitRig, phase store.Phase, captcha *store.Captcha) {
		t.Helper()
		if err := store.UpdateState(r.dirs.State, func(st *store.State) error {
			st.Phase, st.Reason, st.Captcha = phase, "set by another process", captcha
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, r *waitRig)
	}{
		{"not logged in", func(t *testing.T, r *waitRig) {
			if _, err := r.open(t).Logout(t.Context(), service.LogoutRequest{}); err != nil {
				t.Fatal(err)
			}
		}},
		{"needs login", func(t *testing.T, r *waitRig) { setPhase(t, r, store.PhaseNeedsLogin, nil) }},
		{"needs captcha", func(t *testing.T, r *waitRig) {
			setPhase(t, r, store.PhaseNeedsCaptcha, &store.Captcha{URL: "https://verify.example/x", Token: "tok"})
		}},
		{"secrets rolled back", func(t *testing.T, r *waitRig) { restoreOlderSecrets(t, r.env) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWaitRig(t)
			r.signIn(t)
			done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour})
			r.wantState(t, service.ServeServing)
			r.wantPass(t)
			tc.after(t, r)
			r.tick <- struct{}{}
			if p := r.wantPass(t); exitcode.From(p.Err) != exitcode.MustAct {
				t.Fatalf("pass after the change: %v, want a MustAct", p.Err)
			}
			r.wantState(t, service.ServeWaitingForLogin)
			running(t, done)
		})
	}
}

// Ruling M5-R8: a MustAct a login does not fix (a damaged cache) ends Serve
// with that error even under WaitForLogin (pcb serve exits 10): no waiting,
// no retry loop.
func TestDamagedCacheEndsServeUnderTheFlag(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache.Path(r.dirs.State), []byte("not a database, not at all, not even close........"), 0o600); err != nil {
		t.Fatal(err)
	}
	done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Millisecond})
	select {
	case err := <-done:
		done <- err // for the cleanup's stop
		if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "damaged") {
			t.Errorf("Serve = %v, want the damaged cache", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("Serve with a damaged cache never returned: it waits for a login a login cannot fix")
	}
	if n := r.polls.Load(); n != 1 {
		t.Errorf("%d login checks, want the first one only (no retry loop)", n)
	}
	for len(r.states) > 0 {
		if s := <-r.states; s == service.ServeWaitingForLogin {
			t.Error("Serve reported waiting for a login over a damaged cache")
		}
	}
}

// Ruling M5-R9: after a logout and a new login, Serve opens a fresh cache;
// until its first pass gets through (here the calendar listing fails and
// waits for its retry), a calendar Thunderbird knows is pending (503),
// never missing (404). Once the pass got through, an unknown calendar is
// missing again.
func TestUnknownCalendarIsPendingUntilTheFirstPassAfterALogin(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour})
	r.wantState(t, service.ServeServing)
	r.wantPass(t)
	if _, err := r.svc.Logout(t.Context(), service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	r.wantState(t, service.ServeWaitingForLogin)
	r.f.FailNext(http.MethodGet, "/calendar/v1", http.StatusServiceUnavailable, 0, 0)
	r.clk.advance(time.Minute)
	r.signIn(t)
	r.wantState(t, service.ServeServing) // the pass waits in the listing's retry (tick)
	_, _, err := r.svc.Objects(t.Context(), r.work)
	if !errors.Is(err, service.ErrCalendarPending) || exitcode.From(err) != exitcode.Blocked {
		t.Fatalf("Objects(Work) before the first pass = %v, want ErrCalendarPending (503), never a 4xx", err)
	}
	r.tick <- struct{}{}
	if p := r.wantPass(t); p.Err != nil {
		t.Fatalf("pass after the retry: %v", p.Err)
	}
	if _, _, err := r.svc.Objects(t.Context(), r.work); err != nil {
		t.Errorf("Objects(Work) after the pass = %v", err)
	}
	if _, _, err := r.svc.Objects(t.Context(), "no-such-calendar"); !errors.Is(err, service.ErrNoSuchCalendar) {
		t.Errorf("Objects(unknown) after the pass = %v, want ErrNoSuchCalendar (404)", err)
	}
}

// holdWipe makes the next Logout of svc stop just before its wipe: it
// returns once that Logout is there, and release lets it go on and
// returns its error.
func holdWipe(t *testing.T, svc *service.Local) (release func() error) {
	t.Helper()
	in, out, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	svc.OnWipe(func() {
		once.Do(func() {
			close(in)
			<-out
		})
	})
	go func() {
		_, err := svc.Logout(context.Background(), service.LogoutRequest{})
		done <- err
	}()
	select {
	case <-in:
	case <-time.After(waitLimit):
		t.Fatal("Logout never reached its wipe")
	}
	var rel sync.Once
	var err error
	release = func() error {
		rel.Do(func() {
			close(out)
			err = <-done
		})
		return err
	}
	t.Cleanup(func() { _ = release() })
	return release
}

// noServing fails the test if Serve reported serving.
func (r *waitRig) noServing(t *testing.T) {
	t.Helper()
	for len(r.states) > 0 {
		if s := <-r.states; s == service.ServeServing {
			t.Error("Serve served during a logout")
		}
	}
}

// M5-P10: a Serve (no flag) that starts while a Logout wipes refuses to
// serve, even though the stored login still reads ready: no cache opened.
func TestServeRefusesWhileALogoutWipes(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	release := holdWipe(t, r.svc)
	done, _ := r.serve(t, service.ServeOptions{})
	select {
	case err := <-done:
		done <- err
		if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "not logged in to Proton" {
			t.Errorf("Serve during the wipe = %v", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("Serve served while a logout wiped")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	r.noServing(t)
	if _, err := os.Stat(cache.Path(r.dirs.State)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pcb.db after the logout: %v, want absent", err)
	}
}

// M5-P10: a whole Logout that lands between the login check and the
// opening of the cache refuses serving: no empty pcb.db outlives it.
func TestLogoutBetweenTheCheckAndServingRefusesIt(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	var once sync.Once
	r.svc.BeforeServe(func() {
		once.Do(func() {
			if _, err := r.svc.Logout(context.Background(), service.LogoutRequest{}); err != nil {
				t.Errorf("Logout: %v", err)
			}
		})
	})
	done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour})
	r.wantState(t, service.ServeWaitingForLogin)
	r.noServing(t)
	running(t, done)
	if _, err := os.Stat(cache.Path(r.dirs.State)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("pcb.db after the logout: %v, want absent", err)
	}
}

// M5-P10: while a Logout wipes, a waiting Serve does not trust the stored
// login (still ready until the wipe): it never tries to serve it.
func TestWaitIgnoresTheLoginWhileALogoutWipes(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	checks := make(chan struct{}, 64)
	r.svc.OnLoginCheck(func() {
		r.polls.Add(1)
		select {
		case checks <- struct{}{}:
		default:
		}
	})
	var tries atomic.Int32
	r.svc.BeforeServe(func() { tries.Add(1) })
	release := holdWipe(t, r.svc)
	done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Millisecond})
	r.wantState(t, service.ServeWaitingForLogin)
	for range 3 {
		select {
		case <-checks:
		case <-time.After(waitLimit):
			t.Fatal("the wait stopped checking")
		}
	}
	if n := tries.Load(); n != 0 {
		t.Errorf("Serve tried to serve %d times while the logout wiped", n)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	r.noServing(t)
	running(t, done)
}

// M5-P10: when a stop leaves Serve waiting, the wait begins while Serve
// still holds its handle (a read is refused, never opens its own), then
// the handle goes.
func TestWaitBeginsBeforeTheHandleIsCleared(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	var held atomic.Int32 // 1: held at the wait, 2: not held
	served := false
	r.atState = func(s service.ServeState) {
		switch {
		case s == service.ServeServing:
			served = true
		case served && held.Load() == 0:
			if r.svc.ServingHandle() {
				held.Store(1)
			} else {
				held.Store(2)
			}
		}
	}
	r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour})
	r.wantState(t, service.ServeServing)
	r.wantPass(t)
	if _, err := r.svc.Logout(t.Context(), service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	r.wantState(t, service.ServeWaitingForLogin)
	if held.Load() != 1 {
		t.Error("the wait began after Serve cleared its handle: a read could open its own meanwhile")
	}
	if r.svc.ServingHandle() {
		t.Error("Serve still holds its handle while it waits")
	}
}

// Ruling M5-R10: a stored login no login fixes ends Serve under
// WaitForLogin, exit 10 with its own hint, never "run pcb login".
func TestUnfixableStoredLoginEndsServe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, hint string
		setup      func(t *testing.T, r *waitRig)
	}{
		{"state file unreadable", "run pcb logout to reset it", func(t *testing.T, r *waitRig) {
			if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.dirs.State, "state.json"), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"key file missing", "restore it from a backup", func(t *testing.T, r *waitRig) {
			r.signIn(t)
			if err := os.Remove(r.cfg.Secrets.KeyFile); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWaitRig(t)
			tc.setup(t, r)
			done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Millisecond})
			select {
			case err := <-done:
				done <- err
				if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, tc.hint) || ee.Hint == "run pcb login" {
					t.Errorf("Serve = %v, hint %q; want exit 10 with %q", err, ee.Hint, tc.hint)
				}
			case <-time.After(waitLimit):
				t.Fatal("Serve waits for a login a login cannot fix")
			}
			for len(r.states) > 0 {
				if s := <-r.states; s == service.ServeWaitingForLogin {
					t.Error("Serve reported waiting for a login")
				}
			}
		})
	}
}

// Ruling M5-R10: under WaitForLogin, another process switching to another
// account between two passes sends Serve back to waiting, then it serves
// the new account (without the flag: exit 20, as before).
func TestAccountSwitchWaitsThenServesTheNewAccount(t *testing.T) {
	t.Parallel()
	r := newWaitRig(t)
	r.signIn(t)
	done, _ := r.serve(t, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Millisecond})
	r.wantState(t, service.ServeServing)
	r.wantPass(t)
	other := r.open(t)
	if _, err := other.Logout(t.Context(), service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	r.f.AddUser("other", "other-pass")
	r.clk.advance(time.Minute)
	if _, err := other.Login(t.Context(), service.LoginRequest{Username: "other", Password: "other-pass"}); err != nil {
		t.Fatal(err)
	}
	r.tick <- struct{}{}
	if p := r.wantPass(t); exitcode.From(p.Err) != exitcode.Blocked || !strings.Contains(p.Err.Error(), "another Proton account") {
		t.Fatalf("pass after the switch: %v, want the account switch", p.Err)
	}
	r.wantState(t, service.ServeWaitingForLogin)
	r.wantState(t, service.ServeServing)
	if p := r.wantPass(t); p.Err != nil {
		t.Errorf("first pass of the new account: %v", p.Err)
	}
	running(t, done)
	if st := r.loadState(t); st.AccountID != r.f.UserID("other") {
		t.Errorf("state.json account %q, want the other one", st.AccountID)
	}
}
