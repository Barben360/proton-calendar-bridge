package service_test

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// logBuf is a slog text sink safe for the Hub's goroutines.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// line is the first record holding every one of parts, "" if none.
func (l *logBuf) line(parts ...string) string {
	for _, ln := range strings.Split(l.String(), "\n") {
		ok := ln != ""
		for _, p := range parts {
			ok = ok && strings.Contains(ln, p)
		}
		if ok {
			return ln
		}
	}
	return ""
}

func (l *logBuf) want(t *testing.T, parts ...string) {
	t.Helper()
	if l.line(parts...) == "" {
		t.Errorf("no log record with %q in:\n%s", parts, l.String())
	}
}

// The log never holds what a login carries: no address (accounts are local
// IDs), no password, no CAPTCHA URL or token.
func (l *logBuf) wantNoSecret(t *testing.T, secrets ...string) {
	t.Helper()
	s := l.String()
	if strings.Contains(s, "@") {
		t.Errorf("the log holds an address:\n%s", s)
	}
	for _, x := range secrets {
		if x != "" && strings.Contains(s, x) {
			t.Errorf("the log holds %q:\n%s", x, s)
		}
	}
}

func (e *hubEnv) openLogged(t *testing.T) (*service.Hub, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	o := e.options()
	o.Logger = slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelInfo}))
	h, err := service.OpenHub(o)
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h, lb
}

// Every login is logged at Info, in pcb-ui as in pcb serve (the Hub is
// theirs): where it started, then its outcome (ready, needs 2FA, needs
// CAPTCHA, refused with the reason the person reads, too soon with the
// wait). Nothing of the credentials or the CAPTCHA reaches the log (live
// 2026-11-13, F3: pcb-ui's log had no trace of three logins).
func TestHubLogsLoginEvents(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h, lb := e.openLogged(t)
	e.f.SetCaptchaFor(userB, "captcha-tok-123")
	e.f.SetTwoFA(userC, true)

	ra := e.mustLogin(t, h, userA)
	lb.want(t, `msg="login started"`, "entry=cli", "account="+ra.Account, "add=true")
	lb.want(t, `msg="login ended"`, "entry=cli", "account="+ra.Account, "outcome=ready")

	rb, err := e.loginFrom(t, h, userB, service.EntryWizard)
	if err != nil || rb.State != service.StateNeedsCaptcha {
		t.Fatalf("control: Login(b) = %+v, %v, want needs-captcha", rb, err)
	}
	lb.want(t, `msg="login started"`, "entry=wizard", "account="+rb.Account, "add=true")
	lb.want(t, `msg="login ended"`, "entry=wizard", "account="+rb.Account, "outcome=needs-captcha")

	rc, err := e.loginFrom(t, h, userC, service.EntryLoginPage)
	if err != nil || rc.State != service.StateNeedsTOTP {
		t.Fatalf("control: Login(c) = %+v, %v, want needs-totp", rc, err)
	}
	lb.want(t, `msg="login ended"`, "entry=mcp-login-page", "account="+rc.Account, "outcome=needs-2fa")

	// Too soon: no wait since the last attempt.
	_, err = h.Login(t.Context(), service.LoginRequest{Username: userA, Password: e.password(userA), Entry: service.EntryWizard})
	_ = wantExit(t, err, exitcode.Blocked)
	lb.want(t, `msg="login ended"`, "entry=wizard", "account="+ra.Account, "outcome=too-soon", "wait=")

	// Refused: the reason the person reads, and the exit code.
	e.clk.advance(loginguard.MinInterval + time.Second)
	_, err = h.Login(t.Context(), service.LoginRequest{Username: userA, Password: "not-the-password", Entry: service.EntryCLI})
	ee := wantExit(t, err, exitcode.MustAct)
	lb.want(t, `msg="login ended"`, "account="+ra.Account, "outcome=refused", "exit=10", ee.Error())

	lb.wantNoSecret(t, e.f.Password, passwordB, "not-the-password", "captcha-tok-123", e.f.URL, "verify")
}

// The 2FA code's answer is logged as the login's outcome.
func TestHubLogsTheTOTPOutcome(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h, lb := e.openLogged(t)
	e.f.SetTwoFA(userB, true)
	rb := e.mustLogin(t, h, userB)
	_, err := h.SubmitTOTP(t.Context(), rb.Account, "000000")
	if err == nil {
		t.Fatal("control: a wrong code was accepted")
	}
	lb.want(t, `msg="login 2FA code"`, "account="+rb.Account, "outcome=refused")
	lb.wantNoSecret(t, passwordB, "000000")
}

