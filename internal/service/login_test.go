package service_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

var t0 = time.Date(2026, 11, 11, 10, 0, 7, 0, time.UTC)

// clock is the injected time: Now for the service and the fake's TOTP, Sleep
// records what the service waited for without waiting.
type clock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
	// onSleep, when set, runs on every Sleep (the clock does not move by itself).
	onSleep func(d time.Duration)
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *clock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	return nil
}

type env struct {
	f    *protontest.Fake
	clk  *clock
	dirs paths.Dirs
	cfg  config.Config
	svc  *service.Local
}

func newEnv(t *testing.T, fo protontest.Options) *env {
	t.Helper()
	e := &env{clk: &clock{now: t0}}
	fo.Now = e.clk.Now
	e.f = protontest.New(t, fo)
	root := t.TempDir()
	e.dirs = paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	e.cfg = config.Defaults(e.dirs)
	e.cfg.Proton.BaseURL = e.f.URL
	e.svc = e.open(t)
	return e
}

// open starts a service on the env's dirs, closed with the test.
func (e *env) open(t *testing.T) *service.Local {
	t.Helper()
	s, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (e *env) secrets() *store.Secrets {
	return store.NewSecrets(e.dirs.State, &seal.KeyFile{Path: e.cfg.Secrets.KeyFile})
}

func (e *env) accounts(t *testing.T) map[string]store.Account {
	t.Helper()
	a, err := e.secrets().Load()
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *env) loadState(t *testing.T) store.State {
	t.Helper()
	s, err := store.LoadState(e.dirs.State)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) req() service.LoginRequest {
	return service.LoginRequest{Username: e.f.Username, Password: e.f.Password}
}

func (e *env) login(t *testing.T, r service.LoginRequest) service.LoginResult {
	t.Helper()
	res, err := e.svc.Login(t.Context(), r)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return res
}

func (e *env) account(t *testing.T) store.Account {
	t.Helper()
	a, ok := e.accounts(t)[e.f.UserID(e.f.Username)]
	if !ok {
		t.Fatal("the sealed store has no account for the fake's user")
	}
	return a
}

func tokensOf(a store.Account) api.Tokens {
	return api.Tokens{UID: a.UID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken}
}

func code(t *testing.T, uri string, at time.Time) string {
	t.Helper()
	o, err := otp.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	return o.Code(at)
}

func wantExit(t *testing.T, err error, c exitcode.Code) *exitcode.Error {
	t.Helper()
	var ee *exitcode.Error
	if !errors.As(err, &ee) {
		t.Fatalf("error = %#v, want an *exitcode.Error", err)
	}
	if ee.Code != c {
		t.Fatalf("exit code = %d (%v), want %d", ee.Code, err, c)
	}
	return ee
}

// sessionAlive reports whether tok still authenticates on the fake.
func sessionAlive(t *testing.T, e *env, tok api.Tokens) bool {
	t.Helper()
	c := api.New(tok, api.Options{BaseURL: e.f.URL, HTTP: &http.Client{Timeout: 5 * time.Second}})
	return c.Get(t.Context(), "/core/v4/users", nil, nil) == nil
}

func TestLoginPasswordOnlyReady(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	res := e.login(t, e.req())
	if res.State != service.StateReady || res.AccountID != e.f.UserID(e.f.Username) || res.Username != e.f.Username {
		t.Fatalf("result = %+v", res)
	}
	a := e.account(t)
	if a.Username != e.f.Username || a.UID == "" || a.AccessToken == "" || a.RefreshToken == "" || len(a.KeyPass) == 0 {
		t.Errorf("sealed account is incomplete: username %q, uid %t, access %t, refresh %t, keypass %d bytes",
			a.Username, a.UID != "", a.AccessToken != "", a.RefreshToken != "", len(a.KeyPass))
	}
	if a.Relogin != nil {
		t.Error("Relogin is stored without the opt-in")
	}
	st := e.loadState(t)
	if st.Phase != store.PhaseReady || st.AccountID != res.AccountID || !st.LastLogin.Equal(t0) || st.Captcha != nil || st.Unattended {
		t.Errorf("state = %+v", st)
	}
}

// A refresh during the key calls rotates the tokens: the sealed ones must be
// the fresh pair, not the spent refresh token SRP returned.
func TestLoginSealsTokensRefreshedDuringKeyCalls(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.FailNext("GET", "/core/v4/keys/salts", 401, 401, 0)
	e.login(t, e.req())
	if n := e.f.Calls("POST", "/auth/v4/refresh"); n != 1 {
		t.Fatalf("%d refreshes during the login, want 1", n)
	}
	if !sessionAlive(t, e, tokensOf(e.account(t))) {
		t.Error("the sealed session is not the live one")
	}
}

func TestLoginWithTOTPURI(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	r := e.req()
	r.TOTP = e.f.TOTPURI
	if res := e.login(t, r); res.State != service.StateReady {
		t.Fatalf("state = %q, want ready", res.State)
	}
	if n := e.f.Calls("POST", "/auth/v4/2fa"); n != 1 {
		t.Errorf("%d 2FA requests, want 1", n)
	}
	if e.account(t).UID == "" {
		t.Error("nothing sealed")
	}
}

func TestLoginWithStaticTOTPCode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	r := e.req()
	r.TOTP = code(t, e.f.TOTPURI, t0)
	if res := e.login(t, r); res.State != service.StateReady {
		t.Fatalf("state = %q, want ready", res.State)
	}
}

