package loginflow_test

import (
	"context"
	"errors"
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/loginflow"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestMain(m *testing.M) {
	// A new state never reaches the developer's desktop keyring.
	_ = os.Unsetenv("DBUS_SESSION_BUS_ADDRESS")
	os.Exit(m.Run())
}

type lab struct {
	f    *protontest.Fake
	dirs paths.Dirs
	hub  *service.Hub
}

// newLab opens real accounts (a service.Hub) over a fake Proton.
func newLab(t *testing.T) *lab {
	t.Helper()
	return newLabWith(t, protontest.Options{})
}

func newLabWith(t *testing.T, opts protontest.Options) *lab {
	t.Helper()
	f := protontest.New(t, opts)
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(dirs)
	cfg.Proton.BaseURL = f.URL
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: dirs, Config: cfg}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return &lab{f: f, dirs: dirs, hub: hub}
}

func (l *lab) flow() *loginflow.Flow {
	return loginflow.New(service.EntryWizard, func() (loginflow.Hub, error) { return l.hub, nil })
}

// forgetLastLogin lifts the one-login-a-minute guard (it has its own tests).
func (l *lab) forgetLastLogin(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(l.dirs.State, "last-login")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func code(t *testing.T, uri string) string {
	t.Helper()
	o, err := otp.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	return o.Code(time.Now())
}

func TestFlowPlainLogin(t *testing.T) {
	l := newLab(t)
	f := l.flow()
	if f.Step() != loginflow.StepCredentials || f.Account() != "" {
		t.Fatalf("first step %q, account %q", f.Step(), f.Account())
	}
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if f.Step() != loginflow.StepDone || f.Err() != nil {
		t.Fatalf("Step %q, Err %v", f.Step(), f.Err())
	}
	accts := l.hub.Accounts()
	if len(accts) != 1 || accts[0].ID != f.Account() || accts[0].Phase != "ready" {
		t.Errorf("accounts %+v, flow account %q", accts, f.Account())
	}
	if f.PasswordBuf() != nil {
		t.Error("the flow still holds the password after the login")
	}
}

// A CAPTCHA, then the 2FA code: the flow walks both steps against the Hub.
func TestFlowCaptchaThenTOTP(t *testing.T) {
	l := newLab(t)
	uri := l.f.SetTwoFA(l.f.Username, true)
	l.f.SetCaptchaFor(l.f.Username, "captcha-token")
	f := l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepCaptcha {
		t.Fatalf("Submit: %v, Step %q", err, f.Step())
	}
	if f.CaptchaURL() == "" {
		t.Error("no CAPTCHA URL")
	}
	l.forgetLastLogin(t)
	if err := f.CaptchaSolved(t.Context()); err != nil || f.Step() != loginflow.StepTOTP {
		t.Fatalf("CaptchaSolved: %v, Step %q", err, f.Step())
	}
	if err := f.SubmitTOTP(t.Context(), code(t, uri)); err != nil || f.Step() != loginflow.StepDone {
		t.Fatalf("SubmitTOTP: %v, Step %q", err, f.Step())
	}
	if a := l.hub.Accounts(); len(a) != 1 || a[0].Phase != "ready" {
		t.Errorf("accounts %+v", a)
	}
}

// The Hub is not there (a stopped desktop bridge): the call is refused with
// its error and the flow keeps its step.
func TestFlowRefusesWithoutAHub(t *testing.T) {
	gone := &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("the bridge is not running")}
	f := loginflow.New(service.EntryWizard, func() (loginflow.Hub, error) { return nil, gone })
	err := f.Submit(t.Context(), "a@example.org", []byte("pw"))
	if !errors.Is(err, gone) || f.Err() == nil || f.Err().Code != exitcode.Blocked || f.Step() != loginflow.StepCredentials {
		t.Errorf("Submit = %v, Err %v, Step %q", err, f.Err(), f.Step())
	}
}

func TestFlowCancelRemovesThePendingAdd(t *testing.T) {
	l := newLab(t)
	l.f.SetTwoFA(l.f.Username, true)
	f := l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepTOTP {
		t.Fatalf("Submit: %v, Step %q", err, f.Step())
	}
	if len(l.hub.Accounts()) != 1 {
		t.Fatal("control: the pending add is not listed")
	}
	f.Cancel()
	if n := len(l.hub.Accounts()); n != 0 || f.Step() != loginflow.StepCredentials {
		t.Errorf("%d accounts and Step %q after Cancel", n, f.Step())
	}
}

