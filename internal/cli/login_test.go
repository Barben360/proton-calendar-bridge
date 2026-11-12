package cli_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func TestNoFlagTakesASecret(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"login", "--password", "x"},
		{"login", "--totp", "123456"},
		{"login", "--password=x"},
		{"login", "resume", "--captcha-token", "T", "--password", "x"},
		{"login", "resume", "--captcha-token", "T", "--totp", "1"},
	} {
		s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		r := stubRig(t, s)
		r.want(exitcode.Usage, args...)
		if len(s.logins) != 0 {
			t.Errorf("pcb %s reached the service", strings.Join(args, " "))
		}
	}
}

func TestNonInteractiveNamesMissing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		drop string
		want []string
	}{
		{"PCB_PASSWORD", []string{"PCB_PASSWORD", "--password-fd"}},
		{"PCB_USERNAME", []string{"PCB_USERNAME", "--username"}},
	} {
		t.Run(tc.drop, func(t *testing.T) {
			s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
			r := stubRig(t, s)
			delete(r.env, tc.drop)
			r.want(exitcode.MustAct, "login", "--non-interactive", "--json")
			e := r.envelope()
			for _, w := range tc.want {
				if e.Error == nil || !strings.Contains(e.Error.Message, w) {
					t.Errorf("message %+v does not name %s", e.Error, w)
				}
			}
			if len(s.logins) != 0 {
				t.Errorf("%d login calls, want none", len(s.logins))
			}
		})
	}
}

func TestNonInteractiveFlagBeatsTerminal(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	delete(r.env, "PCB_PASSWORD")
	r.term.interactive = true
	r.term.secrets = []string{"pw"}
	r.want(exitcode.MustAct, "login", "--non-interactive")
	if len(r.term.secretPrompts)+len(r.term.linePrompts) != 0 {
		t.Errorf("prompted despite --non-interactive: %v %v", r.term.secretPrompts, r.term.linePrompts)
	}
}

func TestNoTerminalMeansNonInteractive(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	delete(r.env, "PCB_PASSWORD")
	r.term.secrets = []string{"pw"} // would answer, but stdin is not a terminal
	r.want(exitcode.MustAct, "login")
	if len(r.term.secretPrompts) != 0 {
		t.Error("prompted without a terminal")
	}
}

func TestInteractivePromptsForCredentials(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	delete(r.env, "PCB_USERNAME")
	delete(r.env, "PCB_PASSWORD")
	r.term = &fakeTerm{interactive: true, lines: []string{"alice"}, secrets: []string{"s3 cret"}}
	r.want(exitcode.OK, "login")
	if len(s.logins) != 1 || s.logins[0].Username != "alice" || s.logins[0].Password != "s3 cret" {
		t.Fatalf("login requests = %v", s.logins)
	}
	if len(r.term.linePrompts) != 1 || len(r.term.secretPrompts) != 1 {
		t.Errorf("prompts: line %v, secret %v; the password must use the secret prompt", r.term.linePrompts, r.term.secretPrompts)
	}
}

func TestEmptyInteractivePasswordIsUsage(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	delete(r.env, "PCB_PASSWORD")
	r.term = &fakeTerm{interactive: true, secrets: []string{""}}
	r.want(exitcode.Usage, "login")
	if len(s.logins) != 0 {
		t.Error("an empty password reached the service")
	}
}

func TestUsernameFlagBeatsEnv(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	r.env["PCB_USERNAME"] = "env-user"
	r.env["PCB_TOTP"] = "123456"
	r.want(exitcode.OK, "login", "--username", "flag-user", "--unattended-relogin")
	got := s.logins[0]
	if got.Username != "flag-user" || got.Password != r.env["PCB_PASSWORD"] || got.TOTP != "123456" || !got.UnattendedRelogin || got.CaptchaToken != "" {
		t.Fatalf("request = %+v", got)
	}
}

func TestLoginSuccessOutput(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	e := r.envelope()
	if !e.OK || e.Command != "login" || !strings.Contains(string(e.Data), `"state": "ready"`) {
		t.Fatalf("envelope = %+v", e)
	}
	for _, s := range []string{r.f.Password, r.f.TOTPURI} {
		if strings.Contains(r.out.String()+r.errb.String(), s) {
			t.Errorf("output contains a secret")
		}
	}
	r.want(exitcode.OK, "status")
	if !strings.Contains(r.out.String(), "phase: ready\n") {
		t.Errorf("status = %q", r.out.String())
	}
}