func TestLoginNeedsTOTPThenSubmit(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	res := e.login(t, e.req())
	if res.State != service.StateNeedsTOTP {
		t.Fatalf("state = %q, want needs-totp", res.State)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Fatal("secrets sealed before the code was verified")
	}
	if st, err := e.svc.Status(t.Context()); err != nil || st.Phase != "needs-totp" {
		t.Errorf("Status = %+v, %v; want phase needs-totp", st, err)
	}
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0))
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitTOTP = %+v, %v", res, err)
	}
	if e.account(t).KeyPass == nil {
		t.Error("nothing sealed after SubmitTOTP")
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want exactly 1 (no second SRP)", n)
	}
	if st, _ := e.svc.Status(t.Context()); st.Phase != "ready" {
		t.Errorf("phase after SubmitTOTP = %q, want ready", st.Phase)
	}
}

func TestSubmitTOTPWithoutPendingIsMustAct(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	_, err := e.svc.SubmitTOTP(t.Context(), "123456")
	if ee := wantExit(t, err, exitcode.MustAct); ee.Hint == "" {
		t.Error("no hint")
	}
}

func TestSubmitTOTPMalformedIsUsage(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	e.login(t, e.req())
	_, err := e.svc.SubmitTOTP(t.Context(), "not a code")
	_ = wantExit(t, err, exitcode.Usage)
	// The pending login survives a malformed code.
	if _, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0)); err != nil {
		t.Fatalf("SubmitTOTP after a malformed code: %v", err)
	}
}

func TestTOTPRetryKeepsPendingLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	e.login(t, e.req())
	for _, left := range []string{"2 tries left", "1 tries left"} {
		_, err := e.svc.SubmitTOTP(t.Context(), "000000")
		if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "enter the current code again") || !strings.Contains(ee.Hint, left) {
			t.Errorf("hint = %q, want it to say %q", ee.Hint, left)
		}
	}
	res, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0))
	if err != nil || res.State != service.StateReady {
		t.Fatalf("SubmitTOTP = %+v, %v", res, err)
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1 (no new login)", n)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 0 {
		t.Errorf("%d revocations, want 0", n)
	}
}

func TestTOTPThirdRejectionAbortsSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	e.login(t, e.req())
	for range 2 {
		if _, err := e.svc.SubmitTOTP(t.Context(), "000000"); err == nil {
			t.Fatal("a wrong code was accepted")
		}
	}
	_, err := e.svc.SubmitTOTP(t.Context(), "000000")
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Error(), "cancelled") {
		t.Errorf("message = %q, want it to say the login was cancelled", ee.Error())
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d revocations, want 1", n)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("secrets sealed after rejected codes")
	}
	// Nothing is pending any more.
	_, err = e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0))
	_ = wantExit(t, err, exitcode.MustAct)
	if st, _ := e.svc.Status(t.Context()); st.Phase == "needs-totp" {
		t.Error("Status still reports a pending login")
	}
}

// failStateOnSleep makes the next state.json write fail: during the 2FA wait
// (after SRP, before the sealing) it replaces state.json by a directory. The
// clock is placed 3 s before a TOTP period ends so the wait happens.
func failStateOnSleep(t *testing.T, e *env) {
	t.Helper()
	e.clk.advance(80 * time.Second) // 10:01:27, 3 s before the period ends
	e.clk.onSleep = func(d time.Duration) {
		e.clk.advance(d)
		p := filepath.Join(e.dirs.State, "state.json")
		if err := os.RemoveAll(p); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Error(err)
		}
	}
}

func TestStateWriteFailureKeepsPreviousSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	r := e.req()
	r.TOTP = e.f.TOTPURI
	e.login(t, r)
	before := e.account(t)
	failStateOnSleep(t, e)
	_, err := e.svc.Login(t.Context(), r)
	_ = wantExit(t, err, exitcode.Blocked)
	after := e.account(t)
	if after.UID != before.UID || after.AccessToken != before.AccessToken || string(after.KeyPass) != string(before.KeyPass) {
		t.Error("the stored account changed after a failed attempt")
	}
	if !sessionAlive(t, e, tokensOf(before)) {
		t.Error("the previously stored session was revoked")
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d revocations, want 1 (the new session only)", n)
	}
}

