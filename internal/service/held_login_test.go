package service_test

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// heldAtTOTP is an env whose login waits for its 2FA code.
func heldAtTOTP(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, protontest.Options{TwoFA: true})
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