func TestWrongPasswordIsExit10WithoutEchoingIt(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.env["PCB_PASSWORD"] = "not-the-password"
	r.want(exitcode.MustAct, "login", "--non-interactive")
	if strings.Contains(r.out.String()+r.errb.String(), "not-the-password") {
		t.Error("the password was echoed")
	}
	if !strings.Contains(r.errb.String(), "Hint:") {
		t.Errorf("stderr %q has no hint", r.errb.String())
	}
}

func TestNeedsTOTPNonInteractive(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	r.want(exitcode.MustAct, "login", "--non-interactive", "--json")
	e := r.envelope()
	if e.Error == nil || e.Error.Code != 10 || !strings.Contains(e.Error.Message+e.Error.Hint, "PCB_TOTP") {
		t.Fatalf("error = %+v, want exit 10 naming PCB_TOTP", e.Error)
	}
	if n := r.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d DELETE /auth/v4, want 1 (the pending session aborted)", n)
	}
	if n := r.f.Calls("POST", "/auth/v4/2fa"); n != 0 {
		t.Errorf("%d 2FA submissions without a code", n)
	}
}

func TestTOTPFromEnvLogsIn(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	r.env["PCB_TOTP"] = r.f.TOTPURI
	r.want(exitcode.OK, "login", "--non-interactive")
}

func TestInteractiveTOTPPrompt(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	r.term = &fakeTerm{interactive: true, secrets: []string{r.totpCode()}}
	r.want(exitcode.OK, "login")
	if n := r.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
	if len(r.term.secretPrompts) != 1 {
		t.Errorf("secret prompts = %v, want one (the code is not echoed)", r.term.secretPrompts)
	}
}

func TestInteractiveTOTPReprompts(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	r.term = &fakeTerm{interactive: true, secrets: []string{"000000", r.totpCode()}}
	r.want(exitcode.OK, "login")
	if len(r.term.secretPrompts) != 2 {
		t.Errorf("secret prompts = %d, want 2", len(r.term.secretPrompts))
	}
	if !strings.Contains(r.errb.String(), "rejected") {
		t.Errorf("stderr %q does not tell the first code was rejected", r.errb.String())
	}
}

func TestInteractiveTOTPStopsAfterThreeRejections(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	r.term = &fakeTerm{interactive: true, secrets: []string{"000000", "000001", "000002", "000003"}}
	r.want(exitcode.MustAct, "login")
	if len(r.term.secretPrompts) != 3 {
		t.Errorf("secret prompts = %d, want 3 (the service's tries)", len(r.term.secretPrompts))
	}
	if n := r.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d DELETE /auth/v4, want 1", n)
	}
}

func TestCaptchaNonInteractiveExit10(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{Captcha: "T1"})
	r.want(exitcode.MustAct, "login", "--non-interactive", "--json")
	e := r.envelope()
	if e.OK || e.Error == nil || e.Error.Code != 10 {
		t.Fatalf("envelope = %+v", e)
	}
	if e.Error.Hint != "pcb login resume --captcha-token T1" {
		t.Errorf("hint = %q", e.Error.Hint)
	}
	if !strings.Contains(e.Error.Message, "https://verify.proton.me/") {
		t.Errorf("message %q lacks the CAPTCHA URL", e.Error.Message)
	}
	var data struct {
		URL   string `json:"captcha_url"`
		Token string `json:"captcha_token"`
	}
	mustUnmarshal(t, e.Data, &data)
	if data.Token != "T1" || !strings.Contains(data.URL, "T1") {
		t.Errorf("data = %+v", data)
	}
}

func TestCaptchaInteractiveRetries(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{Captcha: "T1"})
	r.term = &fakeTerm{interactive: true, lines: []string{""}}
	r.want(exitcode.OK, "login")
	if n := r.f.Calls("POST", "/auth/v4"); n != 2 {
		t.Errorf("%d POST /auth/v4, want 2", n)
	}
	if !strings.Contains(r.errb.String(), "https://verify.proton.me/") || len(r.term.linePrompts) != 1 {
		t.Errorf("stderr %q, prompts %v: the URL and one wait for Enter expected", r.errb.String(), r.term.linePrompts)
	}
}

