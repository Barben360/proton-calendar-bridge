package service_test

import (
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// heldAtTOTP is an env whose login waits for its 2FA code.
func heldAtTOTP(t *testing.T) *env {
	t.Helper()
	return heldAtTOTPWith(t, protontest.Options{TwoFA: true})
}

// heldAtTOTPWith is heldAtTOTP over a fake built with o.
func heldAtTOTPWith(t *testing.T, o protontest.Options) *env {
	t.Helper()
	e := newEnv(t, o)
	if res := e.login(t, e.req()); res.State != service.StateNeedsTOTP {
		t.Fatalf("Login = %+v, want needs-totp", res)
	}
	return e
}

// failUsers makes GET /core/v4/users answer 502 for all the attempts the
// client makes in one call.
func failUsers(e *env) {
	for range 3 {
		e.f.FailNext(http.MethodGet, "/core/v4/users", http.StatusBadGateway, 0, 0)
	}
}

// A login the guard refuses (a retry 10 s after the first) must not revoke the
// login still waiting for its code: its code still completes it.
func TestRefusedSecondLoginKeepsTheHeldLogin(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	e.clk.advance(10 * time.Second)
	_, err := e.svc.Login(t.Context(), e.req())
	if ee := wantExit(t, err, exitcode.Blocked); ee.Hint != "retry in 50s" {
		t.Fatalf("second login: hint %q, want retry in 50s", ee.Hint)
	}
	if p := e.phase(t); p != string(service.StateNeedsTOTP) {
		t.Fatalf("phase after the refusal = %q, want the login still waiting for its code", p)
	}
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitTOTP = %+v, %v; want ready", res, err)
	}
}

// Once the guard passes, a new login still replaces the held one.
func TestSecondLoginAfterTheGuardReplacesTheHeldLogin(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	e.clk.advance(61 * time.Second)
	if res := e.login(t, e.req()); res.State != service.StateNeedsTOTP {
		t.Fatalf("second Login = %+v, want a new needs-totp", res)
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 2 {
		t.Fatalf("%d POST /auth/v4, want 2 (the held login replaced, not reused)", n)
	}
}

// A CAPTCHA token already sent to Proton earns no 10 s spacing: retried after
// a failed attempt, it waits the minute like any login.
func TestSpentCaptchaTokenWaitsTheMinute(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fresh bool // the retry runs in a new process: only state.json remembers the token
	}{{"same process", false}, {"new process", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, protontest.Options{Captcha: "chal-spent"})
			res := e.login(t, e.req())
			r := e.req()
			r.CaptchaToken = res.CaptchaToken
			e.clk.advance(30 * time.Second)
			e.f.FailNext(http.MethodPost, "/auth/v4/info", http.StatusBadGateway, 0, 0)
			if _, err := e.svc.Login(t.Context(), r); err == nil {
				t.Fatal("the attempt on a failing Proton succeeded")
			}
			svc := e.svc
			if tc.fresh {
				svc = e.open(t)
			}
			e.clk.advance(11 * time.Second)
			e.clk.slept = nil
			_, _ = svc.Login(t.Context(), r)
			if len(e.clk.slept) != 1 || e.clk.slept[0] != 49*time.Second {
				t.Errorf("Sleep calls = %v, want [49s]: a token already sent is spaced by the minute", e.clk.slept)
			}
		})
	}
}

// A 5xx on the user fetch after a valid 2FA code keeps the login held: the
// code is not sent again and no new SRP runs.
func TestTransientErrorAfterSRPKeepsTheLoginHeld(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	srp := e.logins()
	failUsers(e)
	_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if ee := wantExit(t, err, exitcode.Blocked); ee.Hint == "" {
		t.Fatalf("error %v has no hint", err)
	}
	if p := e.phase(t); p != string(service.StateNeedsTOTP) {
		t.Fatalf("phase after the 502 = %q, want the login still held", p)
	}
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if err != nil || res.State != service.StateReady {
		t.Fatalf("retry = %+v, %v; want ready", res, err)
	}
	if n := e.f.Calls("POST", "/auth/v4/2fa"); n != 1 {
		t.Errorf("%d 2FA submits, want 1: the accepted code is not spent twice", n)
	}
	if n := e.logins() - srp; n != 0 {
		t.Errorf("%d new SRP attempts, want none", n)
	}
}

// After maxRetries transient errors the login is cancelled as before.
func TestTransientErrorsEndTheLoginAfterThreeTries(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	for i := range 3 {
		failUsers(e)
		_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
		_ = wantExit(t, err, exitcode.Blocked)
		held := e.phase(t) == string(service.StateNeedsTOTP)
		if want := i < 2; held != want {
			t.Fatalf("after failure %d: held = %v, want %v", i+1, held, want)
		}
	}
	if n := e.f.Calls(http.MethodDelete, "/auth/v4"); n != 1 {
		t.Errorf("%d session revocations, want 1: the cancelled login's half session is revoked", n)
	}
}

