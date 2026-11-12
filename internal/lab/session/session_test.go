package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
)

const totpURI = "otpauth://totp/lab?secret=JBSWY3DPEHPK3PXP"

var t0 = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

func opts(t *testing.T, f *labtest.Front) session.Options {
	t.Helper()
	return session.Options{
		BaseURL:  f.URL,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Creds:    session.Creds{Username: labtest.User, Password: labtest.Pass},
		Now:      func() time.Time { return t0 },
	}
}

func exitErr(t *testing.T, err error) *exitcode.Error {
	t.Helper()
	var e *exitcode.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not an *exitcode.Error", err)
	}
	return e
}

func TestLoginGuardRefusesWithin60s(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	last := t0.Add(-59 * time.Second).Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(o.StateDir, "last-login"), []byte(last), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := session.Login(t.Context(), o)
	if e := exitErr(t, err); e.Code != exitcode.Blocked {
		t.Fatalf("code = %d, want %d (err: %v)", e.Code, exitcode.Blocked, err)
	}
	if f.Requests != 0 {
		t.Fatalf("%d requests reached Proton despite the guard", f.Requests)
	}
	// The refusal must not push the window forward.
	b, _ := os.ReadFile(filepath.Join(o.StateDir, "last-login"))
	if string(b) != last {
		t.Fatalf("last-login rewritten by a refused login: %q", b)
	}
}

func TestLoginGuardAllowsAfter60s(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	last := t0.Add(-61 * time.Second).Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(o.StateDir, "last-login"), []byte(last), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer s.Close()
	b, _ := os.ReadFile(filepath.Join(o.StateDir, "last-login"))
	if got, _ := time.Parse(time.RFC3339Nano, string(b)); !got.Equal(t0) {
		t.Fatalf("last-login = %q, want %s", b, t0)
	}
}

func TestLoginHVStopsWithResumeInfo(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{Challenge: "T1"})
	o := opts(t, f)
	_, err := session.Login(t.Context(), o)
	e := exitErr(t, err)
	if e.Code != exitcode.MustAct {
		t.Fatalf("code = %d, want %d (err: %v)", e.Code, exitcode.MustAct, err)
	}
	if !strings.Contains(e.Hint, "--captcha-token T1") {
		t.Errorf("hint = %q, want it to contain --captcha-token T1", e.Hint)
	}
	wantURL := "https://verify.proton.me/?methods=captcha&token=T1"
	if !strings.Contains(e.Error(), wantURL) {
		t.Errorf("message = %q, want it to contain %s", e.Error(), wantURL)
	}
	if f.AuthCalls != 1 {
		t.Errorf("/auth/v4 calls = %d, want exactly 1 (no retry storm)", f.AuthCalls)
	}
	raw, err := os.ReadFile(filepath.Join(o.StateDir, "hv.json"))
	if err != nil {
		t.Fatalf("hv.json: %v", err)
	}
	var hv struct {
		Token, URL, At string
	}
	if err := json.Unmarshal(raw, &hv); err != nil {
		t.Fatal(err)
	}
	if hv.Token != "T1" || hv.URL != wantURL || hv.At == "" {
		t.Errorf("hv.json = %+v", hv)
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, "session.json")); err == nil {
		t.Error("session.json written on a failed login")
	}
}

func TestLoginWithCaptchaTokenSendsIt(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	o.CaptchaToken = "T1"
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer s.Close()
	if len(f.HVHeaders) != 1 || f.HVHeaders[0] != "T1" {
		t.Fatalf("human-verification header per attempt = %q, want [T1]", f.HVHeaders)
	}
}

func TestLoginWithoutCaptchaTokenSendsNone(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	s, err := session.Login(t.Context(), opts(t, f))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer s.Close()
	if len(f.HVHeaders) != 1 || f.HVHeaders[0] != "" {
		t.Fatalf("human-verification header = %q, want none", f.HVHeaders)
	}
}