func TestCaptchaInteractiveStopsAfterThreeRounds(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) {
		return service.LoginResult{State: service.StateNeedsCaptcha, CaptchaURL: "https://verify.proton.me/?t=T9", CaptchaToken: "T9"}, nil
	}}
	r := stubRig(t, s)
	r.term = &fakeTerm{interactive: true, lines: []string{"", "", "", "", ""}}
	r.want(exitcode.MustAct, "login")
	if len(s.logins) != 4 {
		t.Fatalf("%d login attempts, want 1 + 3 rounds", len(s.logins))
	}
	if s.logins[1].CaptchaToken != "T9" || s.logins[0].CaptchaToken != "" {
		t.Errorf("tokens = %q, %q", s.logins[0].CaptchaToken, s.logins[1].CaptchaToken)
	}
	if !strings.Contains(r.errb.String(), "pcb login resume --captcha-token T9") {
		t.Errorf("stderr %q lacks the resume hint", r.errb.String())
	}
}

func TestLoginResume(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{Captcha: "T1"})
	r.want(exitcode.MustAct, "login", "--non-interactive")
	r.want(exitcode.OK, "login", "resume", "--captcha-token", "T1", "--non-interactive")
	if n := r.f.Calls("POST", "/auth/v4"); n != 2 {
		t.Errorf("%d POST /auth/v4, want 2", n)
	}
}

func TestLoginResumeNeedsToken(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	r.want(exitcode.Usage, "login", "resume", "--json")
	if len(s.logins) != 0 {
		t.Error("resume without a token reached the service")
	}
}

func TestLoginClosesTheService(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	r.want(exitcode.OK, "login")
	if s.closed == 0 {
		t.Error("the service was not closed")
	}
}

func TestCtrlCEndsEveryPrompt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		o      protontest.Options
		env    []string // variables to drop
		pcbArg []string
		abort  bool // the pending login must be aborted at Proton
		hint   string
	}{
		{name: "password prompt", env: []string{"PCB_PASSWORD"}},
		{name: "username prompt", env: []string{"PCB_USERNAME"}},
		{name: "2FA prompt", o: protontest.Options{TwoFA: true}, abort: true},
		{name: "CAPTCHA wait", o: protontest.Options{Captcha: "T1"}, hint: "pcb login resume --captcha-token T1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, &tc.o)
			for _, k := range tc.env {
				delete(r.env, k)
			}
			r.term = &fakeTerm{interactive: true, block: true}
			r.ctrlC = true
			r.want(exitcode.Blocked, "login", "--json")
			// Timed from the cancellation, not the start: SRP and the key
			// derivation before the prompt are slow under -race.
			if r.cancelled.IsZero() {
				t.Fatal("the command never waited on the terminal")
			}
			if d := time.Since(r.cancelled); d > 10*time.Second {
				t.Errorf("the prompt outlived the cancellation by %v", d)
			}
			e := r.envelope()
			if tc.hint != "" && (e.Error == nil || e.Error.Hint != tc.hint) {
				t.Errorf("error = %+v, want the resume hint %q", e.Error, tc.hint)
			}
			if n := r.f.Calls("DELETE", "/auth/v4"); tc.abort && n != 1 {
				t.Errorf("%d DELETE /auth/v4, want the pending login aborted", n)
			}
		})
	}
}

func TestEndOfInputAtAPromptIsExit10(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		drop string
		want string
	}{
		{"password", "PCB_PASSWORD", "Proton password"},
		{"username", "PCB_USERNAME", "Proton username"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
			r := stubRig(t, s)
			delete(r.env, tc.drop)
			r.term = &fakeTerm{interactive: true} // every read answers EOF
			r.want(exitcode.MustAct, "login", "--json")
			if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, tc.want) {
				t.Errorf("error = %+v, want it to name %q", e.Error, tc.want)
			}
			if len(s.logins) != 0 {
				t.Error("login reached the service")
			}
		})
	}
	t.Run("2FA code", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, &protontest.Options{TwoFA: true})
		r.term = &fakeTerm{interactive: true}
		r.want(exitcode.MustAct, "login", "--json")
		if e := r.envelope(); e.Error == nil || !strings.Contains(e.Error.Message, "2FA code") {
			t.Errorf("error = %+v", e.Error)
		}
		if n := r.f.Calls("DELETE", "/auth/v4"); n != 1 {
			t.Errorf("%d DELETE /auth/v4, want 1", n)
		}
	})
}