func TestStateWriteFailureOnFirstLoginSealsNothing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	failStateOnSleep(t, e)
	r := e.req()
	r.TOTP = e.f.TOTPURI
	_, err := e.svc.Login(t.Context(), r)
	_ = wantExit(t, err, exitcode.Blocked)
	if accs, lerr := e.secrets().Load(); lerr != nil || len(accs) != 0 {
		t.Errorf("secrets = %d accounts, %v; want an empty store", len(accs), lerr)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d revocations, want 1", n)
	}
}

func TestLoginCaptchaRecordsState(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{Captcha: "chal-123"})
	res := e.login(t, e.req())
	if res.State != service.StateNeedsCaptcha || res.CaptchaToken != "chal-123" || !strings.Contains(res.CaptchaURL, "verify.proton.me") || !strings.Contains(res.CaptchaURL, "chal-123") {
		t.Fatalf("result = %+v", res)
	}
	st := e.loadState(t)
	if st.Phase != store.PhaseNeedsCaptcha || st.Captcha == nil || st.Captcha.Token != "chal-123" || st.Captcha.URL != res.CaptchaURL {
		t.Errorf("state = %+v", st)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("secrets sealed on a CAPTCHA")
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("%d POST /auth/v4, want 1", n)
	}
}

func TestCaptchaResumeWaitsOutGuard(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{Captcha: "chal-123"})
	res := e.login(t, e.req())
	e.clk.advance(10 * time.Second)
	r := e.req()
	r.CaptchaToken = res.CaptchaToken
	res = e.login(t, r)
	if res.State != service.StateReady {
		t.Fatalf("state = %q, want ready", res.State)
	}
	if len(e.clk.slept) != 1 || e.clk.slept[0] != 50*time.Second {
		t.Errorf("Sleep calls = %v, want [50s]", e.clk.slept)
	}
	if st := e.loadState(t); st.Phase != store.PhaseReady || st.Captcha != nil {
		t.Errorf("state = %+v, want ready without captcha", st)
	}
	st, _ := e.svc.Status(t.Context())
	if st.CaptchaURL != "" {
		t.Errorf("Status.CaptchaURL = %q after the CAPTCHA was solved", st.CaptchaURL)
	}
}

func TestLoginTooSoonIsBlocked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	before := e.f.Calls("POST", "/auth/v4")
	e.clk.advance(10 * time.Second)
	_, err := e.svc.Login(t.Context(), e.req())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Hint != "retry in 50s" {
		t.Errorf("hint = %q, want %q", ee.Hint, "retry in 50s")
	}
	if n := e.f.Calls("POST", "/auth/v4"); n != before {
		t.Errorf("%d new POST /auth/v4, want none", n-before)
	}
	if len(e.clk.slept) != 0 {
		t.Errorf("Sleep calls = %v without a CAPTCHA token", e.clk.slept)
	}
}

func TestUnattendedStoresSeed(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	r := e.req()
	r.TOTP, r.UnattendedRelogin = e.f.TOTPURI, true
	e.login(t, r)
	a := e.account(t)
	if a.Relogin == nil || a.Relogin.Password != e.f.Password || a.Relogin.TOTPURI != e.f.TOTPURI {
		t.Errorf("Relogin = %+v", a.Relogin)
	}
	if !e.loadState(t).Unattended {
		t.Error("state.Unattended = false")
	}
	if st, _ := e.svc.Status(t.Context()); !st.Unattended {
		t.Error("Status.Unattended = false")
	}
}

func TestUnattendedRefusesStaticCode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	r := e.req()
	r.TOTP, r.UnattendedRelogin = code(t, e.f.TOTPURI, t0), true
	_, err := e.svc.Login(t.Context(), r)
	ee := wantExit(t, err, exitcode.Usage)
	if !strings.Contains(ee.Hint+ee.Error(), "otpauth://") {
		t.Errorf("message %q / hint %q do not mention otpauth://", ee.Error(), ee.Hint)
	}
	if n := e.f.Calls("", "/"); n != 0 {
		t.Errorf("%d requests to the fake, want 0", n)
	}
}

func TestLoginValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	for name, r := range map[string]service.LoginRequest{
		"empty username": {Password: "x"},
		"empty password": {Username: "u"},
		"bad totp":       {Username: "u", Password: "x", TOTP: "abc"},
	} {
		_, err := e.svc.Login(t.Context(), r)
		if ee := wantExit(t, err, exitcode.Usage); ee.Hint == "" {
			t.Errorf("%s: no hint", name)
		}
	}
	if n := e.f.Calls("", "/"); n != 0 {
		t.Errorf("%d requests to the fake, want 0", n)
	}
}

func TestSecondAccountRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.f.AddUser("other", "other-pass")
	e.clk.advance(61 * time.Second)
	_, err := e.svc.Login(t.Context(), service.LoginRequest{Username: "other", Password: "other-pass"})
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Hint, "pcb logout") {
		t.Errorf("hint = %q, want it to name pcb logout", ee.Hint)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d revocations, want 1 (the second account's session)", n)
	}
	acc := e.accounts(t)
	if len(acc) != 1 || acc[e.f.UserID(e.f.Username)].Username != e.f.Username {
		t.Errorf("the store changed: %d accounts", len(acc))
	}
	if !sessionAlive(t, e, tokensOf(e.account(t))) {
		t.Error("the first account's session was revoked")
	}
}

func TestReloginSameAccountReplacesSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	old := e.account(t)
	e.clk.advance(61 * time.Second)
	e.login(t, e.req())
	now := e.account(t)
	if now.UID == old.UID || now.AccessToken == old.AccessToken || now.RefreshToken == old.RefreshToken {
		t.Error("tokens were not replaced")
	}
	if sessionAlive(t, e, tokensOf(old)) {
		t.Error("the replaced session is still alive")
	}
	if !sessionAlive(t, e, tokensOf(now)) {
		t.Error("the new session does not work")
	}
	if len(e.accounts(t)) != 1 {
		t.Error("more than one account stored")
	}
}

func TestLogoutRevokesAndWipes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	acc := e.account(t)
	res, err := e.svc.Logout(t.Context())
	if err != nil || !res.WasLoggedIn || !res.Revoked {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	if sessionAlive(t, e, tokensOf(acc)) {
		t.Error("the session is still alive")
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("secrets.sealed remains")
	}
	if st := e.loadState(t); st.Phase != store.PhaseLoggedOut || st.AccountID != "" {
		t.Errorf("state = %+v", st)
	}
}

func TestLogoutWhenLoggedOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	res, err := e.svc.Logout(t.Context())
	if err != nil || res.WasLoggedIn || res.Revoked {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	if n := e.f.Calls("", "/"); n != 0 {
		t.Errorf("%d requests to the fake, want 0", n)
	}
}

func TestLogoutClearsPendingCaptcha(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{Captcha: "chal"})
	e.login(t, e.req())
	res, err := e.svc.Logout(t.Context())
	if err != nil || res.WasLoggedIn {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	if st := e.loadState(t); st.Phase != store.PhaseLoggedOut || st.Captcha != nil {
		t.Errorf("state = %+v", st)
	}
}

func TestLogoutUnreadableStoreWipesLocally(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	acc := e.account(t)
	if err := os.Remove(e.cfg.Secrets.KeyFile); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Logout(t.Context())
	if err != nil || !res.WasLoggedIn || res.Revoked {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("secrets.sealed remains")
	}
	if st := e.loadState(t); st.Phase != store.PhaseLoggedOut {
		t.Errorf("state = %+v", st)
	}
	if !sessionAlive(t, e, tokensOf(acc)) {
		t.Error("the session was revoked without the tokens (impossible)")
	}
}

func TestCloseAbortsPendingLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	if res := e.login(t, e.req()); res.State != service.StateNeedsTOTP {
		t.Fatalf("state = %q", res.State)
	}
	if err := e.svc.Close(); err != nil {
		t.Fatal(err)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d revocations, want 1", n)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("secrets sealed")
	}
	if err := e.svc.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Errorf("%d revocations after a second Close, want 1", n)
	}
}

func TestStatusMakesNoRequests(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{Captcha: "chal"})
	check := func(phase string) service.Status {
		t.Helper()
		st, err := e.svc.Status(t.Context())
		if err != nil || st.Phase != phase {
			t.Fatalf("Status = %+v, %v; want phase %s", st, err, phase)
		}
		if st.KeyFile != e.cfg.Secrets.KeyFile {
			t.Errorf("KeyFile = %q", st.KeyFile)
		}
		return st
	}
	if st := check("logged-out"); st.Secrets != "none" || st.Username != "" {
		t.Errorf("logged-out status = %+v", st)
	}
	res := e.login(t, e.req())
	if st := check("needs-captcha"); st.CaptchaURL != res.CaptchaURL {
		t.Errorf("CaptchaURL = %q", st.CaptchaURL)
	}
	e.clk.advance(61 * time.Second)
	r := e.req()
	r.CaptchaToken = res.CaptchaToken
	e.login(t, r)
	n := e.f.Calls("", "/")
	st := check("ready")
	if st.Username != e.f.Username || st.AccountID != e.f.UserID(e.f.Username) || st.Secrets != "sealed" || st.LastLogin == nil || !st.LastLogin.Equal(e.clk.Now()) {
		t.Errorf("ready status = %+v", st)
	}
	if got := e.f.Calls("", "/"); got != n {
		t.Errorf("Status made %d requests", got-n)
	}
}

func TestStatusUnreadableSecretsNeedsLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	if err := os.Remove(e.cfg.Secrets.KeyFile); err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.Status(t.Context())
	if err != nil || st.Phase != "needs-login" || st.Secrets != "unreadable" || st.Reason == "" || st.Username != "" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
}

