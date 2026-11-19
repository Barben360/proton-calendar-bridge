package service_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/keys"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// keysOpen reports whether the sealed KeyPass of a opens every address key
// of the fake's account (fetched with a's own session).
func keysOpen(t *testing.T, e *env, a store.Account) bool {
	t.Helper()
	c := api.New(tokensOf(a), api.Options{BaseURL: e.f.URL, HTTP: &http.Client{Timeout: 30 * time.Second}})
	user, addrs, err := keys.Fetch(t.Context(), c)
	if err != nil {
		t.Fatalf("keys.Fetch: %v", err)
	}
	u, err := keys.Unlock(user, addrs, a.KeyPass)
	if err != nil {
		return false
	}
	defer u.Close()
	return len(u.AddrKRs) == len(addrs)
}

// noSecrets fails when anything is sealed.
func noSecrets(t *testing.T, e *env) {
	t.Helper()
	if accs, err := e.secrets().Load(); err != nil || len(accs) != 0 {
		t.Errorf("secrets = %d accounts, %v; want nothing sealed", len(accs), err)
	}
}

func TestTwoPasswordUpfrontReady(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoPassword: true})
	r := e.req()
	r.MailboxPassword = e.f.MailboxPassword
	res := e.login(t, r)
	if res.State != service.StateReady || res.AccountID != e.f.UserID(e.f.Username) {
		t.Fatalf("Login = %+v, want ready", res)
	}
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
	if st := e.loadState(t); st.Phase != store.PhaseReady {
		t.Errorf("state phase = %q, want ready", st.Phase)
	}
}

func TestTwoPasswordHeldThenSubmitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoPassword: true})
	res := e.login(t, e.req())
	want := t0.Add(5 * time.Minute)
	if res.State != service.StateNeedsMailboxPassword || res.LoginExpires == nil || !res.LoginExpires.Equal(want) ||
		res.AccountID != e.f.UserID(e.f.Username) || res.Username != e.f.Username {
		t.Fatalf("Login = %+v; want needs-mailbox-password expiring at %v", res, want)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Fatal("secrets sealed before the mailbox password was given")
	}
	st, err := e.svc.Status(t.Context())
	if err != nil || st.Phase != "needs-mailbox-password" || st.Reason != "a login is waiting for its mailbox password" ||
		st.LoginExpires == nil || !st.LoginExpires.Equal(want) {
		t.Fatalf("Status = %+v, %v; want needs-mailbox-password until %v", st, err, want)
	}
	res, err = e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	if err != nil || res.State != service.StateReady || res.AccountID != e.f.UserID(e.f.Username) {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
	}
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
	if st, _ := e.svc.Status(t.Context()); st.Phase != "ready" || st.LoginExpires != nil {
		t.Errorf("Status after the mailbox password = %+v; want ready", st)
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
}

func TestWrongMailboxPasswordThenRight(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoPassword: true})
	e.login(t, e.req())
	before := e.f.Calls("", "/")
	// An empty one is refused without spending a try.
	_, err := e.svc.SubmitMailboxPassword(t.Context(), "")
	_ = wantExit(t, err, exitcode.Usage)
	_, err = e.svc.SubmitMailboxPassword(t.Context(), "not-the-mailbox-password")
	ee := wantExit(t, err, exitcode.MustAct)
	if ee.Error() != "the mailbox password does not unlock the account keys" || !strings.Contains(ee.Hint, "2 tries left") {
		t.Errorf("wrong mailbox password = %q (hint %q)", ee.Error(), ee.Hint)
	}
	if n := e.f.Calls("", "/"); n != before {
		t.Errorf("%d calls to the fake for a wrong mailbox password, want 0", n-before)
	}
	if st, _ := e.svc.Status(t.Context()); st.Phase != "needs-mailbox-password" {
		t.Errorf("phase after a wrong mailbox password = %q, want needs-mailbox-password", st.Phase)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("secrets sealed after a wrong mailbox password")
	}
	res, err := e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1 (no second SRP)", n)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 0 {
		t.Errorf("%d revocations, want 0", n)
	}
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
}

func TestThreeWrongMailboxPasswordsCancel(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoPassword: true})
	e.login(t, e.req())
	for _, left := range []string{"2 tries left", "1 tries left"} {
		_, err := e.svc.SubmitMailboxPassword(t.Context(), "wrong")
		if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, left) {
			t.Errorf("hint = %q, want %q", ee.Hint, left)
		}
	}
	_, err := e.svc.SubmitMailboxPassword(t.Context(), "wrong")
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Error(), "cancelled") || ee.Hint != "run pcb login again" {
		t.Errorf("third wrong mailbox password = %q (hint %q)", ee.Error(), ee.Hint)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d revocations, want 1", n)
	}
	noSecrets(t, e)
	if st, _ := e.svc.Status(t.Context()); st.Phase == "needs-mailbox-password" {
		t.Error("Status still reports a login waiting")
	}
	// Nothing waits any more, even with the right password.
	_, err = e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "no login is waiting for a mailbox password" {
		t.Errorf("after the cancel = %q", ee.Error())
	}
	noSecrets(t, e)
}