func TestCaptchaPromptFailureKeepsTheResumeHint(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{Captcha: "T1"})
	r.term = &fakeTerm{interactive: true} // Enter never comes: EOF
	r.want(exitcode.MustAct, "login", "--json")
	e := r.envelope()
	if e.Error == nil || e.Error.Hint != "pcb login resume --captcha-token T1" {
		t.Fatalf("error = %+v", e.Error)
	}
	if !strings.Contains(string(e.Data), "T1") {
		t.Errorf("data %s lacks the token", e.Data)
	}
}

func TestInteractiveJSONKeepsStdoutToOneEnvelope(t *testing.T) {
	t.Parallel()
	t.Run("2FA", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, &protontest.Options{TwoFA: true})
		r.term = &fakeTerm{interactive: true, secrets: []string{"000000", r.totpCode()}}
		r.want(exitcode.OK, "login", "--json")
		if e := r.envelope(); !e.OK || e.Command != "login" {
			t.Errorf("envelope = %+v", e)
		}
		if !strings.Contains(r.errb.String(), "rejected") {
			t.Errorf("the rejected code is not reported on stderr: %q", r.errb.String())
		}
	})
	t.Run("CAPTCHA", func(t *testing.T) {
		t.Parallel()
		r := newRig(t, &protontest.Options{Captcha: "T1"})
		r.term = &fakeTerm{interactive: true, lines: []string{""}}
		r.want(exitcode.OK, "login", "--json")
		if e := r.envelope(); !e.OK {
			t.Errorf("envelope = %+v", e)
		}
		if strings.Contains(r.out.String(), "verify.proton.me") || !strings.Contains(r.errb.String(), "https://verify.proton.me/") {
			t.Errorf("the CAPTCHA URL must be on stderr only: stdout %q stderr %q", r.out.String(), r.errb.String())
		}
	})
}

func TestCloseFailureIsReportedNotFatal(t *testing.T) {
	t.Parallel()
	closeErr := errors.New("abort failed")
	t.Run("success stays 0", func(t *testing.T) {
		t.Parallel()
		s := &stubAPI{closeErr: closeErr, login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		r := stubRig(t, s)
		r.want(exitcode.OK, "login", "--json")
		if n := strings.Count(r.errb.String(), "abort failed"); n != 1 {
			t.Errorf("stderr %q mentions the failure %d times, want once", r.errb.String(), n)
		}
		if e := r.envelope(); !e.OK {
			t.Errorf("envelope = %+v", e)
		}
	})
	t.Run("failure keeps its code", func(t *testing.T) {
		t.Parallel()
		s := &stubAPI{closeErr: closeErr, login: func(service.LoginRequest) (service.LoginResult, error) {
			return service.LoginResult{}, &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("wrong password")}
		}}
		r := stubRig(t, s)
		r.want(exitcode.MustAct, "login")
		if !strings.Contains(r.errb.String(), "abort failed") {
			t.Errorf("stderr %q lacks the close failure", r.errb.String())
		}
	})
}

