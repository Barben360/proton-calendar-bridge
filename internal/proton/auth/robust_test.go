package auth_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// stallBound is how long the fake holds a stalled request at most: past it
// the request is answered, so a call with no deadline of its own succeeds
// (below the test API client's own 5 s timeout).
const stallBound = 3 * time.Second

// TestLoginCallsHaveADeadline: a Proton that accepts the connection and
// never answers does not hold a login call past its own deadline, even when
// the caller's context has none (ruling A1). Not parallel: it shortens the
// package's deadline.
func TestLoginCallsHaveADeadline(t *testing.T) {
	cases := []struct {
		name         string
		method, path string
		call         func(t *testing.T) func() error
	}{
		{"begin", http.MethodPost, "/auth/v4/info", func(t *testing.T) func() error {
			f := protontest.New(t, protontest.Options{})
			m := manager(t, f)
			return func() error {
				f.StallNext(http.MethodPost, "/auth/v4/info", stallBound)
				p, err := auth.Begin(context.Background(), m, f.Username, []byte(f.Password), "")
				if p != nil {
					p.Close()
				}
				return err
			}
		}},
		{"2fa", http.MethodPost, "/auth/v4/2fa", func(t *testing.T) func() error {
			f, tp := totpAccount(t)
			p := begin(t, f, manager(t, f))
			return func() error {
				f.StallNext(http.MethodPost, "/auth/v4/2fa", stallBound)
				return p.SubmitTOTP(context.Background(), tp.Code(t0))
			}
		}},
		{"unlock auth info", http.MethodPost, "/auth/v4/info", func(t *testing.T) func() error {
			f, s := lockedScope(t, protontest.Options{})
			return func() error {
				f.StallNext(http.MethodPost, "/auth/v4/info", stallBound)
				return s.Unlock(context.Background())
			}
		}},
		{"unlock proof", http.MethodPut, "/core/v4/users/unlock", func(t *testing.T) func() error {
			f, s := lockedScope(t, protontest.Options{})
			return func() error {
				f.StallNext(http.MethodPut, "/core/v4/users/unlock", stallBound)
				return s.Unlock(context.Background())
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := tc.call(t) // the setup logs in with the normal deadline
			setCallTimeout(t, time.Second)
			err := run()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s %s stalled: error = %v, want the call's deadline exceeded", tc.method, tc.path, err)
			}
		})
	}
}

// TestBeginOther422IsNotBadCredentials: only code 8002 is a wrong
// password; another 422 keeps Proton's code (ruling: as Scope.wrongPassword).
func TestBeginOther422IsNotBadCredentials(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	f.FailNext(http.MethodPost, "/auth/v4", http.StatusUnprocessableEntity, 2001, 0)
	_, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "")
	if errors.Is(err, auth.ErrBadCredentials) || !api.IsCode(err, 2001) {
		t.Fatalf("Begin = %v; want Proton's code 2001, not ErrBadCredentials", err)
	}
}

// TestBeginKeepsRetryAfter: a rate-limited login carries Proton's
// Retry-After, in seconds or as an HTTP date.
func TestBeginKeepsRetryAfter(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := manager(t, f)
	retryAfter := func() time.Duration {
		t.Helper()
		_, err := auth.Begin(t.Context(), m, f.Username, []byte(f.Password), "")
		var e *api.Error
		if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests {
			t.Fatalf("Begin = %v, want the 429", err)
		}
		return e.RetryAfter
	}
	f.FailNext(http.MethodPost, "/auth/v4", http.StatusTooManyRequests, 85131, 120)
	if got := retryAfter(); got != 120*time.Second {
		t.Errorf("Retry-After: 120 gave %v, want 2m0s", got)
	}
	f.FailNextRetryAt(http.MethodPost, "/auth/v4", http.StatusTooManyRequests, 85131, time.Now().Add(300*time.Second))
	if got := retryAfter(); got < 290*time.Second || got > 300*time.Second {
		t.Errorf("Retry-After as a date 300 s ahead gave %v", got)
	}
}

// setCallTimeout shortens auth.CallTimeout for t. t must not run in
// parallel.
func setCallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := auth.CallTimeout
	auth.CallTimeout = d
	t.Cleanup(func() { auth.CallTimeout = old })
}