// Review C I1: a login started again for an account that is ready, then
// abandoned at the 2FA step (a page's TTL, a page replaced, Cancel), never
// signs that account out: its stored session keeps working.
func TestFlowCancelKeepsAnAccountThatWasReady(t *testing.T) {
	l := newLab(t)
	first := l.flow()
	if err := first.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || first.Step() != loginflow.StepDone {
		t.Fatalf("first login: %v, Step %q", err, first.Step())
	}
	id := first.Account()
	l.f.SetTwoFA(l.f.Username, true)
	l.forgetLastLogin(t)
	f := l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepTOTP {
		t.Fatalf("second login: %v, Step %q", err, f.Step())
	}
	f.Cancel()
	a := l.hub.Accounts()
	if len(a) != 1 || a[0].ID != id || a[0].Phase != "ready" {
		t.Errorf("accounts after Cancel %+v, want %s still ready", a, id)
	}
	if f.Step() != loginflow.StepCredentials || f.Account() != "" {
		t.Errorf("Step %q, account %q after Cancel", f.Step(), f.Account())
	}
}

// Report records an error without moving the flow.
func TestFlowReportKeepsTheStep(t *testing.T) {
	f := loginflow.New(service.EntryWizard, func() (loginflow.Hub, error) { return nil, errors.New("unused") })
	f.Report(&exitcode.Error{Code: exitcode.Blocked, Err: errors.New("could not read the password")})
	if f.Err() == nil || f.Err().Error() != "could not read the password" || f.Step() != loginflow.StepCredentials {
		t.Errorf("Err %v, Step %q", f.Err(), f.Step())
	}
}

const module = "github.com/Barben360/proton-calendar-bridge"

// The flow is shared by pcb-ui's wizard and the MCP login page (ruling
// R18): it imports neither of them, nor the daemon or the CLI, or a cycle
// (ui → daemon → mcp → ui) comes back.
func TestLoginflowArch(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		ctx := build.Default
		ctx.GOOS = goos
		pkg, err := ctx.ImportDir(".", 0)
		if err != nil {
			t.Fatal(err)
		}
		ours := 0
		for _, imp := range pkg.Imports {
			if strings.HasPrefix(imp, module+"/") {
				ours++
			}
			for _, banned := range []string{"/internal/ui", "/internal/daemon", "/internal/mcp", "/internal/cli", "/internal/proton", "/internal/store"} {
				if imp == module+banned || strings.HasPrefix(imp, module+banned+"/") {
					t.Errorf("GOOS=%s: internal/loginflow imports %s", goos, imp)
				}
			}
		}
		if ours == 0 {
			t.Errorf("GOOS=%s: no import of this module: the check would pass vacuously", goos)
		}
	}
}

// recHub records what the flow tells the Hub of itself.
type recHub struct {
	*service.Hub
	entries []string
	by      []string
}

func (r *recHub) Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error) {
	r.entries = append(r.entries, req.Entry)
	return r.Hub.Login(ctx, req)
}

func (r *recHub) Remove(ctx context.Context, selector string, req service.LogoutRequest) (service.AccountInfo, error) {
	r.by = append(r.by, req.By)
	return r.Hub.Remove(ctx, selector, req)
}