func TestLoginTOTPFromURI(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{TwoFA: true})
	o := opts(t, f)
	tp, err := otp.Parse(totpURI)
	if err != nil {
		t.Fatal(err)
	}
	o.Creds.TOTP = tp
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer s.Close()
	want := tp.Code(t0)
	if len(f.Codes) != 1 || f.Codes[0] != want {
		t.Fatalf("2FA codes received = %q, want [%s] (the code at the injected clock)", f.Codes, want)
	}
	if other := tp.Code(t0.Add(time.Hour)); other == want {
		t.Fatal("test is vacuous: codes at different times are equal")
	}
}

func TestLoginNeedsTOTPWhenAccountHas2FA(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{TwoFA: true})
	_, err := session.Login(t.Context(), opts(t, f))
	if e := exitErr(t, err); e.Code != exitcode.MustAct {
		t.Fatalf("code = %d, want %d (err: %v)", e.Code, exitcode.MustAct, err)
	}
	if len(f.Codes) != 0 {
		t.Fatal("a 2FA code was sent without a TOTP")
	}
}

func TestLoginMissingCredsIsUsageAndKeepsGuardFree(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	o.Creds.Password = ""
	_, err := session.Login(t.Context(), o)
	e := exitErr(t, err)
	if e.Code != exitcode.Usage {
		t.Fatalf("code = %d, want %d", e.Code, exitcode.Usage)
	}
	if !strings.Contains(e.Hint, "--env-file ~/.config/pcb-live/lab.env ") {
		t.Errorf("hint %q: want the filled local copy ~/.config/pcb-live/lab.env, not the placeholder example", e.Hint)
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, "last-login")); err == nil {
		t.Error("a usage error consumed the login window")
	}
}

func TestLoginWrongPasswordDoesNotLeakIt(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	o.Creds.Password = "wrong-password-canary"
	_, err := session.Login(t.Context(), o)
	if err == nil {
		t.Fatal("login with a wrong password succeeded")
	}
	if strings.Contains(err.Error(), "wrong-password-canary") {
		t.Fatalf("error leaks the password: %v", err)
	}
}

