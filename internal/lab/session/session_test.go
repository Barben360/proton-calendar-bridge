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
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
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

// refreshes counts the POST /auth/v4/refresh that reached f.
func refreshes(f *labtest.Front) int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	n := 0
	for _, s := range f.Log {
		if s.Method == "POST" && s.Path == "/auth/v4/refresh" {
			n++
		}
	}
	return n
}

// Resume rebuilds the client on the saved tokens without refreshing: every
// refresh Proton accepts needs the access token, and a refresh per command
// would rotate the session for nothing (live finding 2026-11-12, E01). The
// saved scopes stay as saved.
func TestResumeUsesTheSavedTokensWithoutRefreshing(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	uid := s.UID
	s.Close()
	path := filepath.Join(o.StateDir, "session.json")
	var saved map[string]any
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	saved["scopes"] = []string{"full", "self", "saved-marker"}
	raw, _ = json.Marshal(saved)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	authBefore := f.AuthCalls
	for i := 1; i <= 2; i++ {
		r, err := session.Resume(t.Context(), o)
		if err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		if r.UID != uid || strings.Join(r.Scopes, " ") != "full self saved-marker" {
			t.Errorf("resume %d: UID %q scopes %q, want %q and the saved scopes", i, r.UID, r.Scopes, uid)
		}
		u, err := r.Client.GetUser(t.Context())
		if err != nil || u.ID == "" {
			t.Fatalf("resume %d: GetUser with the resumed client: %v", i, err)
		}
		r.Close()
	}
	if f.AuthCalls != authBefore {
		t.Error("Resume logged in again")
	}
	if n := refreshes(f); n != 0 {
		t.Errorf("Resume sent %d refreshes, want none", n)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(raw, after) {
		t.Error("Resume rewrote session.json without a refresh")
	}
}

// The resumed client refreshes on a rejected access token, with that token
// (go-proton-api's own refresh), saves each rotation, and a later Resume and
// probe still work: the second refresh is the one Proton refused without the
// access token.
func TestResumedClientRefreshesTwiceAndSavesEachRotation(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	path := filepath.Join(o.StateDir, "session.json")
	r, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		before, _ := os.ReadFile(path)
		f.Backend.ExpireAccessTokens()
		if _, err := r.Client.GetUser(t.Context()); err != nil {
			t.Fatalf("GetUser after expiry %d: %v", i, err)
		}
		if after, _ := os.ReadFile(path); bytes.Equal(before, after) {
			t.Errorf("expiry %d: the rotated tokens were not saved", i)
		}
	}
	r.Close()
	if n := refreshes(f); n != 2 {
		t.Fatalf("%d refreshes, want 2", n)
	}
	again, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatalf("Resume after the rotations: %v", err)
	}
	defer again.Close()
	if resp, err := again.Get(t.Context(), "/core/v4/users"); err != nil || !resp.OK() {
		t.Fatalf("probe GET on the saved tokens: %v, HTTP %d code %d", err, resp.Status, resp.Code)
	}
}

// The tokens the resumed client rotates are redacted from the recording too.
func TestResumedClientForbidsRotatedTokens(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	o.Recorder = &record.Recorder{Dir: t.TempDir()}
	r, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f.Backend.ExpireAccessTokens()
	if _, err := r.Client.GetUser(t.Context()); err != nil {
		t.Fatalf("GetUser after expiry: %v", err)
	}
	var st struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	raw, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
	if err := json.Unmarshal(raw, &st); err != nil || st.AccessToken == "" {
		t.Fatalf("session.json: %v", err)
	}
	for _, tok := range []string{st.AccessToken, st.RefreshToken} {
		// The path is written as given: only the Forbid list keeps it out.
		e := record.Exchange{Method: "GET", Path: "/echo/" + tok, Status: 200}
		path, err := (&record.Recorder{Dir: t.TempDir()}).Save("control", e)
		if b, _ := os.ReadFile(path); err != nil || !strings.Contains(string(b), tok) {
			t.Fatalf("control: an unrestricted recorder did not write the token as given: %v", err)
		}
		if _, err := o.Recorder.Save("echo", e); err == nil {
			t.Error("the recorder wrote a rotated token")
		}
	}
}

