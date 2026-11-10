package session_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ProtonMail/go-proton-api/server"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
)

const (
	testUser = "lab"
	testPass = "lab-pass-not-a-secret"
	totpURI  = "otpauth://totp/lab?secret=JBSWY3DPEHPK3PXP"
)

var t0 = time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)

// front is an httptest proxy in front of go-proton-api's in-memory server.
// It can answer /auth/v4 with a CAPTCHA challenge and fake a TOTP account.
type front struct {
	mu        sync.Mutex
	authCalls int      // POST /auth/v4
	hvHeaders []string // human-verification token header per POST /auth/v4
	codes     []string // 2FA codes received
	requests  int      // every request
	challenge string   // non-empty: answer POST /auth/v4 with 9001 and this token
	twoFA     bool
	url       string
}

func newFront(t *testing.T, f *front) *front {
	t.Helper()
	backend := server.New(server.WithTLS(false))
	t.Cleanup(backend.Close)
	if _, _, err := backend.CreateUser(testUser, []byte(testPass)); err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(backend.GetHostURL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = func(resp *http.Response) error {
		if !f.twoFA || resp.Request.Method != http.MethodPost || resp.Request.URL.Path != "/auth/v4" || resp.StatusCode != http.StatusOK {
			return nil
		}
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			return err
		}
		body["2FA"] = map[string]any{"Enabled": 1}
		data, _ := json.Marshal(body)
		resp.Body = io.NopCloser(bytes.NewReader(data))
		resp.ContentLength = int64(len(data))
		resp.Header.Del("Content-Length")
		return nil
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/auth/v4":
			f.mu.Lock()
			f.authCalls++
			f.hvHeaders = append(f.hvHeaders, r.Header.Get("x-pm-human-verification-token"))
			challenge := f.challenge
			f.mu.Unlock()
			if challenge != "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"Code": 9001, "Error": "Human verification required",
					"Details": map[string]any{
						"HumanVerificationMethods": []string{"captcha"},
						"HumanVerificationToken":   challenge,
					},
				})
				return
			}
		case r.Method == http.MethodPost && r.URL.Path == "/auth/v4/2fa":
			var req struct{ TwoFactorCode string }
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.codes = append(f.codes, req.TwoFactorCode)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": 1000, "Scopes": []string{"full"}})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	f.url = ts.URL
	return f
}

func opts(t *testing.T, f *front) session.Options {
	t.Helper()
	return session.Options{
		BaseURL:  f.url,
		StateDir: filepath.Join(t.TempDir(), "state"),
		Creds:    session.Creds{Username: testUser, Password: testPass},
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
	f := newFront(t, &front{})
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
	if f.requests != 0 {
		t.Fatalf("%d requests reached Proton despite the guard", f.requests)
	}
	// The refusal must not push the window forward.
	b, _ := os.ReadFile(filepath.Join(o.StateDir, "last-login"))
	if string(b) != last {
		t.Fatalf("last-login rewritten by a refused login: %q", b)
	}
}

func TestLoginGuardAllowsAfter60s(t *testing.T) {
	f := newFront(t, &front{})
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
	f := newFront(t, &front{challenge: "T1"})
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
	if f.authCalls != 1 {
		t.Errorf("/auth/v4 calls = %d, want exactly 1 (no retry storm)", f.authCalls)
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
	f := newFront(t, &front{})
	o := opts(t, f)
	o.CaptchaToken = "T1"
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer s.Close()
	if len(f.hvHeaders) != 1 || f.hvHeaders[0] != "T1" {
		t.Fatalf("human-verification header per attempt = %q, want [T1]", f.hvHeaders)
	}
}

func TestLoginWithoutCaptchaTokenSendsNone(t *testing.T) {
	f := newFront(t, &front{})
	s, err := session.Login(t.Context(), opts(t, f))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer s.Close()
	if len(f.hvHeaders) != 1 || f.hvHeaders[0] != "" {
		t.Fatalf("human-verification header = %q, want none", f.hvHeaders)
	}
}

func TestLoginTOTPFromURI(t *testing.T) {
	f := newFront(t, &front{twoFA: true})
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
	if len(f.codes) != 1 || f.codes[0] != want {
		t.Fatalf("2FA codes received = %q, want [%s] (the code at the injected clock)", f.codes, want)
	}
	if other := tp.Code(t0.Add(time.Hour)); other == want {
		t.Fatal("test is vacuous: codes at different times are equal")
	}
}

func TestLoginNeedsTOTPWhenAccountHas2FA(t *testing.T) {
	f := newFront(t, &front{twoFA: true})
	_, err := session.Login(t.Context(), opts(t, f))
	if e := exitErr(t, err); e.Code != exitcode.MustAct {
		t.Fatalf("code = %d, want %d (err: %v)", e.Code, exitcode.MustAct, err)
	}
	if len(f.codes) != 0 {
		t.Fatal("a 2FA code was sent without a TOTP")
	}
}

func TestLoginMissingCredsIsUsageAndKeepsGuardFree(t *testing.T) {
	f := newFront(t, &front{})
	o := opts(t, f)
	o.Creds.Password = ""
	_, err := session.Login(t.Context(), o)
	if e := exitErr(t, err); e.Code != exitcode.Usage {
		t.Fatalf("code = %d, want %d", e.Code, exitcode.Usage)
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, "last-login")); err == nil {
		t.Error("a usage error consumed the login window")
	}
}

func TestLoginWrongPasswordDoesNotLeakIt(t *testing.T) {
	f := newFront(t, &front{})
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
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	f := newFront(t, &front{})
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
	f := newFront(t, &front{})
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
	if bytes.Contains(raw, []byte(testPass)) {
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
	f := newFront(t, &front{})
	o := opts(t, f)
	s, err := session.Login(t.Context(), o)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	uid := s.UID
	s.Close()
	before, _ := os.ReadFile(filepath.Join(o.StateDir, "session.json"))

	authBefore := f.authCalls
	r, err := session.Resume(t.Context(), o)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer r.Close()
	if r.UID != uid {
		t.Errorf("resumed UID = %q, want %q", r.UID, uid)
	}
	if f.authCalls != authBefore {
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
	f := newFront(t, &front{})
	_, err := session.Resume(t.Context(), opts(t, f))
	if e := exitErr(t, err); e.Code != exitcode.MustAct || !strings.Contains(e.Hint, "pcb-lab login") {
		t.Fatalf("got %v, want MustAct with a login hint", err)
	}
	if f.requests != 0 {
		t.Fatal("network used without a session")
	}
}

func TestRedactIDNeverReturnsTheWholeID(t *testing.T) {
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
