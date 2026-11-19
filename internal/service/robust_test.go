package service_test

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
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
	e.signIn(t)
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
	e.signIn(t)
	e.f.FailNext(http.MethodGet, "/calendar/v1", http.StatusUnprocessableEntity, api.CodeHumanVerification, 0)
	_, err := e.svc.Calendars(t.Context())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Err.Error() != "Proton asks for a human verification (code 9001)" ||
		ee.Hint != "open the Proton web app from this machine's network and complete the verification" {
		t.Fatalf("message %q, hint %q", ee.Err, ee.Hint)
	}
}

// A login whose call hits its deadline (Proton accepts the connection and
// never answers) is Blocked, exit 20: never a wrong password (proton-I1).
// Not parallel: it shortens auth.CallTimeout.
func TestLoginDeadlineIsBlocked(t *testing.T) {
	old := auth.CallTimeout
	auth.CallTimeout = time.Second
	t.Cleanup(func() { auth.CallTimeout = old })
	e := newEnv(t, protontest.Options{})
	// Never answered until the test ends: without the call deadline the login
	// would never return.
	h := e.f.HoldNext(http.MethodPost, "/auth/v4/info")
	t.Cleanup(h.Release)
	err := within(t, func() error { _, err := e.svc.Login(t.Context(), e.req()); return err })
	if err == nil {
		t.Fatal("a stalled login succeeded")
	}
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Err.Error() != "the operation was cancelled or timed out" || ee.Hint != "run the command again" {
		t.Fatalf("message %q, hint %q; want the timeout", ee.Err, ee.Hint)
	}
}

// within runs f and fails, instead of hanging, if it has not returned in 30 s.
func within(t *testing.T, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second): // a hang guard, not a deadline
		t.Fatal("the call has no deadline: it is still waiting for the held request")
		return nil
	}
}

// A login 429 with a long Retry-After extends the guard: the next login
// within the window is refused as too soon and names what is left, and is
// allowed once it is over (issue 45).
func TestLoginRateLimitExtendsTheGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.FailNext(http.MethodPost, "/auth/v4", http.StatusTooManyRequests, 85131, 300)
	if _, err := e.svc.Login(t.Context(), e.req()); err == nil {
		t.Fatal("the rate-limited login succeeded")
	}
	e.clk.advance(61 * time.Second)
	_, err := e.svc.Login(t.Context(), e.req())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Hint != "retry in 3m59s" {
		t.Fatalf("second login: %v (hint %q), want refused with retry in 3m59s", ee.Err, ee.Hint)
	}
	e.clk.advance(240 * time.Second)
	if res := e.login(t, e.req()); res.State == "" {
		t.Fatalf("login after the Retry-After = %+v", res)
	}
}

// The same for an unattended relogin: a CalDAV request or a sync pass does
// not retry the login before Proton's Retry-After is over.
func TestReloginRateLimitExtendsTheGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.loginUnattended(t, false)
	e.f.RevokeSessions()
	e.clk.advance(61 * time.Second)
	e.f.FailNext(http.MethodPost, "/auth/v4", http.StatusTooManyRequests, 85131, 300)
	if _, err := e.svc.Calendars(t.Context()); err == nil {
		t.Fatal("the request with a rate-limited relogin succeeded")
	}
	srp := e.logins()
	e.clk.advance(61 * time.Second)
	_, err := e.svc.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.Blocked); ee.Hint != "retry in 3m59s" {
		t.Fatalf("second request: hint %q, want retry in 3m59s", ee.Hint)
	}
	if n := e.logins() - srp; n != 0 {
		t.Fatalf("%d login(s) reached Proton inside the Retry-After", n)
	}
	e.clk.advance(240 * time.Second)
	if got := e.calendars(t); got == nil && e.phase(t) != "ready" {
		t.Fatalf("after the Retry-After the relogin did not repair the session (phase %q)", e.phase(t))
	}
}