// Each login of a flow carries the flow's entry, and its Cancel removes the
// add as a cancel: the Hub's log tells the wizard, the MCP login page and
// pcb login apart (live 2026-11-13, F3).
func TestFlowTellsTheHubItsEntry(t *testing.T) {
	l := newLab(t)
	r := &recHub{Hub: l.hub}
	f := loginflow.New(service.EntryLoginPage, func() (loginflow.Hub, error) { return r, nil })
	l.f.SetCaptchaFor(l.f.Username, "tok")
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepCaptcha {
		t.Fatalf("Submit: %v, step %q", err, f.Step())
	}
	f.Cancel()
	if want := []string{service.EntryLoginPage}; !slices.Equal(r.entries, want) {
		t.Errorf("entries = %q, want %q", r.entries, want)
	}
	if want := []string{service.ByCancel}; !slices.Equal(r.by, want) {
		t.Errorf("Remove by = %q, want %q", r.by, want)
	}
	// The retry after the CAPTCHA carries it too.
	r.entries = nil
	f = loginflow.New(service.EntryLoginPage, func() (loginflow.Hub, error) { return r, nil })
	l.forgetLastLogin(t)
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepCaptcha {
		t.Fatalf("Submit again: %v, step %q", err, f.Step())
	}
	l.forgetLastLogin(t)
	if err := f.CaptchaSolved(t.Context()); err != nil {
		t.Fatalf("CaptchaSolved: %v", err)
	}
	if want := []string{service.EntryLoginPage, service.EntryLoginPage}; !slices.Equal(r.entries, want) {
		t.Errorf("entries with the retry = %q, want %q", r.entries, want)
	}
}