func TestTOTPThenMailboxPassword(t *testing.T) {
	t.Parallel()
	ft := &fakeTimers{}
	e := newEnvWith(t, protontest.Options{TwoFA: true, TwoPassword: true}, ft.AfterFunc)
	res := e.login(t, e.req())
	want := t0.Add(5 * time.Minute)
	if res.State != service.StateNeedsTOTP {
		t.Fatalf("Login = %+v, want needs-totp", res)
	}
	e.clk.advance(time.Minute)
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, e.clk.Now()))
	if err != nil || res.State != service.StateNeedsMailboxPassword || res.LoginExpires == nil || !res.LoginExpires.Equal(want) {
		t.Fatalf("SubmitTOTP = %+v, %v; want needs-mailbox-password still expiring at %v", res, err, want)
	}
	if st, _ := e.svc.Status(t.Context()); st.Phase != "needs-mailbox-password" || st.LoginExpires == nil || !st.LoginExpires.Equal(want) {
		t.Errorf("Status = %+v; want needs-mailbox-password until %v", st, want)
	}
	if n := ft.count(); n != 1 {
		t.Errorf("%d expiry timers, want 1 (one deadline for the whole login)", n)
	}
	res, err = e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
	}
	if n := e.f.Calls("POST", "/auth/v4/2fa"); n != 1 {
		t.Errorf("%d 2FA submissions, want 1", n)
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
	if ft.timer(t, 0).fire() {
		t.Error("the expiry timer was left armed after the login completed")
	}
}

func TestUpfrontMailboxPasswordSurvivesTOTPHold(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true, TwoPassword: true})
	r := e.req()
	r.MailboxPassword = e.f.MailboxPassword
	if res := e.login(t, r); res.State != service.StateNeedsTOTP {
		t.Fatalf("Login = %+v, want needs-totp", res)
	}
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0))
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitTOTP = %+v, %v; want ready directly", res, err)
	}
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
}

func TestSinglePasswordIgnoresMailboxPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	r := e.req()
	r.MailboxPassword = "a-mailbox-password-this-account-does-not-use"
	if res := e.login(t, r); res.State != service.StateReady {
		t.Fatalf("Login = %+v, want ready", res)
	}
	// The fake's keys open only with the passphrase of the login password.
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
}

func TestSubmitOnTheWrongStepIsMustAct(t *testing.T) {
	t.Parallel()
	t.Run("2FA code while the mailbox password is awaited", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, protontest.Options{TwoPassword: true})
		e.login(t, e.req())
		_, err := e.svc.SubmitTOTP(t.Context(), "123456")
		if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "no login is waiting for a 2FA code" || !strings.Contains(ee.Hint, "mailbox password") {
			t.Errorf("SubmitTOTP = %q (hint %q)", ee.Error(), ee.Hint)
		}
		if n := e.f.Calls("POST", "/auth/v4/2fa"); n != 0 {
			t.Errorf("%d 2FA submissions, want 0", n)
		}
		if res, err := e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword); err != nil || res.State != service.StateReady {
			t.Fatalf("the held login did not survive: %+v, %v", res, err)
		}
	})
	t.Run("mailbox password while the 2FA code is awaited", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, protontest.Options{TwoFA: true, TwoPassword: true})
		e.login(t, e.req())
		_, err := e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
		if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "no login is waiting for a mailbox password" || !strings.Contains(ee.Hint, "2FA code") {
			t.Errorf("SubmitMailboxPassword = %q (hint %q)", ee.Error(), ee.Hint)
		}
		if st, _ := e.svc.Status(t.Context()); st.Phase != "needs-totp" {
			t.Errorf("phase = %q, want needs-totp", st.Phase)
		}
		res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0))
		if err != nil || res.State != service.StateNeedsMailboxPassword {
			t.Fatalf("the held login did not survive: %+v, %v", res, err)
		}
	})
	t.Run("nothing held", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t, protontest.Options{})
		_, err := e.svc.SubmitMailboxPassword(t.Context(), "anything")
		if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "no login is waiting for a mailbox password" || ee.Hint != "run pcb login first" {
			t.Errorf("SubmitMailboxPassword = %q (hint %q)", ee.Error(), ee.Hint)
		}
	})
}

