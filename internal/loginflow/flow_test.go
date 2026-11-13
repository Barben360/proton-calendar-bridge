package loginflow_test

import (
	"errors"
	"go/build"
	"os"
	"path/filepath"
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
	return loginflow.New(func() (loginflow.Hub, error) { return l.hub, nil })
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
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil {
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
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil || f.Step() != loginflow.StepCaptcha {
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
	f := loginflow.New(func() (loginflow.Hub, error) { return nil, gone })
	err := f.Submit(t.Context(), "a@example.org", []byte("pw"), false)
	if !errors.Is(err, gone) || f.Err() == nil || f.Err().Code != exitcode.Blocked || f.Step() != loginflow.StepCredentials {
		t.Errorf("Submit = %v, Err %v, Step %q", err, f.Err(), f.Step())
	}
}

func TestFlowCancelRemovesThePendingAdd(t *testing.T) {
	l := newLab(t)
	l.f.SetTwoFA(l.f.Username, true)
	f := l.flow()
	if err := f.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil || f.Step() != loginflow.StepTOTP {
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

// Report records an error without moving the flow.
func TestFlowReportKeepsTheStep(t *testing.T) {
	f := loginflow.New(func() (loginflow.Hub, error) { return nil, errors.New("unused") })
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
