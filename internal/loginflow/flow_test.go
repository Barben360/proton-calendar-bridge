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
	f := protontest.New(t, protontest.Options{})
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

// CaptchaWait is the time left until the CaptchaRetryAt the Hub returned
// with the CAPTCHA (10 s after the guard's stamp of that attempt), for the
// window to announce; nothing once the CAPTCHA step is left.
func TestFlowCaptchaWait(t *testing.T) {
	l := newLab(t)
	l.f.SetTwoFA(l.f.Username, true) // the retry lands on the 2FA step
	l.f.SetCaptchaFor(l.f.Username, "captcha-token")
	f := l.flow()
	var now time.Time
	f.SetNow(func() time.Time { return now })
	if d := f.CaptchaWait(); d != 0 {
		t.Errorf("before any login: CaptchaWait %v, want 0", d)
	}
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password)); err != nil || f.Step() != loginflow.StepCaptcha {
		t.Fatalf("Submit: %v, Step %q", err, f.Step())
	}
	b, err := os.ReadFile(filepath.Join(l.dirs.State, "last-login"))
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	now = stamp.Add(4 * time.Second)
	if d := f.CaptchaWait(); d != 6*time.Second {
		t.Errorf("solved 4 s after the attempt: CaptchaWait %v, want 6s", d)
	}
	now = stamp.Add(30 * time.Second)
	if d := f.CaptchaWait(); d != 0 {
		t.Errorf("solved 30 s after the attempt: CaptchaWait %v, want 0", d)
	}
	// Back within the 10 s: off the CAPTCHA step nothing is announced.
	now = stamp.Add(5 * time.Second)
	l.forgetLastLogin(t)
	if err := f.CaptchaSolved(t.Context()); err != nil || f.Step() != loginflow.StepTOTP {
		t.Fatalf("CaptchaSolved: %v, Step %q", err, f.Step())
	}
	if d := f.CaptchaWait(); d != 0 {
		t.Errorf("on the 2FA step: CaptchaWait %v, want 0", d)
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