// The flow offers no unattended relogin: a login through it (pcb-ui's
// wizard, the MCP login page) turns it off for an account where pcb login
// --unattended-relogin turned it on, directly or after a CAPTCHA, so the
// stored password and 2FA secret are dropped.
func TestFlowLoginTurnsUnattendedReloginOff(t *testing.T) {
	for _, captcha := range []bool{false, true} {
		l := newLab(t)
		if _, err := l.hub.Login(t.Context(), service.LoginRequest{Username: l.f.Username, Password: l.f.Password, UnattendedRelogin: true}); err != nil {
			t.Fatalf("captcha %t: CLI login: %v", captcha, err)
		}
		unattended := func() bool {
			t.Helper()
			api, _, err := l.hub.Account(l.f.Username)
			if err != nil {
				t.Fatal(err)
			}
			st, err := api.Status(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			return st.Unattended
		}
		if !unattended() {
			t.Fatalf("captcha %t: the CLI login did not turn unattended relogin on: the test would pass vacuously", captcha)
		}
		if captcha {
			l.f.SetCaptchaFor(l.f.Username, "captcha-token")
		}
		l.forgetLastLogin(t)
		f := l.flow()
		if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil {
			t.Fatalf("captcha %t: Submit: %v", captcha, err)
		}
		if captcha {
			if f.Step() != loginflow.StepCaptcha {
				t.Fatalf("Step %q, want captcha", f.Step())
			}
			l.forgetLastLogin(t)
			if err := f.CaptchaSolved(t.Context()); err != nil {
				t.Fatalf("CaptchaSolved: %v", err)
			}
		}
		if f.Step() != loginflow.StepDone {
			t.Fatalf("captcha %t: Step %q, Err %v", captcha, f.Step(), f.Err())
		}
		if unattended() {
			t.Errorf("captcha %t: unattended relogin still on after a login through the flow", captcha)
		}
	}
}

// mailboxLab is a lab whose account is in two-password mode.
func mailboxLab(t *testing.T) *lab {
	t.Helper()
	return newLabWith(t, protontest.Options{TwoPassword: true})
}

// A two-password account stops on the mailbox step once the login password
// is accepted; the flow drops the login password there (the Hub holds the
// login), and the mailbox password finishes it.
func TestFlowMailboxPasswordStep(t *testing.T) {
	l := mailboxLab(t)
	f := l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if f.Step() != loginflow.StepMailboxPassword {
		t.Fatalf("Step %q, want mailbox-password", f.Step())
	}
	if f.PasswordBuf() != nil {
		t.Error("the flow still holds the login password on the mailbox step")
	}
	if err := f.SubmitMailboxPassword(t.Context(), []byte(l.f.MailboxPassword)); err != nil {
		t.Fatalf("SubmitMailboxPassword: %v", err)
	}
	if f.Step() != loginflow.StepDone || f.Err() != nil {
		t.Errorf("Step %q, Err %v; want done, nil", f.Step(), f.Err())
	}
}

// 2FA first, then the mailbox password.
func TestFlowTOTPThenMailboxPassword(t *testing.T) {
	l := mailboxLab(t)
	uri := l.f.SetTwoFA(l.f.Username, true)
	f := l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepTOTP {
		t.Fatalf("Submit: %v, step %q", err, f.Step())
	}
	if err := f.SubmitTOTP(t.Context(), code(t, uri)); err != nil {
		t.Fatalf("SubmitTOTP: %v", err)
	}
	if f.Step() != loginflow.StepMailboxPassword {
		t.Fatalf("Step %q, want mailbox-password", f.Step())
	}
	if err := f.SubmitMailboxPassword(t.Context(), []byte(l.f.MailboxPassword)); err != nil || f.Step() != loginflow.StepDone {
		t.Fatalf("SubmitMailboxPassword: %v, step %q", err, f.Step())
	}
}

// A wrong mailbox password keeps the step with the error while the Hub holds
// the login; once the Hub drops it (3 tries), the flow is back on the
// credentials. The right one still works after a wrong one.
func TestFlowWrongMailboxPassword(t *testing.T) {
	l := mailboxLab(t)
	f := l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepMailboxPassword {
		t.Fatalf("Submit: %v, step %q", err, f.Step())
	}
	for i := 1; i <= 2; i++ {
		err := f.SubmitMailboxPassword(t.Context(), []byte("wrong-mailbox-pw"))
		var ee *exitcode.Error
		if !errors.As(err, &ee) || ee.Code != exitcode.MustAct {
			t.Fatalf("try %d: err = %v, want exit %d", i, err, exitcode.MustAct)
		}
		if f.Step() != loginflow.StepMailboxPassword || f.Err() == nil {
			t.Fatalf("try %d: step %q, Err %v; want the step kept with Err set", i, f.Step(), f.Err())
		}
	}
	if err := f.SubmitMailboxPassword(t.Context(), []byte("wrong-mailbox-pw")); err == nil {
		t.Fatal("third wrong try: no error")
	}
	if f.Step() != loginflow.StepCredentials || f.Err() == nil || f.Account() != "" {
		t.Errorf("after the Hub dropped the login: step %q, Err %v, account %q; want credentials, Err set, no account", f.Step(), f.Err(), f.Account())
	}

	l.forgetLastLogin(t)
	f = l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil {
		t.Fatal(err)
	}
	_ = f.SubmitMailboxPassword(t.Context(), []byte("wrong-mailbox-pw"))
	if err := f.SubmitMailboxPassword(t.Context(), []byte(l.f.MailboxPassword)); err != nil || f.Step() != loginflow.StepDone || f.Err() != nil {
		t.Errorf("right one after a wrong one: %v, step %q, Err %v", err, f.Step(), f.Err())
	}
}

// SubmitMailboxPassword off its step is a usage error and changes nothing.
func TestFlowMailboxPasswordOnAnotherStep(t *testing.T) {
	l := mailboxLab(t)
	f := l.flow()
	err := f.SubmitMailboxPassword(t.Context(), []byte("x"))
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.Usage {
		t.Fatalf("err = %v, want a usage error", err)
	}
	if f.Step() != loginflow.StepCredentials {
		t.Errorf("Step %q, want credentials", f.Step())
	}
}

// cancelHub records which of Cancel's Hub calls ran.
type cancelHub struct {
	*service.Hub
	calls []string
}

func (c *cancelHub) AbandonLogin(ctx context.Context, selector string) error {
	c.calls = append(c.calls, "abandon")
	return c.Hub.AbandonLogin(ctx, selector)
}

func (c *cancelHub) SignOut(ctx context.Context, selector string, req service.LogoutRequest) (service.LogoutResult, error) {
	c.calls = append(c.calls, "signout")
	return c.Hub.SignOut(ctx, selector, req)
}

func (c *cancelHub) Remove(ctx context.Context, selector string, req service.LogoutRequest) (service.AccountInfo, error) {
	c.calls = append(c.calls, "remove")
	return c.Hub.Remove(ctx, selector, req)
}

// Cancel on the mailbox step: an add this flow made is removed, an account
// that was ready is only abandoned (its session stays), another one that
// existed pending is signed out.
func TestCancelOnMailboxStep(t *testing.T) {
	// reach returns a recording flow on its mailbox step; finishFirst logs
	// the account in completely beforehand (it was ready).
	reach := func(t *testing.T, l *lab, finishFirst bool) (*loginflow.Flow, *cancelHub) {
		t.Helper()
		h := &cancelHub{Hub: l.hub}
		if finishFirst {
			first := l.flow()
			if err := first.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil {
				t.Fatal(err)
			}
			if err := first.SubmitMailboxPassword(t.Context(), []byte(l.f.MailboxPassword)); err != nil || first.Step() != loginflow.StepDone {
				t.Fatalf("first login: %v, step %q", err, first.Step())
			}
			l.forgetLastLogin(t)
		}
		f := loginflow.New(service.EntryWizard, func() (loginflow.Hub, error) { return h, nil })
		if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepMailboxPassword {
			t.Fatalf("Submit: %v, step %q", err, f.Step())
		}
		return f, h
	}
	t.Run("created is removed", func(t *testing.T) {
		l := mailboxLab(t)
		f, h := reach(t, l, false)
		f.Cancel()
		if want := []string{"remove"}; !slices.Equal(h.calls, want) {
			t.Errorf("Hub calls on Cancel = %q, want %q", h.calls, want)
		}
		if got := l.hub.Accounts(); len(got) != 0 {
			t.Errorf("accounts after Cancel = %+v, want none", got)
		}
		if f.Step() != loginflow.StepCredentials {
			t.Errorf("Step %q, want credentials", f.Step())
		}
	})
	t.Run("was ready is abandoned", func(t *testing.T) {
		l := mailboxLab(t)
		f, h := reach(t, l, true)
		f.Cancel()
		if want := []string{"abandon"}; !slices.Equal(h.calls, want) {
			t.Errorf("Hub calls on Cancel = %q, want %q", h.calls, want)
		}
	})
	t.Run("existed pending is signed out", func(t *testing.T) {
		l := mailboxLab(t)
		pre := l.flow()
		if err := pre.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || pre.Step() != loginflow.StepMailboxPassword {
			t.Fatalf("first Submit: %v, step %q", err, pre.Step())
		}
		l.forgetLastLogin(t)
		h := &cancelHub{Hub: l.hub}
		f := loginflow.New(service.EntryWizard, func() (loginflow.Hub, error) { return h, nil })
		if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepMailboxPassword {
			t.Fatalf("second Submit: %v, step %q", err, f.Step())
		}
		f.Cancel()
		if want := []string{"signout"}; !slices.Equal(h.calls, want) {
			t.Errorf("Hub calls on Cancel = %q, want %q", h.calls, want)
		}
	})
}

// heldHub is a Hub stub whose SubmitTOTP fails with a must-act error while
// the account is reported held at phase.
type heldHub struct {
	loginflow.Hub
	phase string
}

func (h *heldHub) Accounts() []service.AccountInfo {
	return []service.AccountInfo{{ID: "acc", Pending: true, Phase: h.phase}}
}

func (h *heldHub) Login(context.Context, service.LoginRequest) (service.LoginResult, error) {
	return service.LoginResult{Account: "acc", State: service.StateNeedsTOTP}, nil
}

func (h *heldHub) SubmitTOTP(context.Context, string, string) (service.LoginResult, error) {
	return service.LoginResult{}, &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("wrong mailbox password")}
}