func TestSessionFileMode0600(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer s.Close()
	for name, want := range map[string]os.FileMode{
		"session.json": 0o600, "last-login": 0o600, ".": 0o700,
	} {
		fi, err := os.Stat(filepath.Join(o.StateDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", name, got, want)
		}
	}
}

func TestSessionFileHoldsNoPassword(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw, err := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(labtest.Pass)) {
		t.Fatal("session.json contains the password")
	}
	var got struct {
		UID          string `json:"uid"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.UID == "" || got.UID != s.UID || got.AccessToken == "" || got.RefreshToken == "" {
		t.Fatalf("session.json incomplete: %s", raw)
	}
}

func TestResumeRebuildsClientAndPersistsRotation(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	uid := s.UID
	s.Close()
	before, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))

	authBefore := f.AuthCalls
	r, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer r.Close()
	if r.UID != uid {
		t.Errorf("resumed UID = %q, want %q", r.UID, uid)
	}
	if f.AuthCalls != authBefore {
		t.Error("Resume logged in again instead of refreshing")
	}
	// The client must work: it can fetch the user.
	u, err := r.Client.GetUser(t.Context())
	if err != nil {
		t.Fatalf("GetUser with the resumed client: %v", err)
	}
	if u.ID == "" {
		t.Error("empty user")
	}
	after, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	if bytes.Equal(before, after) {
		t.Error("session.json not updated with the rotated tokens")
	}
	// And the rotated file resumes again.
	r2, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	r2.Close()
}

func TestResumeWithoutSessionIsMustAct(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	_, err := session.Resume(t.Context(), opts(t, f))
	if e := exitErr(t, err); e.Code != exitcode.MustAct || !strings.Contains(e.Hint, "pcb-lab login") {
		t.Fatalf("got %v, want MustAct with a login hint", err)
	}
	if f.Requests != 0 {
		t.Fatal("network used without a session")
	}
}

func TestRedactIDNeverReturnsTheWholeID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"", "ab", "abcd", "abcde", "5ad57d77-3c1c-4784-a6c5-e38b25881c57"} {
		got := session.RedactID(id)
		if id != "" && strings.Contains(got, id) {
			t.Errorf("RedactID(%q) = %q contains the id", id, got)
		}
	}
	if got := session.RedactID("5ad57d77-3c1c"); !strings.HasPrefix(got, "5ad5…") {
		t.Errorf("RedactID = %q", got)
	}
}

func TestConcurrentLoginsReachProtonOnce(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	const n = 8
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := session.Login(ctx, o)
			if s != nil {
				s.Close()
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	ok, blocked := 0, 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case exitErr(t, err).Code == exitcode.Blocked:
			blocked++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if f.AuthCalls != 1 || ok != 1 || blocked != n-1 {
		t.Fatalf("/auth/v4 calls = %d, ok = %d, blocked = %d; want 1, 1, %d", f.AuthCalls, ok, blocked, n-1)
	}
}

func TestHVFileLifecycle(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{Challenge: "T1"})
	o := opts(t, f)
	if _, err := session.Login(t.Context(), o); err == nil {
		t.Fatal("expected the HV stop")
	}
	hv := filepath.Join(o.StateDir, "hv.json")
	if _, err := os.Stat(hv); err != nil {
		t.Fatalf("hv.json missing after the stop: %v", err)
	}
	// 61 s later the author retries with the solved token.
	f.Mu.Lock()
	f.Challenge = ""
	f.Mu.Unlock()
	o.Now = func() time.Time { return t0.Add(61 * time.Second) }
	o.CaptchaToken = "T1"
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat(hv); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hv.json still present after a successful login: %v", err)
	}
}

func TestHVFileRemovedByPlainSuccessfulLogin(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hv := filepath.Join(o.StateDir, "hv.json")
	if err := os.WriteFile(hv, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(hv); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale hv.json kept: %v", err)
	}
}

func TestExistingStateDirTightenedTo0700(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	if err := os.MkdirAll(o.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(o.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, _ := os.Stat(o.StateDir)
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("state dir mode = %o, want 700", got)
	}
}

func TestResumeReportsFailedTokenWrite(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	raw, err := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil || saved.AccessToken == "" || saved.RefreshToken == "" {
		t.Fatalf("session.json has no tokens to look for: %v", err)
	}
	if err := os.Chmod(o.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(o.StateDir, 0o700) //nolint:errcheck // t.TempDir cleanup
	r, err := session.Resume(t.Context(), o)
	if err == nil {
		r.Close()
		t.Fatal("Resume succeeded although the rotated tokens could not be saved")
	}
	if !strings.Contains(err.Error(), "saving the rotated session tokens") {
		t.Fatalf("error = %v", err)
	}
	// The error is printed and logged: it must carry no token, old or rotated.
	msg := strings.ReplaceAll(err.Error(), o.StateDir, "<state>")
	for _, tok := range []string{saved.AccessToken, saved.RefreshToken} {
		if strings.Contains(msg, tok) {
			t.Errorf("error contains a session token: %q", msg)
		}
	}
	if long := regexp.MustCompile(`[A-Za-z0-9_-]{24,}`).FindString(msg); long != "" {
		t.Errorf("error contains the token-like string %q: %q", long, msg)
	}
}

func TestTOTPWaitsForTheNextPeriod(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{TwoFA: true})
	o := opts(t, f)
	tp, err := otp.Parse(totpURI)
	if err != nil {
		t.Fatal(err)
	}
	o.Creds.TOTP = tp
	clock := t0.Add(27 * time.Second) // 3 s before the boundary
	var slept time.Duration
	o.Now = func() time.Time { return clock }
	o.Sleep = func(_ context.Context, d time.Duration) error {
		slept += d
		clock = clock.Add(d)
		return nil
	}
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	next := tp.Code(t0.Add(30 * time.Second))
	if slept != 3*time.Second {
		t.Errorf("slept %s, want 3s", slept)
	}
	if len(f.Codes) != 1 || f.Codes[0] != next {
		t.Fatalf("code sent = %q, want the next period's %s", f.Codes, next)
	}
	if tp.Code(t0) == next {
		t.Fatal("vacuous: both periods give the same code")
	}
}

func TestHVEventsAreLogged(t *testing.T) {
	t.Parallel()
	const second = "SECOND-CHALLENGE-TOKEN"
	f := labtest.NewFront(t, &labtest.Front{Challenge: "T1"})
	o := opts(t, f)
	if _, err := session.Login(t.Context(), o); err == nil {
		t.Fatal("expected the first HV stop")
	}
	// 61 s later a retry with the (stale) solved token is answered by another challenge.
	f.Mu.Lock()
	f.Challenge = second
	f.Mu.Unlock()
	o.Now = func() time.Time { return t0.Add(61 * time.Second) }
	o.CaptchaToken = "T1"
	if _, err := session.Login(t.Context(), o); err == nil {
		t.Fatal("expected the second HV stop")
	}

	path := filepath.Join(o.StateDir, "hv-log.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("hv-log.jsonl: %v", err)
	}
	for _, secret := range []string{"T1\"", second} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("hv-log.jsonl contains the token %q: %s", secret, raw)
		}
	}
	type hvLine struct {
		At          string `json:"at"`
		TokenLen    int    `json:"token_len"`
		SentCaptcha bool   `json:"sent_captcha_token"`
	}
	var lines []hvLine
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e hvLine
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		lines = append(lines, e)
	}
	if len(lines) != 2 {
		t.Fatalf("hv-log.jsonl has %d lines, want 2: %s", len(lines), raw)
	}
	if lines[0].At != "2026-11-11T10:00:00Z" || lines[0].TokenLen != 2 || lines[0].SentCaptcha {
		t.Errorf("first line = %+v", lines[0])
	}
	if lines[1].At != "2026-11-11T10:01:01Z" || lines[1].TokenLen != len(second) || !lines[1].SentCaptcha {
		t.Errorf("second line = %+v", lines[1])
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("hv-log.jsonl mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

func TestSuccessfulLoginLeavesTheHVLog(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{Challenge: "T1"})
	o := opts(t, f)
	if _, err := session.Login(t.Context(), o); err == nil {
		t.Fatal("expected the HV stop")
	}
	f.Mu.Lock()
	f.Challenge = ""
	f.Mu.Unlock()
	o.Now = func() time.Time { return t0.Add(61 * time.Second) }
	o.CaptchaToken = "T1"
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if b, err := os.ReadFile(filepath.Join(o.StateDir, "hv-log.jsonl")); err != nil || strings.Count(string(b), "\n") != 1 {
		t.Fatalf("hv-log.jsonl = %q, %v; the event history must survive a solved challenge", b, err)
	}
}

// API is pcb's transport on the saved session: it identifies as pcb-lab, and
// a rejected access token is refreshed once and the rotated pair saved, so a
// later Resume still works.
func TestAPIRefreshesAndSavesTheRotation(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := s.API(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	f.Backend.ExpireAccessTokens()
	var u struct{ User struct{ Name string } }
	if err := c.Get(t.Context(), "/core/v4/users", nil, &u); err != nil || u.User.Name != labtest.User {
		t.Fatalf("GET after expiry: %v, user %q", err, u.User.Name)
	}
	after, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	if bytes.Equal(before, after) {
		t.Error("the refresh's rotated tokens were not saved")
	}
	f.Mu.Lock()
	for _, seen := range f.Log {
		if seen.Path == "/core/v4/users" && !strings.HasPrefix(seen.UserAgent, "pcb-lab/") {
			t.Errorf("API sent User-Agent %q, want pcb-lab/<version>", seen.UserAgent)
		}
	}
	f.Mu.Unlock()
	again, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatalf("Resume after the API's refresh: %v", err)
	}
	again.Close()
}