func TestMailboxWaitExpires(t *testing.T) {
	t.Parallel()
	ft := &fakeTimers{}
	e := newEnvWith(t, protontest.Options{TwoPassword: true}, ft.AfterFunc)
	e.login(t, e.req())
	if n := ft.count(); n != 1 || ft.timer(t, 0).d != 5*time.Minute {
		t.Fatalf("%d timers, want one of 5 min", n)
	}
	e.clk.advance(5 * time.Minute)
	if !ft.timer(t, 0).fire() {
		t.Fatal("the expiry timer was stopped before it fired")
	}
	_, err := e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	ee := wantExit(t, err, exitcode.MustAct)
	if ee.Error() != "the login waiting for its mailbox password expired after 5 minutes" || ee.Hint != "run pcb login again" {
		t.Errorf("SubmitMailboxPassword after expiry = %q (hint %q)", ee.Error(), ee.Hint)
	}
	// SubmitTOTP says the same: the login that expired waited for its mailbox password.
	_, err = e.svc.SubmitTOTP(t.Context(), "123456")
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "the login waiting for its mailbox password expired after 5 minutes" {
		t.Errorf("SubmitTOTP after expiry = %q", ee.Error())
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d DELETE /auth/v4, want 1 (the half session revoked)", n)
	}
	if st, _ := e.svc.Status(t.Context()); st.Phase == "needs-mailbox-password" || st.LoginExpires != nil {
		t.Errorf("Status after expiry = %+v; want no login waiting", st)
	}
	noSecrets(t, e)
}

func TestMailboxPasswordNeverStored(t *testing.T) {
	t.Parallel()
	const mailbox = "mailbox-secret-7f3a9c"
	e := newEnv(t, protontest.Options{TwoPassword: true, MailboxPassword: mailbox})
	r := e.req()
	r.MailboxPassword = mailbox
	r.UnattendedRelogin = true
	if res := e.login(t, r); res.State != service.StateReady {
		t.Fatalf("Login = %+v, want ready", res)
	}
	accs := e.accounts(t)
	if len(accs) != 1 {
		t.Fatalf("%d sealed accounts, want 1", len(accs))
	}
	if a := e.account(t); a.Relogin == nil || a.Relogin.Password != e.f.Password {
		t.Errorf("Relogin = %+v; want the login password sealed for unattended relogin", a.Relogin)
	}
	sealed, err := json.Marshal(accs)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(mailbox)) {
		t.Error("the sealed store contains the mailbox password")
	}
	raw, err := os.ReadFile(filepath.Join(e.dirs.State, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(mailbox)) {
		t.Error("state.json contains the mailbox password")
	}
	if s := fmt.Sprintf("%v %+v %#v %s", r, r, r, r); strings.Contains(s, mailbox) {
		t.Errorf("fmt shows the mailbox password: %s", s)
	}
	var line bytes.Buffer
	slog.New(slog.NewJSONHandler(&line, nil)).Info("login", "request", r)
	if strings.Contains(line.String(), mailbox) {
		t.Errorf("slog shows the mailbox password: %s", line.String())
	}
}

