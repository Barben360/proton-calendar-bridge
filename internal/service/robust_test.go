package service_test

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// A rate-limited login says how long Proton asked to wait (proton-M2).
func TestLoginRateLimitedNamesRetryAfter(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.FailNext(http.MethodPost, "/auth/v4", http.StatusTooManyRequests, 85131, 120)
	_, err := e.svc.Login(t.Context(), e.req())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Err.Error() != "Proton is rate limiting requests" || ee.Hint != "retry in 2m0s" {
		t.Fatalf("message %q, hint %q; want the rate limit and retry in 2m0s", ee.Err, ee.Hint)
	}
}

// A 422 at login with another code than 8002 is no wrong password: it is
// Proton refusing, not a MustAct (proton-I2).
func TestLoginOther422IsNotWrongPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.FailNext(http.MethodPost, "/auth/v4", http.StatusUnprocessableEntity, 2001, 0)
	_, err := e.svc.Login(t.Context(), e.req())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Err.Error() != "Proton refused the request (code 2001)" {
		t.Fatalf("message %q, want Proton's refusal with its code", ee.Err)
	}
}

// A person's login that meets a CAPTCHA while the stored login is ready
// leaves state.json as it was: the working session keeps serving
// (state-I4).
func TestLoginCaptchaWhileReadyKeepsState(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	before := e.loadState(t)
	e.f.SetCaptcha("chal-ready")
	e.clk.advance(61 * time.Second)
	res := e.login(t, e.req())
	if res.State != service.StateNeedsCaptcha || res.CaptchaToken != "chal-ready" {
		t.Fatalf("result = %+v, want the CAPTCHA", res)
	}
	if after := e.loadState(t); !reflect.DeepEqual(after, before) {
		t.Fatalf("state.json changed on a CAPTCHA:\n before %+v\n after  %+v", before, after)
	}
	if _, err := e.svc.Calendars(t.Context()); err != nil {
		t.Fatalf("the stored session stopped working: %v", err)
	}
}

// The key passphrase a login derives is zeroed once sealed (proton-M3).
func TestLoginZeroesTheKeyPassphrase(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	var seen []byte
	e.svc.SetKeyPassHook(func(kp []byte) { seen = kp })
	e.login(t, e.req())
	if len(seen) == 0 {
		t.Fatal("the hook saw no key passphrase")
	}
	for _, b := range seen {
		if b != 0 {
			t.Fatal("the key passphrase is still in memory after the login")
		}
	}
	if len(e.account(t).KeyPass) == 0 {
		t.Fatal("the sealed account lost its key passphrase")
	}
}

// Proton asking for a human verification on an authenticated read says so,
// with the step that clears it (proton-M6).
func TestReadHumanVerificationIsTyped(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.f.FailNext(http.MethodGet, "/calendar/v1", http.StatusUnprocessableEntity, api.CodeHumanVerification, 0)
	_, err := e.svc.Calendars(t.Context())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Err.Error() != "Proton asks for a human verification (code 9001)" ||
		ee.Hint != "open the Proton web app from this machine's network and complete the verification" {
		t.Fatalf("message %q, hint %q", ee.Err, ee.Hint)
	}
}
