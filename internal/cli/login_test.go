package cli_test

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
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
	r.mk = nil // the default service: it reads config.toml
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
	r.mk = nil // the default service points at the fake only through config.toml
	r.want(exitcode.OK, "status", "--json")
	if n := r.f.Calls("", "/auth/v4"); n != 0 {
		t.Errorf("status called Proton (%d)", n)
	}
}
