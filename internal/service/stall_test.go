package service_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Live 2026-11-13 (HUMAN-CHECKS 7.1.5–7.1.6): pcb serve --wait-for-login
// took a login, and the HTTP/2 connection the login's key calls opened
// stopped answering; every pass timed out on it again (95 s, then 167 s)
// until a restart dialled a new one. Here the fake is behind a TLS HTTP/2
// front whose first connection stops answering once the login is done:
// the first passes after the login must get through on a new connection.
func TestServeAfterALoginLeavesAConnectionThatStoppedAnswering(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "Work"})
	front := protontest.Front(t, e.f)
	tr := front.Transport()
	t.Cleanup(tr.CloseIdleConnections)
	cfg := e.cfg
	cfg.Proton.BaseURL = front.URL
	svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: cfg, Now: e.clk.Now,
		HTTP: &http.Client{Timeout: 300 * time.Millisecond, Transport: tr},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-time.After(20 * time.Millisecond):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	e.svc = svc
	var checks atomic.Int32
	svc.OnLoginCheck(func() {
		// The first check finds no login; the second is the one the
		// login's wake starts, after its key calls and before the first pass.
		if checks.Add(1) >= 2 {
			front.Stall()
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	passes, done := make(chan service.WatchPass, 64), make(chan error, 1)
	waiting := make(chan struct{}, 1)
	go func() {
		done <- svc.Serve(ctx, service.ServeOptions{WaitForLogin: true, LoginPoll: time.Hour,
			OnPass: func(p service.WatchPass) { passes <- p },
			OnState: func(s service.ServeState) {
				if s == service.ServeWaitingForLogin {
					waiting <- struct{}{}
				}
			}})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve after its context ended = %v, want nil", err)
		}
	})
	select {
	case <-waiting: // the first check found no login
	case <-time.After(waitLimit):
		t.Fatal("Serve never waited for a login")
	}
	e.signIn(t)

	var errs []error
	for len(errs) < 2 {
		select {
		case p := <-passes:
			if p.Err == nil {
				if front.Hung() == 0 || front.Conns() < 2 {
					t.Fatalf("a pass got through with %d requests hung on %d connections: the stall did not happen", front.Hung(), front.Conns())
				}
				return
			}
			errs = append(errs, p.Err)
		case <-time.After(waitLimit):
			t.Fatalf("no pass after the login (failed so far: %v)", errs)
		}
	}
	t.Fatalf("the first two passes after the login failed (%v), %d requests hung on the connection that stopped answering, %d connections",
		errs, front.Hung(), front.Conns())
}
