package auth_test

import (
	"errors"
	"strings"
	"testing"

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