// A validation failure that is not transient (a refused session) still ends
// the login at once.
func TestNonTransientErrorAfterSRPEndsTheLogin(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	e.f.FailNext(http.MethodGet, "/core/v4/users", http.StatusForbidden, 0, 0)
	if _, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now())); err == nil {
		t.Fatal("SubmitTOTP succeeded on a 403")
	}
	if p := e.phase(t); p == string(service.StateNeedsTOTP) {
		t.Fatalf("phase = %q after a 403, want the login ended", p)
	}
}

// A 429 on the 2FA submit records Proton's wait like one on the login does.
func TestTOTPRateLimitExtendsTheGuard(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	e.f.FailNext(http.MethodPost, "/auth/v4/2fa", http.StatusTooManyRequests, 85131, 300)
	if _, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now())); err == nil {
		t.Fatal("SubmitTOTP succeeded on a 429")
	}
	srp := e.logins()
	e.clk.advance(61 * time.Second)
	_, err := e.svc.Login(t.Context(), e.req())
	if ee := wantExit(t, err, exitcode.Blocked); ee.Hint != "retry in 3m59s" {
		t.Fatalf("login after the 429: hint %q, want retry in 3m59s", ee.Hint)
	}
	if n := e.logins() - srp; n != 0 {
		t.Fatalf("%d login(s) reached Proton inside the Retry-After", n)
	}
}

// A mailbox password given for an account with one password is ignored, and
// the log says so.
func TestMailboxPasswordForAOnePasswordAccountIsWarnedAbout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	lb := &logBuf{}
	svc, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
		Logger: slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelInfo}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	r := e.req()
	r.MailboxPassword = "not-needed-mailbox" // gitleaks:allow test value
	if res, err := svc.Login(t.Context(), r); err != nil || res.State != service.StateReady {
		t.Fatalf("Login = %+v, %v; want ready", res, err)
	}
	lb.want(t, "mailbox password was given for an account with one password")
	lb.wantNoSecret(t, "not-needed-mailbox")
}

// A held login whose 2FA submit was rate limited does not call Proton again
// inside the Retry-After: the submit is refused with the wait, the login kept.
func TestTOTPRateLimitHoldsTheLoginUntilTheWaitIsOver(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	e.f.FailNext(http.MethodPost, "/auth/v4/2fa", http.StatusTooManyRequests, 85131, 120)
	_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if ee := wantExit(t, err, exitcode.Blocked); !strings.Contains(ee.Hint, "2m0s") {
		t.Fatalf("hint after the 429 = %q, want it to name the 2m0s wait", ee.Hint)
	}
	e.clk.advance(10 * time.Second)
	_, err = e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if ee := wantExit(t, err, exitcode.Blocked); ee.Hint != "retry in 1m50s" {
		t.Fatalf("early resubmit: hint %q, want retry in 1m50s", ee.Hint)
	}
	if n := e.f.Calls("POST", "/auth/v4/2fa"); n != 1 {
		t.Fatalf("%d 2FA submits, want 1: none inside the Retry-After", n)
	}
	if p := e.phase(t); p != string(service.StateNeedsTOTP) {
		t.Fatalf("phase = %q, want the login kept", p)
	}
	e.clk.advance(111 * time.Second)
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if err != nil || res.State != service.StateReady {
		t.Fatalf("submit after the wait = %+v, %v; want ready", res, err)
	}
}

// A Retry-After longer than the login's remaining life ends it at once,
// naming the wait, instead of holding a login that cannot be finished.
func TestTOTPRateLimitBeyondTheLoginLifeEndsTheLogin(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	e.f.FailNext(http.MethodPost, "/auth/v4/2fa", http.StatusTooManyRequests, 85131, 400)
	_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "6m40s") {
		t.Fatalf("hint = %q, want it to name the 6m40s wait", ee.Hint)
	}
	if p := e.phase(t); p == string(service.StateNeedsTOTP) {
		t.Fatalf("phase = %q, want the login ended", p)
	}
	if n := e.f.Calls(http.MethodDelete, "/auth/v4"); n != 1 {
		t.Errorf("%d session revocations, want 1", n)
	}
}

// An unattended relogin whose 2FA submit is rate limited records the wait too.
func TestReloginTOTPRateLimitExtendsTheGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	e.loginUnattended(t, true)
	e.f.RevokeSessions()
	e.clk.advance(61 * time.Second)
	e.f.FailNext(http.MethodPost, "/auth/v4/2fa", http.StatusTooManyRequests, 85131, 300)
	if _, err := e.svc.Calendars(t.Context()); err == nil {
		t.Fatal("the request with a rate-limited relogin succeeded")
	}
	srp := e.logins()
	e.clk.advance(61 * time.Second)
	_, err := e.svc.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.Blocked); ee.Hint != "retry in 3m59s" {
		t.Fatalf("second request: hint %q, want retry in 3m59s", ee.Hint)
	}
	// The adapters' wait (MCP retry_after_s, CalDAV Retry-After) is the guard's.
	if got := service.ProtonRetryAfter(err); got != 3*time.Minute+59*time.Second {
		t.Fatalf("ProtonRetryAfter(relogin refused as too soon) = %v, want 3m59s", got)
	}
	if n := e.logins() - srp; n != 0 {
		t.Fatalf("%d login(s) reached Proton inside the Retry-After", n)
	}
}

