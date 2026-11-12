package service_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
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

	e.login(t, e.req())
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
}

// atPass is a pass and the fake time its callback ran at.
type atPass struct {
	service.WatchPass
	At time.Time
}

func newIdleRig(t *testing.T) *idleRig {
	t.Helper()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	e.login(t, e.req())
	r := &idleRig{env: e, idle: make(chan struct{}), passes: make(chan atPass), resume: make(chan struct{})}
	svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: func(ctx context.Context, d time.Duration) error {
		if d == service.IdleInterval {
			select {
			case r.idle <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			<-ctx.Done()
			return ctx.Err()
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