func TestBadConfigFailsBeforeAnyPrompt(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.production = true // the accounts as pcb opens them: config.toml read
	if err := os.WriteFile(config.Path(r.dirs), []byte("bogus = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	delete(r.env, "PCB_PASSWORD")
	r.term = &fakeTerm{interactive: true, lines: []string{"u"}, secrets: []string{"p"}}
	r.want(exitcode.MustAct, "login")
	if len(r.term.linePrompts)+len(r.term.secretPrompts) != 0 {
		t.Errorf("prompted before config.toml was checked: %v %v", r.term.linePrompts, r.term.secretPrompts)
	}
}

func TestDefaultServiceReadsConfigToml(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.production = true // the accounts point at the fake only through config.toml
	r.want(exitcode.OK, "status", "--json")
	if n := r.f.Calls("", "/auth/v4"); n != 0 {
		t.Errorf("status called Proton (%d)", n)
	}
}

// noInProcess makes opening the in-process accounts fail the test: the
// commands from here on must go through the daemon.
func noInProcess(t *testing.T, r *rig) {
	t.Helper()
	r.svc, r.hub = nil, nil
	r.open = func() (cli.Hub, error) {
		t.Error("the command opened the in-process accounts")
		return nil, errors.New("no in-process accounts in this test")
	}
}

// serveWaiting starts pcb serve --wait-for-login on the rig's accounts.
func serveWaiting(t *testing.T, r *rig) *serveRun {
	t.Helper()
	s := startServe(t, r, nil, "--wait-for-login", "--listen", "127.0.0.1:0")
	s.listening()
	return s
}

// waitForServe waits until the daemon's status shows the loop of the
// account address in state want, and returns that account's status.
func waitForServe(t *testing.T, r *rig, address string, want service.ServeState) ctl.AccountStatus {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	deadline := time.Now().Add(30 * time.Second)
	var last ctl.Status
	for {
		if c, err := ctl.Dial(t.Context(), r.dirs.Socket()); err == nil {
			st, err := c.Status(t.Context())
			_ = c.Close()
			if err == nil {
				last = st
				for _, as := range st.Accounts {
					if strings.EqualFold(as.Address, address) && as.Health != nil && as.Health.Serve == want {
						return as
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never showed %s %s: %+v", address, want, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const noDaemonNote = "Note: no pcb daemon is running; this command runs on its own.\n"

// M5-Rpre4: with a daemon, pcb login logs in through it (ruling R15: the
// running Hub would not see an account added in another process), and the
// account the login adds is served at once.
func TestLoginGoesThroughTheDaemon(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	r := newRig(t, &protontest.Options{})
	s := serveWaiting(t, r)
	noInProcess(t, r)
	r.want(exitcode.OK, "login", "--non-interactive")
	if !strings.Contains(r.out.String(), "Logged in as "+r.f.Username) || strings.Contains(r.errb.String(), "Note:") {
		t.Errorf("stdout %q, stderr %q; want the login without the no-daemon note", r.out.String(), r.errb.String())
	}
	waitForServe(t, r, r.f.Username, service.ServeServing)
	if n := r.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
	// The daemon's login created the default client (decision 0013): the
	// result says so across the socket, and the daemon logs it.
	if !strings.HasSuffix(r.out.String(), "CalDAV credentials: pcb client show\n") {
		t.Errorf("stdout %q; want the credentials line", r.out.String())
	}
	if log := s.errb.String(); !strings.Contains(log, `created the CalDAV client \"default\": see pcb client show`) {
		t.Errorf("daemon log = %q; want the default-client line", log)
	}
}

// With a daemon, pcb logout goes through it, and the account's loop goes
// back to waiting for a login (ruling M5-R8).
func TestLogoutGoesThroughTheDaemon(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive") // no daemon yet: in process
	serveWaiting(t, r)
	waitForServe(t, r, r.f.Username, service.ServeServing)
	noInProcess(t, r)
	r.want(exitcode.OK, "logout")
	if r.out.String() != "Logged out.\n" || strings.Contains(r.errb.String(), "Note:") {
		t.Errorf("stdout %q, stderr %q; want Logged out. without the no-daemon note", r.out.String(), r.errb.String())
	}
	waitForServe(t, r, r.f.Username, service.ServeWaitingForLogin)
	if n := r.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d DELETE /auth/v4, want 1 (the session revoked)", n)
	}
	r.want(exitcode.OK, "status", "--json", "--daemon")
	var st ctl.Status
	mustUnmarshal(t, r.envelope().Data, &st)
	if len(st.Accounts) != 1 || st.Accounts[0].Login.Phase != "logged-out" || st.Accounts[0].Health == nil ||
		st.Accounts[0].Health.Serve != service.ServeWaitingForLogin {
		t.Errorf("status after the logout = %+v", st)
	}
}

// Review Focus 3: without a terminal, the daemon holds the login waiting
// for its code; login resume --totp finishes it with the code from
// PCB_TOTP, without the credentials and without a second SRP.
func TestNonInteractiveTOTPIsHeldByTheDaemon(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	startDaemon(t, r, realHub(t, r), nil)
	noInProcess(t, r)
	r.want(exitcode.MustAct, "login", "--non-interactive", "--json")
	e := r.envelope()
	wantHint := "PCB_TOTP=<code> pcb login resume --totp before " + t0.Add(5*time.Minute).Local().Format(time.TimeOnly) + " (the daemon holds the login)"
	if e.Error == nil || e.Error.Message != "the account needs a 2FA code" || e.Error.Hint != wantHint {
		t.Fatalf("error = %+v; want the 2FA code asked for with the hint %q", e.Error, wantHint)
	}
	var held service.LoginResult
	mustUnmarshal(t, e.Data, &held)
	if held.State != service.StateNeedsTOTP || held.LoginExpires == nil {
		t.Errorf("data = %s; want needs-totp with login_expires", e.Data)
	}
	if n := r.f.Calls("DELETE", "/auth/v4"); n != 0 {
		t.Errorf("%d DELETE /auth/v4: the held login was cancelled", n)
	}

	delete(r.env, "PCB_USERNAME")
	delete(r.env, "PCB_PASSWORD")
	code := r.totpCode()
	r.env["PCB_TOTP"] = code
	r.want(exitcode.OK, "login", "resume", "--totp", "--non-interactive", "--json")
	var res service.LoginResult
	mustUnmarshal(t, r.envelope().Data, &res)
	if res.State != service.StateReady || !res.DefaultClientCreated {
		t.Fatalf("resume --totp = %+v, want ready with the default client created", res)
	}
	if n := r.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1 (none for the resume)", n)
	}
	if strings.Contains(r.out.String()+r.errb.String(), code) {
		t.Error("the output carries the 2FA code")
	}
}

// Interactive, with a daemon: the prompt answers the login the daemon
// holds, again after a rejected code.
func TestInteractiveTOTPThroughTheDaemon(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{TwoFA: true})
	startDaemon(t, r, realHub(t, r), nil)
	noInProcess(t, r)
	r.term = &fakeTerm{interactive: true, secrets: []string{"000000", r.totpCode()}}
	r.want(exitcode.OK, "login")
	if len(r.term.secretPrompts) != 2 || !strings.Contains(r.errb.String(), "rejected") {
		t.Errorf("%d prompts, stderr %q; want two, the first code reported rejected", len(r.term.secretPrompts), r.errb.String())
	}
	if n := r.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
}

func TestResumeTOTPWithoutADaemonIsExit10(t *testing.T) {
	t.Parallel()
	s := &stubAPI{}
	r := stubRig(t, s)
	r.env["PCB_TOTP"] = "123456"
	r.want(exitcode.MustAct, "login", "resume", "--totp", "--json")
	e := r.envelope()
	if e.Error == nil || e.Error.Message != "no pcb daemon holds a login waiting for its code" || e.Error.Hint != "run pcb login with PCB_TOTP set" {
		t.Errorf("error = %+v", e.Error)
	}
	if len(s.totps)+len(s.logins) != 0 {
		t.Errorf("the in-process service got %d codes and %d logins", len(s.totps), len(s.logins))
	}
}

// M5-P11: resume takes exactly one of --captcha-token and --totp (a flag:
// the code never comes from argv).
func TestResumeNeedsExactlyOneMode(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"login", "resume"},
		{"login", "resume", "--totp", "--captcha-token", "T1"},
		{"login", "resume", "--totp", "123456"},
	} {
		s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		r := stubRig(t, s)
		r.env["PCB_TOTP"] = "123456"
		r.want(exitcode.Usage, append(args, "--json")...)
		if len(s.logins)+len(s.totps) != 0 {
			t.Errorf("pcb %s reached the service", strings.Join(args, " "))
		}
	}
}

// Without a daemon, pcb login and pcb logout run in process and say so on
// stderr, in text mode only.
func TestNoDaemonLoginSaysSo(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive")
	if r.errb.String() != noDaemonNote || !strings.Contains(r.out.String(), "Logged in as") {
		t.Errorf("login: stdout %q, stderr %q; want the note on stderr", r.out.String(), r.errb.String())
	}
	r.want(exitcode.OK, "logout", "--json")
	if strings.Contains(r.errb.String()+r.out.String(), "Note:") {
		t.Errorf("logout --json: stdout %q, stderr %q; want no note", r.out.String(), r.errb.String())
	}
	r.want(exitcode.OK, "logout")
	if r.errb.String() != noDaemonNote || r.out.String() != "Not logged in.\n" {
		t.Errorf("logout: stdout %q, stderr %q; want the note on stderr", r.out.String(), r.errb.String())
	}
	delete(r.env, "PCB_PASSWORD") // fails before Proton: the login guard holds a second login back
	r.want(exitcode.MustAct, "login", "--non-interactive", "--json")
	if strings.Contains(r.errb.String()+r.out.String(), "Note:") {
		t.Errorf("login --json: stdout %q, stderr %q; want no note", r.out.String(), r.errb.String())
	}
}

// Ruling M5-R12 (6): the credentials' flags do not apply to resume --totp
// (the daemon holds the login they made); they are refused, not ignored.
func TestResumeTOTPRefusesCredentialFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		flag string
		args []string
	}{
		{"--username", []string{"--username", "alice"}},
		{"--password-fd", []string{"--password-fd", "3"}},
		{"--unattended-relogin", []string{"--unattended-relogin"}},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			t.Parallel()
			s := &stubAPI{}
			r := stubRig(t, s)
			r.env["PCB_TOTP"] = "123456"
			r.want(exitcode.Usage, append([]string{"login", "resume", "--totp", "--json"}, tc.args...)...)
			if e := r.envelope().Error; e == nil || !strings.Contains(e.Message, tc.flag) || !strings.Contains(e.Message, "--totp") {
				t.Errorf("error = %+v; want it to name %s and --totp", e, tc.flag)
			}
			if len(s.totps)+len(s.logins) != 0 {
				t.Error("the command reached the service")
			}
		})
	}
}