// leaks lists where secrets appear in plaintext: in the log, or in any file
// under the config and state directories (the cache and the logs live in
// the state directory).
func leaks(t *testing.T, e *env, logs string, secrets map[string]string) []string {
	t.Helper()
	var out []string
	for name, secret := range secrets {
		if strings.Contains(logs, secret) {
			out = append(out, name+" in the log")
		}
	}
	files := 0
	for _, root := range []string{e.dirs.Config, e.dirs.State} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files++
			for name, secret := range secrets {
				if strings.Contains(path, secret) || bytes.Contains(b, []byte(secret)) {
					out = append(out, name+" in "+path)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if files == 0 {
		t.Fatal("no file under the config and state directories: the scan proves nothing")
	}
	slices.Sort(out)
	return out
}

// A login that goes through a CAPTCHA, a TOTP code and a mailbox password
// leaves none of them, nor the login password, in the log or in a file of
// the config and state directories: the sealed store is encrypted, and
// nothing else keeps them.
func TestSecretsNeverReachTheLogsOrTheDisk(t *testing.T) {
	t.Parallel()
	const (
		mailbox = "mailbox-secret-7f3a9c"
		captcha = "chal-secret-4d21e8"
	)
	e := newEnv(t, protontest.Options{TwoFA: true, TwoPassword: true, MailboxPassword: mailbox, Captcha: captcha})
	logs := &logBuffer{}
	e.logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := e.svc.Close(); err != nil {
		t.Fatal(err)
	}
	e.svc = e.open(t)
	totpURI := e.f.TOTPURI
	_, totpSecret, _ := strings.Cut(totpURI, "secret=")
	secrets := map[string]string{
		"the mailbox password": mailbox,
		"the CAPTCHA token":    captcha,
		"the TOTP secret":      totpSecret,
		"the login password":   e.f.Password,
	}
	if totpSecret == "" {
		t.Fatalf("no TOTP secret in %q", totpURI)
	}
	// While Proton's challenge waits, its token is in state.json by design
	// (the window shows it); once solved and used it must be gone.
	check := func(step string) {
		t.Helper()
		scan := maps.Clone(secrets)
		if step == "the CAPTCHA was asked" {
			delete(scan, "the CAPTCHA token")
		}
		if got := leaks(t, e, logs.String(), scan); len(got) != 0 {
			t.Errorf("after %s: %s", step, strings.Join(got, "; "))
		}
	}

	res := e.login(t, e.req())
	if res.State != service.StateNeedsCaptcha || res.CaptchaToken != captcha {
		t.Fatalf("Login = %+v, want the CAPTCHA", res)
	}
	check("the CAPTCHA was asked")
	e.clk.advance(30 * time.Second)
	r := e.req()
	r.CaptchaToken, r.TOTP, r.UnattendedRelogin = captcha, totpURI, true
	if res = e.login(t, r); res.State != service.StateNeedsMailboxPassword {
		t.Fatalf("Login with the solved CAPTCHA and the TOTP = %+v, want needs-mailbox-password", res)
	}
	check("the mailbox password was asked")
	if res, err := e.svc.SubmitMailboxPassword(t.Context(), mailbox); err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
	}
	if a := e.account(t); a.Relogin == nil || a.Relogin.Password != e.f.Password || a.Relogin.TOTPURI != totpURI {
		t.Fatalf("Relogin = %+v; want the password and the TOTP sealed, so that the scan has something to find", a.Relogin)
	}
	check("the login was ready")
	if logs.String() == "" {
		t.Error("the log is empty: the log scan proves nothing")
	}
}

func TestNonASCIIMailboxPassword(t *testing.T) {
	t.Parallel()
	const mailbox = "mot-de-passe-é"
	e := newEnv(t, protontest.Options{TwoPassword: true, MailboxPassword: mailbox})
	if res := e.login(t, e.req()); res.State != service.StateNeedsMailboxPassword {
		t.Fatalf("Login = %+v, want needs-mailbox-password", res)
	}
	res, err := e.svc.SubmitMailboxPassword(t.Context(), mailbox)
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
	}
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
}

// A wrong mailbox password given up front counts as the first try: the
// login is held for another one, without a second SRP.
func TestWrongUpfrontMailboxPasswordIsHeld(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoPassword: true})
	r := e.req()
	r.MailboxPassword = "not-the-mailbox-password"
	_, err := e.svc.Login(t.Context(), r)
	ee := wantExit(t, err, exitcode.MustAct)
	if ee.Error() != "the mailbox password does not unlock the account keys" || !strings.Contains(ee.Hint, "2 tries left") {
		t.Errorf("Login = %q (hint %q)", ee.Error(), ee.Hint)
	}
	if st, _ := e.svc.Status(t.Context()); st.Phase != "needs-mailbox-password" {
		t.Fatalf("phase = %q, want needs-mailbox-password", st.Phase)
	}
	res, err := e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
}

// A wrong mailbox password given up front to a 2FA account waits through the
// 2FA hold, then counts as the first try once the code passes.
func TestWrongUpfrontMailboxPasswordDuringTOTPHold(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true, TwoPassword: true})
	r := e.req()
	r.MailboxPassword = "not-the-mailbox-password"
	if res := e.login(t, r); res.State != service.StateNeedsTOTP {
		t.Fatalf("Login = %+v, want needs-totp", res)
	}
	_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0))
	ee := wantExit(t, err, exitcode.MustAct)
	if ee.Error() != "the mailbox password does not unlock the account keys" || !strings.Contains(ee.Hint, "2 tries left") {
		t.Errorf("SubmitTOTP = %q (hint %q)", ee.Error(), ee.Hint)
	}
	if st, _ := e.svc.Status(t.Context()); st.Phase != "needs-mailbox-password" {
		t.Fatalf("phase = %q, want needs-mailbox-password", st.Phase)
	}
	res, err := e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitMailboxPassword = %+v, %v; want ready", res, err)
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
	if !keysOpen(t, e, e.account(t)) {
		t.Error("the sealed KeyPass does not open the account keys")
	}
}

// Abandoning a login held for its mailbox password revokes its session.
func TestAbandonAtMailboxStepRevokes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoPassword: true})
	if res := e.login(t, e.req()); res.State != service.StateNeedsMailboxPassword {
		t.Fatalf("Login = %+v, want needs-mailbox-password", res)
	}
	e.svc.AbandonLogin(t.Context())
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d DELETE /auth/v4, want 1 (the held session revoked)", n)
	}
	_, err := e.svc.SubmitMailboxPassword(t.Context(), e.f.MailboxPassword)
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "no login is waiting for a mailbox password" {
		t.Errorf("SubmitMailboxPassword after abandon = %q", ee.Error())
	}
	noSecrets(t, e)
}