// TestEveryErrorIsAnExitError walks the failure paths of the API: each error
// is an *exitcode.Error with the mapped code and a hint, and none carries a
// password, a TOTP seed or a token.
func TestEveryErrorIsAnExitError(t *testing.T) {
	t.Parallel()
	type tc struct {
		name string
		fo   protontest.Options
		code exitcode.Code
		run  func(t *testing.T, e *env) error
	}
	login := func(mod func(*service.LoginRequest)) func(*testing.T, *env) error {
		return func(t *testing.T, e *env) error {
			r := e.req()
			if mod != nil {
				mod(&r)
			}
			_, err := e.svc.Login(t.Context(), r)
			return err
		}
	}
	// calendars runs setup (which logs in), then lists the calendars.
	calendars := func(setup func(*testing.T, *env)) func(*testing.T, *env) error {
		return func(t *testing.T, e *env) error {
			if setup != nil {
				setup(t, e)
			}
			_, err := e.svc.Calendars(t.Context())
			return err
		}
	}
	cases := []tc{
		{"wrong password", protontest.Options{}, exitcode.MustAct, login(func(r *service.LoginRequest) { r.Password = "wrong-" + r.Password })},
		{"totp rejected", protontest.Options{TwoFA: true}, exitcode.MustAct, login(func(r *service.LoginRequest) { r.TOTP = "000000" })},
		{"two password", protontest.Options{TwoPassword: true}, exitcode.MustAct, login(nil)},
		{"fido2", protontest.Options{FIDO2Only: true}, exitcode.MustAct, login(nil)},
		{"bad server proof", protontest.Options{BadLoginProof: true}, exitcode.Blocked, login(nil)},
		{"proton down", protontest.Options{}, exitcode.Blocked, func(t *testing.T, e *env) error {
			e.f.FailNext("POST", "/auth/v4/info", 503, 0, 0)
			return login(nil)(t, e)
		}},
		{"rate limited", protontest.Options{}, exitcode.Blocked, func(t *testing.T, e *env) error {
			e.f.FailNext("POST", "/auth/v4/info", 429, 85131, 30)
			return login(nil)(t, e)
		}},
		{"unsupported verification", protontest.Options{Captcha: "c", CaptchaMethods: []string{"sms"}}, exitcode.Blocked, login(nil)},
		{"guard too soon", protontest.Options{}, exitcode.Blocked, func(t *testing.T, e *env) error {
			e.login(t, e.req())
			return login(nil)(t, e)
		}},
		{"closed server", protontest.Options{}, exitcode.Blocked, func(t *testing.T, e *env) error {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			e.cfg.Proton.BaseURL = "http://" + l.Addr().String()
			_ = l.Close()
			e.svc = e.open(t)
			return login(nil)(t, e)
		}},
		{"unreadable store", protontest.Options{}, exitcode.Blocked, func(t *testing.T, e *env) error {
			e.login(t, e.req())
			if err := os.Remove(e.cfg.Secrets.KeyFile); err != nil {
				t.Fatal(err)
			}
			e.clk.advance(61 * time.Second)
			return login(nil)(t, e)
		}},
		{"malformed state", protontest.Options{}, exitcode.Blocked, func(t *testing.T, e *env) error {
			if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(e.dirs.State, "state.json"), []byte("{secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := e.svc.Status(t.Context())
			return err
		}},
		{"session ended", protontest.Options{TwoFA: true}, exitcode.MustAct, func(t *testing.T, e *env) error {
			e.login(t, e.req())
			e.f.RevokeSessions()
			_, err := e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0))
			return err
		}},
		{"captcha rejected", protontest.Options{}, exitcode.MustAct, func(t *testing.T, e *env) error {
			e.f.FailNext("POST", "/auth/v4", 422, int(gpa.HumanValidationInvalidToken), 0)
			return login(func(r *service.LoginRequest) { r.CaptchaToken = "stale-token" })(t, e)
		}},
		{"failing logout", protontest.Options{}, exitcode.Blocked, func(t *testing.T, e *env) error {
			e.login(t, e.req())
			if err := os.WriteFile(e.secrets().Path, []byte(`{"version":99,"sealer":"keyfile","data":""}`), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := e.svc.Logout(t.Context())
			return err
		}},
		{"unwritable state", protontest.Options{TwoFA: true}, exitcode.Blocked, func(t *testing.T, e *env) error {
			failStateOnSleep(t, e)
			_, err := e.svc.Login(t.Context(), service.LoginRequest{Username: e.f.Username, Password: e.f.Password, TOTP: e.f.TOTPURI})
			return err
		}},
		{"empty username", protontest.Options{}, exitcode.Usage, login(func(r *service.LoginRequest) { r.Username = "" })},
		{"no pending totp", protontest.Options{}, exitcode.MustAct, func(t *testing.T, e *env) error {
			_, err := e.svc.SubmitTOTP(t.Context(), "123456")
			return err
		}},
		{"malformed totp", protontest.Options{TwoFA: true}, exitcode.Usage, func(t *testing.T, e *env) error {
			e.login(t, e.req())
			_, err := e.svc.SubmitTOTP(t.Context(), "otpauth://totp/x?secret=!!!")
			return err
		}},
		{"calendars logged out", protontest.Options{}, exitcode.MustAct, calendars(nil)},
		{"refresh transient", protontest.Options{}, exitcode.Blocked, calendars(func(t *testing.T, e *env) {
			e.login(t, e.req())
			e.f.ExpireAccessTokens()
			e.f.FailNext("POST", "/auth/v4/refresh", 503, 0, 0)
		})},
		{"calendars session ended", protontest.Options{}, exitcode.MustAct, calendars(func(t *testing.T, e *env) {
			e.login(t, e.req())
			e.f.RevokeSessions()
		})},
		{"relogin captcha", protontest.Options{}, exitcode.MustAct, calendars(func(t *testing.T, e *env) {
			e.loginUnattended(t, false)
			e.f.RevokeSessions()
			e.f.SetCaptcha("T9")
			e.clk.advance(61 * time.Second)
		})},
		{"relogin wrong password", protontest.Options{}, exitcode.MustAct, calendars(func(t *testing.T, e *env) {
			e.loginUnattended(t, false)
			e.editAccount(t, func(a *store.Account) { a.Relogin.Password = "wrong-" + a.Relogin.Password })
			e.f.RevokeSessions()
			e.clk.advance(61 * time.Second)
		})},
		{"relogin totp rejected", protontest.Options{TwoFA: true}, exitcode.MustAct, calendars(func(t *testing.T, e *env) {
			e.loginUnattended(t, true)
			e.editAccount(t, func(a *store.Account) { a.Relogin.TOTPURI = "otpauth://totp/x?secret=JBSWY3DPEHPK3PXP" })
			e.f.RevokeSessions()
			e.clk.advance(61 * time.Second)
		})},
		{"relogin too soon", protontest.Options{}, exitcode.Blocked, calendars(func(t *testing.T, e *env) {
			e.loginUnattended(t, false)
			e.f.RevokeSessions()
			e.clk.advance(30 * time.Second)
		})},
		{"keys no longer unlock", protontest.Options{}, exitcode.MustAct, calendars(func(t *testing.T, e *env) {
			e.login(t, e.req())
			e.editAccount(t, func(a *store.Account) { a.KeyPass = []byte("not-the-key-passphrase") })
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, c.fo)
			err := c.run(t, e)
			if err == nil {
				t.Fatal("no error")
			}
			ee := wantExit(t, err, c.code)
			if ee.Hint == "" {
				t.Error("no hint")
			}
			u, perr := url.Parse(e.f.TOTPURI)
			if perr != nil || u.Query().Get("secret") == "" {
				t.Fatalf("cannot read the fake's TOTP seed: %v", perr)
			}
			// Every secret the run touched: the password, the seed, and every
			// access token the fake handed out (also the pending sessions').
			secrets := append([]string{e.f.Password, e.f.TOTPURI, u.Query().Get("secret"), "wrong-" + e.f.Password}, e.f.AccessTokens()...)
			if accs, lerr := e.secrets().Load(); lerr == nil {
				for _, a := range accs {
					secrets = append(secrets, a.AccessToken, a.RefreshToken, string(a.KeyPass))
					if a.Relogin != nil {
						secrets = append(secrets, a.Relogin.Password, a.Relogin.TOTPURI, "JBSWY3DPEHPK3PXP")
					}
				}
			}
			for _, s := range secrets {
				if s != "" && strings.Contains(ee.Error()+"\n"+ee.Hint, s) {
					t.Errorf("an error text contains a secret: message %q hint %q", ee.Error(), ee.Hint)
				}
			}
		})
	}
}