// Any 429 with a Retry-After after the 2FA step (the scope unlock, the user
// fetch) keeps the login held and refuses an immediate resubmit: the wait is
// the login's own, not only the guard's.
func TestHeldLoginRefusesResubmitInsideAnyRetryAfter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, method, path string }{
		{"unlock", http.MethodPut, "/core/v4/users/unlock"},
		{"users over 60s", http.MethodGet, "/core/v4/users"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := heldAtTOTPWith(t, protontest.Options{TwoFA: true, LockedSalts: true})
			wait := 45
			if tc.method == http.MethodGet {
				wait = 120
			}
			e.f.FailNext(tc.method, tc.path, http.StatusTooManyRequests, 85131, wait)
			_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
			if err == nil {
				t.Fatal("SubmitTOTP succeeded on a 429")
			}
			if p := e.phase(t); p != string(service.StateNeedsTOTP) {
				t.Fatalf("phase = %q, want the login kept", p)
			}
			n := e.f.Calls(tc.method, tc.path)
			e.clk.advance(5 * time.Second)
			_, err = e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
			ee := wantExit(t, err, exitcode.Blocked)
			if !strings.Contains(ee.Hint, "retry in") {
				t.Fatalf("early resubmit: hint %q, want a retry in wording", ee.Hint)
			}
			if got := e.f.Calls(tc.method, tc.path); got != n {
				t.Fatalf("%d %s %s after the resubmit, want %d: nothing inside the Retry-After", got, tc.method, tc.path, n)
			}
			if p := e.phase(t); p != string(service.StateNeedsTOTP) {
				t.Fatalf("phase = %q after the refusal, want the login kept", p)
			}
			e.clk.advance(time.Duration(wait) * time.Second)
			res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
			if err != nil || res.State != service.StateReady {
				t.Fatalf("submit after the wait = %+v, %v; want ready", res, err)
			}
		})
	}
}

// The wait is kept even when another login holds the guard's lock and the
// guard cannot record it.
func TestHeldLoginKeepsTheWaitWhenTheGuardLockIsHeld(t *testing.T) {
	t.Parallel()
	e := heldAtTOTPWith(t, protontest.Options{TwoFA: true, LockedSalts: true})
	release, err := loginguard.Acquire(e.dirs.State, 0, e.clk.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.f.FailNext(http.MethodPut, "/core/v4/users/unlock", http.StatusTooManyRequests, 85131, 45)
	if _, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now())); err == nil {
		t.Fatal("SubmitTOTP succeeded on a 429")
	}
	release()
	n := e.f.Calls(http.MethodPut, "/core/v4/users/unlock")
	e.clk.advance(5 * time.Second)
	_, err = e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	_ = wantExit(t, err, exitcode.Blocked)
	if got := e.f.Calls(http.MethodPut, "/core/v4/users/unlock"); got != n {
		t.Fatalf("%d unlock calls, want %d: the resubmit must be refused", got, n)
	}
}

// A wait longer than the login can be held cancels it with a must-act error
// that says so and when to run pcb login again.
func TestRateLimitBeyondTheLoginLifeSaysTheLoginWasCancelled(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	e.f.FailNext(http.MethodPost, "/auth/v4/2fa", http.StatusTooManyRequests, 85131, 400)
	_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Error(), "cancelled") || !strings.Contains(ee.Hint, "run pcb login after") {
		t.Fatalf("error %q hint %q: want a cancelled login and a run pcb login after hint", ee.Error(), ee.Hint)
	}
}

// A transient error after a refresh on a 401 keeps the refreshed client: the
// retry must not start again from the refresh token SRP returned, which the
// refresh spent, and ends the login with a session, not a dead end.
func TestTransientRetryReusesTheRefreshedClient(t *testing.T) {
	t.Parallel()
	e := heldAtTOTP(t)
	// The first key call answers 401 and refreshes the key client (the 2FA
	// submit's own client is another one); then the user fetch fails.
	e.f.FailNext(http.MethodGet, "/core/v4/keys/salts", http.StatusUnauthorized, 0, 0)
	failUsers(e)
	if _, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now())); err == nil {
		t.Fatal("SubmitTOTP succeeded on a 502")
	}
	if n := e.f.Calls(http.MethodPost, "/auth/v4/refresh"); n != 1 {
		t.Fatalf("%d refreshes after the first attempt, want 1", n)
	}
	if p := e.phase(t); p != string(service.StateNeedsTOTP) {
		t.Fatalf("phase = %q, want the login kept", p)
	}
	e.clk.advance(31 * time.Second)
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if err != nil || res.State != service.StateReady {
		t.Fatalf("retry = %+v, %v; want ready on the refreshed tokens", res, err)
	}
	if n := e.f.Calls(http.MethodPost, "/auth/v4/refresh"); n != 1 {
		t.Fatalf("%d refreshes in all, want 1: the retry must not use the spent refresh token", n)
	}
}
