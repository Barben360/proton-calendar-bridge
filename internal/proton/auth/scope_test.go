package auth_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

const saltsPath = "/core/v4/keys/salts"

func lockedScope(t *testing.T, o protontest.Options) (*protontest.Fake, auth.Scope) {
	t.Helper()
	o.LockedSalts = true
	f := protontest.New(t, o)
	m := manager(t, f)
	p := begin(t, f, m)
	return f, auth.Scope{M: m, C: client(f, p.Tokens), Username: f.Username, Password: []byte(f.Password)}
}

func saltsCode(t *testing.T, c *api.Client) int {
	t.Helper()
	var out struct{ Code int }
	err := c.Get(t.Context(), saltsPath, nil, &out)
	var e *api.Error
	switch {
	case err == nil:
		return out.Code
	case errors.As(err, &e):
		return e.Code
	}
	t.Fatalf("GET salts: %v", err)
	return 0
}

func TestScopeUnlockThenSaltsThenLock(t *testing.T) {
	t.Parallel()
	_, s := lockedScope(t, protontest.Options{})
	if code := saltsCode(t, s.C); code != api.CodeInsufficientScope {
		t.Fatalf("salts before Unlock: code %d, want %d", code, api.CodeInsufficientScope)
	}
	if err := s.Unlock(t.Context()); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if code := saltsCode(t, s.C); code != api.CodeOK {
		t.Fatalf("salts after Unlock: code %d, want %d", code, api.CodeOK)
	}
	if err := s.Lock(t.Context()); err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if code := saltsCode(t, s.C); code != api.CodeInsufficientScope {
		t.Fatalf("salts after Lock: code %d, want %d", code, api.CodeInsufficientScope)
	}
}

func TestScopeUnlockRejectsBadServerProof(t *testing.T) {
	t.Parallel()
	f, s := lockedScope(t, protontest.Options{BadUnlockProof: true})
	if err := s.Unlock(t.Context()); !errors.Is(err, auth.ErrServerProof) {
		t.Fatalf("Unlock = %v, want ErrServerProof", err)
	}
	// The scope a lying server may have granted is dropped again.
	if n := f.Calls("PUT", "/core/v4/users/lock"); n != 1 {
		t.Fatalf("PUT /core/v4/users/lock calls = %d, want 1", n)
	}
	if code := saltsCode(t, s.C); code != api.CodeInsufficientScope {
		t.Fatalf("salts after a refused proof: code %d, want %d", code, api.CodeInsufficientScope)
	}
}

func TestScopeUnlockWrongPasswordIsBadCredentials(t *testing.T) {
	t.Parallel()
	_, s := lockedScope(t, protontest.Options{})
	wrong := "wrong-" + string(s.Password)
	s.Password = []byte(wrong)
	err := s.Unlock(t.Context())
	if !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("Unlock with a wrong password = %v, want ErrBadCredentials", err)
	}
	if strings.Contains(err.Error(), wrong) {
		t.Fatalf("the error text contains the password: %v", err)
	}
	if code := saltsCode(t, s.C); code != api.CodeInsufficientScope {
		t.Fatalf("salts after a refused unlock: code %d, want %d", code, api.CodeInsufficientScope)
	}
}

func TestScopeUnlockEmptyServerProofFailsClosed(t *testing.T) {
	t.Parallel()
	_, s := lockedScope(t, protontest.Options{EmptyUnlockProof: true})
	if err := s.Unlock(t.Context()); !errors.Is(err, auth.ErrServerProof) {
		t.Fatalf("Unlock without a server proof = %v, want ErrServerProof", err)
	}
	if code := saltsCode(t, s.C); code != api.CodeInsufficientScope {
		t.Fatalf("salts after a missing proof: code %d, want %d", code, api.CodeInsufficientScope)
	}
}

func TestScopeUnlockReportsAFailedRelock(t *testing.T) {
	t.Parallel()
	f, s := lockedScope(t, protontest.Options{BadUnlockProof: true})
	f.FailNext("PUT", "/core/v4/users/lock", http.StatusInternalServerError, 0, 0)
	err := s.Unlock(t.Context())
	var e *api.Error
	if !errors.Is(err, auth.ErrServerProof) || !errors.As(err, &e) || e.Status != http.StatusInternalServerError {
		t.Fatalf("Unlock = %v, want ErrServerProof joined with the failed re-lock (HTTP 500)", err)
	}
}

func TestScopeUnlockBadCredentialsOnlyOn422With8002(t *testing.T) {
	t.Parallel()
	f, s := lockedScope(t, protontest.Options{})
	for _, c := range []struct{ status, code int }{
		{http.StatusUnprocessableEntity, 2001},
		{http.StatusBadRequest, protontest.CodePasswordWrong},
		{http.StatusForbidden, protontest.CodePasswordWrong},
	} {
		f.FailNext("PUT", "/core/v4/users/unlock", c.status, c.code, 0)
		if err := s.Unlock(t.Context()); err == nil || errors.Is(err, auth.ErrBadCredentials) {
			t.Errorf("unlock answered %d/%d: err = %v, want a plain error", c.status, c.code, err)
		}
	}
}

// A 429 on the unlock PUT comes back at once with its Retry-After: waiting
// it out inside the call would run into CallTimeout (30 s) for a 31 to 60 s
// ask and lose the rate-limit class (issue 45).
func TestScopeUnlockRateLimitKeepsRetryAfter(t *testing.T) {
	t.Parallel()
	f, s := lockedScope(t, protontest.Options{})
	var slept []time.Duration
	s.C = api.New(s.C.Tokens(), api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		Sleep:   func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil },
	})
	f.FailNext("PUT", "/core/v4/users/unlock", http.StatusTooManyRequests, 85131, 45)
	err := s.Unlock(t.Context())
	var e *api.Error
	if !errors.As(err, &e) || e.Status != http.StatusTooManyRequests || e.RetryAfter != 45*time.Second {
		t.Fatalf("Unlock = %v, want the 429 with Retry-After 45s", err)
	}
	if len(slept) != 0 {
		t.Fatalf("Unlock waited %v inside its call; it must return the 429", slept)
	}
}