func TestLogoutWipesEvenWhenCancelled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res, err := e.svc.Logout(ctx)
	if err != nil || !res.WasLoggedIn {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("secrets.sealed remains after a cancelled logout")
	}
	if st := e.loadState(t); st.Phase != store.PhaseLoggedOut {
		t.Errorf("state = %+v", st)
	}
}

func TestLogoutKeepsStoreItCannotJudge(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	// A format this build does not know is not a lost key: keep the file.
	if err := os.WriteFile(e.secrets().Path, []byte(`{"version":99,"sealer":"keyfile","data":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Logout(t.Context())
	if ee := wantExit(t, err, exitcode.Blocked); ee.Hint == "" {
		t.Error("no hint")
	}
	if _, err := os.Stat(e.secrets().Path); err != nil {
		t.Errorf("secrets.sealed was removed: %v", err)
	}
	if st := e.loadState(t); st.Phase != store.PhaseReady {
		t.Errorf("state = %+v, want it untouched", st)
	}
}

func TestLogoutWipesCorruptStore(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	if err := os.WriteFile(e.secrets().Path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Logout(t.Context())
	if err != nil || !res.WasLoggedIn || res.Revoked {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("the corrupt store remains")
	}
}

func TestEveryMethodAfterCloseIsBlocked(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	if err := e.svc.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	calls := map[string]func() error{
		"Login":       func() error { _, err := e.svc.Login(ctx, e.req()); return err },
		"SubmitTOTP":  func() error { _, err := e.svc.SubmitTOTP(ctx, "123456"); return err },
		"Logout":      func() error { _, err := e.svc.Logout(ctx); return err },
		"Status":      func() error { _, err := e.svc.Status(ctx); return err },
		"Calendars":   func() error { _, err := e.svc.Calendars(ctx); return err },
		"Collections": func() error { _, err := e.svc.Collections(ctx); return err },
		"Objects":     func() error { _, _, err := e.svc.Objects(ctx, "cal"); return err },
		"ObjectsByUID": func() error {
			_, err := e.svc.ObjectsByUID(ctx, "cal", []string{"a@x"})
			return err
		},
		"ObjectsByName": func() error {
			_, err := e.svc.ObjectsByName(ctx, "cal", []string{"a@x.ics"})
			return err
		},
		"Changes": func() error { _, err := e.svc.Changes(ctx, "cal", ""); return err },
		"Serve":   func() error { return e.svc.Serve(ctx, service.ServeOptions{}) },
	}
	for name, call := range calls {
		ee := wantExit(t, call(), exitcode.Blocked)
		if !strings.Contains(ee.Error(), "service closed") || ee.Hint == "" {
			t.Errorf("%s: message %q hint %q", name, ee.Error(), ee.Hint)
		}
	}
	if n := e.f.Calls("", "/"); n != 0 {
		t.Errorf("%d requests after Close", n)
	}
}

// uidRecorder remembers the x-pm-uid of the requests it carries.
type uidRecorder struct {
	mu   sync.Mutex
	uids []string
}

func (u *uidRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	u.mu.Lock()
	u.uids = append(u.uids, r.Header.Get("x-pm-uid"))
	u.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

// A refresh during the key calls spends the refresh token SRP returned; when
// the login then fails, the abort must revoke the refreshed session, not try
// (and fail) with the spent pair.
func TestAbortAfterRefreshRevokesLiveSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	rec := &uidRecorder{}
	svc, err := service.NewLocal(service.Options{
		Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
		HTTP: &http.Client{Transport: rec, Timeout: 10 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	e.f.FailNext("GET", "/core/v4/keys/salts", 401, 401, 0)
	e.f.FailNext("GET", "/core/v4/addresses", 400, 2000, 0)
	if _, err := svc.Login(t.Context(), e.req()); err == nil {
		t.Fatal("the login succeeded")
	}
	if n := e.f.Calls("POST", "/auth/v4/refresh"); n != 1 {
		t.Fatalf("%d refreshes during the login, want 1", n)
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Fatal("a failed login sealed a session")
	}
	minted := e.f.AccessTokens()
	rec.mu.Lock()
	uid := rec.uids[len(rec.uids)-1]
	rec.mu.Unlock()
	live := api.Tokens{UID: uid, AccessToken: minted[len(minted)-1]}
	if sessionAlive(t, e, live) {
		t.Error("the refreshed session is still alive after the failed login")
	}
}

const (
	rolledBackMsg  = "secrets.sealed is older than the last one pcb wrote (generation 1 < 2): it was restored or replaced"
	rolledBackHint = "if you restored it on purpose, run pcb logout, then pcb login"
)

// restoreOlderSecrets puts back secrets.sealed as it was after the login
// (generation 1) once the store sealed generation 2: a restored backup.
func restoreOlderSecrets(t *testing.T, e *env) []byte {
	t.Helper()
	old, err := os.ReadFile(e.secrets().Path)
	if err != nil {
		t.Fatal(err)
	}
	e.editAccount(t, func(a *store.Account) { a.AccessToken += "-rotated" })
	if err := os.WriteFile(e.secrets().Path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	return old
}

func TestRolledBackSecretsNeedALogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	restoreOlderSecrets(t, e)
	calls := e.f.Calls("", "/")
	st, err := e.svc.Status(t.Context())
	if err != nil || st.Secrets != "rolled-back" || st.Phase != "needs-login" || st.Reason != rolledBackMsg || st.Username != "" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	_, err = e.svc.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != rolledBackMsg || ee.Hint != rolledBackHint {
		t.Errorf("Calendars: message %q hint %q", ee.Error(), ee.Hint)
	}
	e.clk.advance(61 * time.Second)
	_, err = e.svc.Login(t.Context(), e.req())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != rolledBackMsg || ee.Hint != rolledBackHint {
		t.Errorf("Login: message %q hint %q", ee.Error(), ee.Hint)
	}
	if n := e.f.Calls("", "/") - calls; n != 0 {
		t.Errorf("%d requests made with the restored secrets, want 0", n)
	}
}

func TestLogoutWipesRolledBackSecrets(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	acc := e.account(t)
	old := restoreOlderSecrets(t, e)
	res, err := e.svc.Logout(t.Context())
	if err != nil || !res.WasLoggedIn || res.Revoked {
		t.Fatalf("Logout = %+v, %v", res, err)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 0 {
		t.Errorf("%d revocations with the restored tokens, want 0", n)
	}
	if !sessionAlive(t, e, tokensOf(acc)) {
		t.Error("the session was revoked")
	}
	if _, err := os.Stat(e.secrets().Path); err == nil {
		t.Error("the restored secrets.sealed remains")
	}
	if st := e.loadState(t); st.Phase != store.PhaseLoggedOut {
		t.Errorf("state = %+v", st)
	}
	// Restored once more after the logout: still refused.
	if err := os.WriteFile(e.secrets().Path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.secrets().Load(); !errors.Is(err, store.ErrRolledBack) {
		t.Errorf("restored after logout: Load err = %v, want ErrRolledBack", err)
	}
	if err := os.Remove(e.secrets().Path); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(61 * time.Second)
	if res := e.login(t, e.req()); res.State != service.StateReady {
		t.Fatalf("login after the logout: %+v", res)
	}
	if st, err := e.svc.Status(t.Context()); err != nil || st.Secrets != "sealed" || st.Phase != "ready" {
		t.Errorf("Status after a new login = %+v, %v", st, err)
	}
}

// Logout is the reset of a damaged state.json: the new one carries the
// wiped generation + 1, so the wiped file is still refused if restored.
func TestLogoutOverMalformedStateKeepsTheGeneration(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	old, err := os.ReadFile(e.secrets().Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dirs.State, "state.json"), []byte("{damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Logout(t.Context()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if st := e.loadState(t); st.Phase != store.PhaseLoggedOut || st.SecretsGeneration != 2 {
		t.Fatalf("state = %+v, want logged-out with generation 2", st)
	}
	if err := os.WriteFile(e.secrets().Path, old, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.secrets().Load(); !errors.Is(err, store.ErrRolledBack) {
		t.Errorf("the wiped file restored: Load err = %v, want ErrRolledBack", err)
	}
}

// A mirror edited out of range fails closed with a MustAct; logout resets
// it and a new login works.
func TestGenerationOutOfRangeNeedsALogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	var st map[string]json.RawMessage
	if err := json.Unmarshal([]byte(mustJSON(t, e.loadState(t))), &st); err != nil {
		t.Fatal(err)
	}
	st["secrets_generation"] = json.RawMessage("18446744073709551615")
	if err := os.WriteFile(filepath.Join(e.dirs.State, "state.json"), []byte(mustJSON(t, st)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Status(t.Context())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "the login state file holds a secrets generation out of range: it was edited or damaged" ||
		ee.Hint != "run pcb logout, then pcb login" {
		t.Errorf("message %q hint %q", ee.Error(), ee.Hint)
	}
	if _, err := e.svc.Logout(t.Context()); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	e.clk.advance(61 * time.Second)
	if res := e.login(t, e.req()); res.State != service.StateReady {
		t.Fatalf("login after the reset: %+v", res)
	}
	if s, err := e.svc.Status(t.Context()); err != nil || s.Secrets != "sealed" {
		t.Errorf("Status = %+v, %v", s, err)
	}
	if g := e.loadState(t).SecretsGeneration; g != 3 {
		t.Errorf("generation after logout and login = %d, want 3", g)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The session pcb holds in memory when the store is found rolled back is
// its newest, sealed only in the file the restore replaced: it is revoked
// (ruling M5-R3), and the restored file's session is not.
func TestRolledBackSecretsRevokeTheHeldSession(t *testing.T) {
	t.Parallel()
	for name, detect := range map[string]func(t *testing.T, e *env){
		"operation": func(t *testing.T, e *env) {
			_, err := e.svc.Calendars(t.Context())
			if ee := wantExit(t, err, exitcode.MustAct); ee.Hint != rolledBackHint {
				t.Errorf("hint = %q", ee.Hint)
			}
		},
		"logout": func(t *testing.T, e *env) {
			if res, err := e.svc.Logout(t.Context()); err != nil || !res.WasLoggedIn {
				t.Fatalf("Logout = %+v, %v", res, err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, protontest.Options{})
			e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
			e.login(t, e.req())
			old, err := os.ReadFile(e.secrets().Path)
			if err != nil {
				t.Fatal(err)
			}
			e.f.ExpireAccessTokens()
			if got := e.calendars(t); len(got) != 1 { // refreshes: the held client has the newest tokens
				t.Fatalf("calendars = %+v", got)
			}
			newest := e.account(t)
			if err := os.WriteFile(e.secrets().Path, old, 0o600); err != nil {
				t.Fatal(err)
			}
			if !sessionAlive(t, e, tokensOf(newest)) {
				t.Fatal("the newest session is not alive before the check")
			}
			detect(t, e)
			if sessionAlive(t, e, tokensOf(newest)) {
				t.Error("the newest session, held in memory, is still alive")
			}
			if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
				t.Errorf("%d revocations, want 1 (the held session)", n)
			}
			// A second detection has nothing left to revoke.
			_, _ = e.svc.Calendars(t.Context())
			if n := e.f.Calls("DELETE", "/auth/v4"); n != 1 {
				t.Errorf("%d revocations after a second detection, want 1", n)
			}
		})
	}
}
