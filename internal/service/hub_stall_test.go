package service_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Live 2026-11-13 (HUMAN-CHECKS 7.1.5–7.1.6), on the path pcb serve runs
// since M6: the Hub's loop of an account takes over after a login, and the
// HTTP/2 connection the login's key calls opened stops answering; every
// pass timed out on it again until a restart dialled a new one. Here the
// fake is behind a TLS HTTP/2 front whose first connection stops answering
// once the account's loop found the login ready, before its first pass:
// the first passes must get through on a new connection. Both handovers:
// the first login of a pcb serve --wait-for-login with no account (the
// login adds the account and its loop), and the login of a signed-out
// account whose loop waits for one.
func TestHubServeAfterALoginLeavesAConnectionThatStoppedAnswering(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		signedOut bool
	}{
		{"the login adds the account", false},
		{"the login wakes the account's waiting loop", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newHubEnv(t)
			e.f.AddCalendar(userA, protontest.Calendar{Name: "A"})
			front := protontest.Front(t, e.f)
			tr := front.Transport()
			t.Cleanup(tr.CloseIdleConnections)
			e.cfg.Proton.BaseURL = front.URL
			e.http = &http.Client{Timeout: 300 * time.Millisecond, Transport: tr}
			e.sleep = func(ctx context.Context, _ time.Duration) error {
				select {
				case <-time.After(20 * time.Millisecond):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			h := e.open(t)
			e.skip(t, h)
			id := ""
			if c.signedOut {
				id = e.mustLogin(t, h, userA).Account
				if _, err := h.SignOut(t.Context(), id, service.LogoutRequest{}); err != nil {
					t.Fatalf("SignOut: %v", err)
				}
			}
			h.AlsoOnOpen(func(l *service.Local) { l.BeforeServe(front.Stall) })
			r := newServeRig(h)
			r.serve(t, r.options(true))
			if c.signedOut {
				r.waitFor(t, "the signed-out account's loop waiting for a login", func() bool { return r.last(id) == service.ServeWaitingForLogin })
			}
			got := e.mustLogin(t, h, userA).Account
			if id != "" && got != id {
				t.Fatalf("the login reached account %s, want the signed-out %s", got, id)
			}
			id = got
			r.waitFor(t, "a pass that gets through, or two that fail", func() bool {
				return r.okPasses(id) > 0 || len(r.passes[id]) >= 2
			})
			r.mu.Lock()
			passes := append([]service.WatchPass(nil), r.passes[id]...)
			r.mu.Unlock()
			var errs []error
			for _, p := range passes[:min(2, len(passes))] {
				if p.Err == nil {
					if front.Hung() == 0 || front.Conns() < 2 {
						t.Fatalf("a pass got through with %d requests hung on %d connections: the stall did not happen", front.Hung(), front.Conns())
					}
					return
				}
				errs = append(errs, p.Err)
			}
			t.Fatalf("the first two passes after the login failed (%v), %d requests hung on the connection that stopped answering, %d connections",
				errs, front.Hung(), front.Conns())
		})
	}
}