// The no-daemon note follows the in-process service's opening: a directory
// error is not preceded by it.
func TestNoDaemonNoteNotBeforeADirectoryError(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.production = true // the accounts as pcb opens them, which resolves the directories
	r.env["PCB_STATE_DIR"] = "relative/state"
	for _, args := range [][]string{{"login", "--non-interactive"}, {"logout"}} {
		r.want(exitcode.Blocked, args...)
		if strings.Contains(r.errb.String(), "Note:") || !strings.Contains(r.errb.String(), "PCB_STATE_DIR") {
			t.Errorf("pcb %s: stderr %q; want the directory error without the no-daemon note", strings.Join(args, " "), r.errb.String())
		}
	}
}

// Ruling A2 (1): outside pcb serve, the service's warnings reach stderr in
// pcb's message style (here: a last login time in the future), and never
// stdout, so a --json envelope stays alone.
func TestCommandsPrintTheServiceWarningsOnStderr(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.production = true // the accounts as pcb opens them (real clock)
	stamp := filepath.Join(r.dirs.State, "last-login")
	future := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	for _, args := range [][]string{{"login", "--non-interactive"}, {"login", "--non-interactive", "--json"}} {
		if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stamp, []byte(future.Format(time.RFC3339Nano)), 0o600); err != nil {
			t.Fatal(err)
		}
		r.want(exitcode.OK, args...)
		want := "Warning: the last login time is in the future: the clock was set back; ignoring it (file: " + stamp +
			", last_login: " + future.Format(time.RFC3339) + ", now: "
		got := strings.TrimPrefix(r.errb.String(), noDaemonNote)
		if !strings.HasPrefix(got, want) || !strings.HasSuffix(strings.SplitN(got, "\n", 2)[0], ")") || strings.Count(got, "Warning:") != 1 {
			t.Errorf("pcb %s: stderr %q, want one line starting %q", strings.Join(args, " "), got, want)
		}
		if args[len(args)-1] == "--json" {
			if e := r.envelope(); !e.OK {
				t.Errorf("envelope = %+v", e)
			}
		} else if strings.Contains(r.out.String(), "Warning") {
			t.Errorf("stdout %q holds the warning", r.out.String())
		}
	}
}