// An add that is dropped is logged with who dropped it: the wizard's
// Cancel (Remove with By), the lazy sweep, OpenHub, a failed first login.
func TestHubLogsTheAddsItRemoves(t *testing.T) {
	t.Parallel()
	t.Run("cancel", func(t *testing.T) {
		t.Parallel()
		e := newHubEnv(t)
		h, lb := e.openLogged(t)
		e.f.SetCaptchaFor(userB, "tok")
		rb := e.mustLogin(t, h, userB)
		if _, err := h.Remove(t.Context(), rb.Account, service.LogoutRequest{By: service.ByCancel}); err != nil {
			t.Fatal(err)
		}
		lb.want(t, `msg="account removed"`, "account="+rb.Account, "pending=true", "by=cancel")
	})
	t.Run("sweep", func(t *testing.T) {
		t.Parallel()
		e := newHubEnv(t)
		h, lb := e.openLogged(t)
		e.f.SetTwoFA(userB, true)
		rb := e.mustLogin(t, h, userB)
		e.expireAll()
		if len(h.Accounts()) != 0 {
			t.Fatal("control: the expired 2FA add is still listed")
		}
		lb.want(t, `msg="removed an add that never logged in"`, "account="+rb.Account, "by=sweep")
	})
	t.Run("OpenHub", func(t *testing.T) {
		t.Parallel()
		e := newHubEnv(t)
		h := e.open(t)
		e.f.SetCaptchaFor(userB, "tok")
		rb := e.mustLogin(t, h, userB)
		_ = h.Close()
		e.clk.advance(25 * time.Hour)
		h2, lb := e.openLogged(t)
		if len(h2.Accounts()) != 0 {
			t.Fatal("control: the expired CAPTCHA add is still listed")
		}
		lb.want(t, `msg="removed an add that never logged in"`, "account="+rb.Account, "by=OpenHub")
	})
	t.Run("failed login", func(t *testing.T) {
		t.Parallel()
		e := newHubEnv(t)
		h, lb := e.openLogged(t)
		e.clk.advance(loginguard.MinInterval + time.Second)
		_, err := h.Login(t.Context(), service.LoginRequest{Username: userC, Password: "wrong"})
		_ = wantExit(t, err, exitcode.MustAct)
		ln := lb.line(`msg="login started"`, "add=true")
		i := strings.Index(ln, "account=")
		if i < 0 {
			t.Fatalf("no add started:\n%s", lb.String())
		}
		id := strings.Fields(ln[i:])[0]
		lb.want(t, `msg="removed an add that never logged in"`, id, "by=failed-login")
	})
}

// loginFrom logs in as username from entry, after the login guard's
// interval.
func (e *hubEnv) loginFrom(t *testing.T, h *service.Hub, username, entry string) (service.LoginResult, error) {
	t.Helper()
	e.clk.advance(loginguard.MinInterval + time.Second)
	return h.Login(t.Context(), service.LoginRequest{Username: username, Password: e.password(username), Entry: entry})
}

// A refusal whose message names an address (the Proton user is already
// another account's) is logged with the address replaced.
func TestHubLogRedactsTheAddressOfARefusal(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h, lb := e.openLogged(t)
	e.skip(t, h)
	e.mustLogin(t, h, userA)
	_, err := e.login(t, h, aliasA)
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), userA) {
		t.Fatalf("control: the refusal %q does not name %s", ee.Error(), userA)
	}
	lb.want(t, `msg="login ended"`, "outcome=refused", "already added as <address>")
	lb.wantNoSecret(t)
}

// A login refused before an account is chosen (no password, the Hub
// closing, a login already running) logs "login refused" with its entry
// and reason, no account and no "login ended" without a start (review M6).
func TestHubLogsALoginRefusedBeforeItStarts(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h, lb := e.openLogged(t)
	_, err := h.Login(t.Context(), service.LoginRequest{Username: userA, Entry: service.EntryWizard})
	_ = wantExit(t, err, exitcode.Usage)
	lb.want(t, `msg="login refused"`, "entry=wizard", "exit=2", "a username and a password are required")
	for _, ln := range strings.Split(lb.String(), "\n") {
		if strings.Contains(ln, `msg="login ended"`) || strings.Contains(ln, `msg="login started"`) || strings.Contains(ln, `account=""`) {
			t.Errorf("a refusal before the start logged %q", ln)
		}
	}
}