// A good 2FA code followed by a wrong upfront mailbox password leaves the Hub
// held at the mailbox step: the flow follows it, so the screen is not stranded
// on the 2FA one. A Hub still at the 2FA step keeps StepTOTP.
func TestFlowSubmitTOTPFollowsTheHeldStep(t *testing.T) {
	for _, c := range []struct {
		phase string
		want  loginflow.Step
	}{
		{string(service.StateNeedsMailboxPassword), loginflow.StepMailboxPassword},
		{string(service.StateNeedsTOTP), loginflow.StepTOTP},
	} {
		h := &heldHub{phase: c.phase}
		f := loginflow.New(service.EntryWizard, func() (loginflow.Hub, error) { return h, nil })
		if err := f.Submit(t.Context(), "a@example.test", []byte("pw")); err != nil || f.Step() != loginflow.StepTOTP {
			t.Fatalf("%s: Submit: %v, step %q", c.phase, err, f.Step())
		}
		if err := f.SubmitTOTP(t.Context(), "123456"); err == nil {
			t.Fatalf("%s: SubmitTOTP: no error", c.phase)
		}
		if f.Step() != c.want || f.Err() == nil {
			t.Errorf("%s: step %q, Err %v; want %q with Err set", c.phase, f.Step(), f.Err(), c.want)
		}
	}
}