// E01's probe refresh: each refresh, and its retry with the previous refresh
// token, carries the access token of the pair it refreshes, so a second probe
// refresh in a row works.
func TestRefreshSendsTheAccessTokenOfEachPair(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	type body struct{ RefreshToken, AccessToken string }
	var mu sync.Mutex
	var sent []body
	f.Mu.Lock()
	f.Intercept = func(method, path string, b []byte) (labtest.Reply, bool) {
		if method == "POST" && path == "/auth/v4/refresh" {
			var rb body
			_ = json.Unmarshal(b, &rb)
			mu.Lock()
			sent = append(sent, rb)
			mu.Unlock()
		}
		return labtest.Reply{}, false
	}
	f.Mu.Unlock()
	type pair struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	read := func() pair {
		var p pair
		raw, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
		if err := json.Unmarshal(raw, &p); err != nil || p.AccessToken == "" {
			t.Fatalf("session.json: %v", err)
		}
		return p
	}
	for i := 1; i <= 2; i++ {
		old := read()
		res, err := session.Refresh(t.Context(), o)
		if err != nil || res.First.Status != 200 || !res.Retried {
			t.Fatalf("refresh %d: %v, first HTTP %d code %d, retried %t", i, err, res.First.Status, res.First.Code, res.Retried)
		}
		mu.Lock()
		got := sent[len(sent)-2:]
		mu.Unlock()
		for j, b := range got {
			if b.RefreshToken != old.RefreshToken || b.AccessToken != old.AccessToken {
				t.Errorf("refresh %d, request %d: the body does not carry the refreshed pair's access token", i, j+1)
			}
		}
	}
}

// Resume checks the saved session: one that is gone is MustAct (exit 10,
// HUMAN-CHECKS), not a failure of the first probe call.
func TestResumeOfARevokedSessionIsMustAct(t *testing.T) {
	t.Parallel()
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	f.Backend.RevokeSessions()
	r, err := session.Resume(t.Context(), o)
	if err == nil {
		r.Close()
		t.Fatal("Resume of a revoked session succeeded")
	}
	if e := exitErr(t, err); e.Code != exitcode.MustAct || !strings.Contains(e.Hint, "pcb-lab login") {
		t.Fatalf("got %v, want MustAct with a login hint", err)
	}
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

// A rotation the resumed client cannot save is reported on Stderr, without
// any token, old or rotated.
func TestResumedClientReportsFailedTokenWrite(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	var stderr bytes.Buffer
	o.Stderr = &stderr
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
	r, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.Chmod(o.StateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(o.StateDir, 0o700) //nolint:errcheck // t.TempDir cleanup
	f.Backend.ExpireAccessTokens()
	if _, err := r.Client.GetUser(t.Context()); err != nil {
		t.Fatalf("GetUser after expiry: %v", err)
	}
	msg := strings.ReplaceAll(stderr.String(), o.StateDir, "<state>")
	if !strings.Contains(msg, "cannot save the rotated session tokens") {
		t.Fatalf("stderr = %q, want the failed save reported", msg)
	}
	for _, tok := range append(f.Backend.AccessTokens(), saved.RefreshToken) {
		if strings.Contains(msg, tok) {
			t.Errorf("the warning contains a session token: %q", msg)
		}
	}
	if long := regexp.MustCompile(`[A-Za-z0-9_-]{24,}`).FindString(msg); long != "" {
		t.Errorf("the warning contains the token-like string %q: %q", long, msg)
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
	f.Mu.Lock()
	first := len(f.Log) // Resume's own check goes through go-proton-api
	f.Mu.Unlock()
	// Twice: a refreshed pair's refresh token needs its access token.
	for i := 1; i <= 2; i++ {
		before, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
		f.Backend.ExpireAccessTokens()
		var u struct{ User struct{ Name string } }
		if err := c.Get(t.Context(), "/core/v4/users", nil, &u); err != nil || u.User.Name != labtest.User {
			t.Fatalf("GET after expiry %d: %v, user %q", i, err, u.User.Name)
		}
		after, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))
		if bytes.Equal(before, after) {
			t.Errorf("expiry %d: the refresh's rotated tokens were not saved", i)
		}
	}
	f.Mu.Lock()
	for _, seen := range f.Log[first:] {
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
