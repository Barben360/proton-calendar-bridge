package protontest_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

func login(t *testing.T, f *protontest.Fake, user, pass string) gpa.Auth {
	t.Helper()
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	c, a, err := m.NewClientWithLogin(t.Context(), user, []byte(pass))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	return a
}

// getUser sends one raw GET /core/v4/users with a's tokens and returns the
// status, the Proton code and the headers.
func getUser(t *testing.T, f *protontest.Fake, a gpa.Auth) (status, code int, hdr http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.URL+"/core/v4/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-pm-appversion", "Other")
	req.Header.Set("x-pm-uid", a.UID)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck // test
	var env struct{ Code int }
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env.Code, resp.Header
}

func TestCallsMatchesExactlyOrByPrefix(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	login(t, f, f.Username, f.Password)
	cases := []struct {
		method, path string
		want         int
	}{
		{"POST", "/auth/v4", 1},
		{"POST", "/auth/v4/info", 1},
		{"POST", "/auth/v4/", 1}, // the prefix form: /auth/v4/info only
		{"", "/auth/v4/info", 1},
		{"GET", "/auth/v4", 0},
	}
	for _, c := range cases {
		if got := f.Calls(c.method, c.path); got != c.want {
			t.Errorf("Calls(%q, %q) = %d, want %d", c.method, c.path, got, c.want)
		}
	}
}

func TestFailNextAnswersOnceWithRetryAfter(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	a := login(t, f, f.Username, f.Password)
	f.FailNext("GET", "/core/v4/users", 429, 2028, 7)
	status, code, hdr := getUser(t, f, a)
	if status != 429 || code != 2028 || hdr.Get("Retry-After") != "7" {
		t.Fatalf("first answer = %d/%d Retry-After %q, want 429/2028 Retry-After 7", status, code, hdr.Get("Retry-After"))
	}
	if status, code, _ := getUser(t, f, a); status != 200 || code != 1000 {
		t.Fatalf("second answer = %d/%d, want 200/1000", status, code)
	}
}

func TestExpireAccessTokensUntilRefreshed(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	a := login(t, f, f.Username, f.Password)
	f.ExpireAccessTokens()
	if status, _, _ := getUser(t, f, a); status != 401 {
		t.Fatalf("expired token answered %d, want 401", status)
	}
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	c, fresh, err := m.NewClientWithRefresh(t.Context(), a.UID, a.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	fresh.UID = a.UID
	if status, _, _ := getUser(t, f, fresh); status != 200 {
		t.Fatalf("refreshed token answered %d, want 200", status)
	}
}

func TestAddUserLogsInWithItsOwnPassword(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	id := f.AddUser("second", "second-pass")
	if id == "" || f.UserID("second") != id || f.UserID(f.Username) == "" || f.UserID("nobody") != "" {
		t.Fatalf("UserID: second=%q (AddUser %q), default=%q, nobody=%q", f.UserID("second"), id, f.UserID(f.Username), f.UserID("nobody"))
	}
	if a := login(t, f, "second", "second-pass"); a.UserID != id {
		t.Fatalf("logged in as %q, want %q", a.UserID, id)
	}
}

func TestSuccessesCarryCode1000(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	a := login(t, f, f.Username, f.Password)
	if status, code, _ := getUser(t, f, a); status != 200 || code != 1000 {
		t.Fatalf("answer = %d/%d, want 200/1000", status, code)
	}
}

func TestSetCaptchaChallengesLaterLogins(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	_ = login(t, f, f.Username, f.Password) // no challenge yet
	f.SetCaptcha("T9")
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	_, _, err := m.NewClientWithLogin(t.Context(), f.Username, []byte(f.Password))
	var ae *gpa.APIError
	if !errors.As(err, &ae) || ae.Code != 9001 {
		t.Fatalf("login after SetCaptcha: %v, want a 9001 challenge", err)
	}
}

func TestTOTPCodesRecordsSubmittedCodes(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{TwoFA: true})
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	c, _, err := m.NewClientWithLogin(t.Context(), f.Username, []byte(f.Password))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.Auth2FA(t.Context(), gpa.Auth2FAReq{TwoFactorCode: "123456"})
	_ = c.Auth2FA(t.Context(), gpa.Auth2FAReq{TwoFactorCode: "654321"})
	if got := f.TOTPCodes(); len(got) != 2 || got[0] != "123456" || got[1] != "654321" {
		t.Fatalf("TOTPCodes = %q", got)
	}
}

func TestFailNextForHitsOnlyThatUser(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	f.AddUser("b", "b-pass")
	a := login(t, f, f.Username, f.Password)
	b := login(t, f, "b", "b-pass")
	f.FailNextFor("b", "GET", "/core/v4/users", 500, 0, 0)
	if status, _, _ := getUser(t, f, a); status != 200 {
		t.Fatalf("call as the other user = %d, want 200", status)
	}
	if status, _, _ := getUser(t, f, b); status != 500 {
		t.Fatalf("call as b = %d, want 500", status)
	}
	if status, _, _ := getUser(t, f, b); status != 200 {
		t.Fatalf("second call as b = %d, want 200 (one failure queued)", status)
	}
}

func TestStallNextForHitsOnlyThatUser(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	f.AddUser("b", "b-pass")
	a := login(t, f, f.Username, f.Password)
	b := login(t, f, "b", "b-pass")
	f.StallNextFor("b", "GET", "/core/v4/users", 300*time.Millisecond)
	start := time.Now()
	if status, _, _ := getUser(t, f, a); status != 200 || time.Since(start) > 200*time.Millisecond {
		t.Fatalf("call as the other user = %d after %v, want 200 without a stall", status, time.Since(start))
	}
	start = time.Now()
	if status, _, _ := getUser(t, f, b); status != 200 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("call as b = %d after %v, want 200 after the 300ms stall", status, time.Since(start))
	}
}

func TestCaptchaForChallengesOnlyThatUser(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	f.AddUser("b", "b-pass")
	f.SetCaptchaFor("b", "tok")
	_ = login(t, f, f.Username, f.Password) // not challenged
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	_, _, err := m.NewClientWithLogin(t.Context(), "b", []byte("b-pass"))
	var ae *gpa.APIError
	if !errors.As(err, &ae) || ae.Code != 9001 || ae.Status != 422 || !strings.Contains(string(ae.Details), "tok") {
		t.Fatalf("login as b: %v (%s), want 422/9001 with token tok", err, ae.Details)
	}
}

func TestTwoFAPerUser(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	f.AddUser("b", "b-pass")
	uri := f.SetTwoFA("b", true)
	if a := login(t, f, f.Username, f.Password); a.TwoFA.Enabled != 0 {
		t.Fatalf("other user's 2FA.Enabled = %d, want 0", a.TwoFA.Enabled)
	}
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	c, a, err := m.NewClientWithLogin(t.Context(), "b", []byte("b-pass"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if a.TwoFA.Enabled != 1 {
		t.Fatalf("b's 2FA.Enabled = %d, want 1", a.TwoFA.Enabled)
	}
	if err := c.Auth2FA(t.Context(), gpa.Auth2FAReq{TwoFactorCode: "000000"}); err == nil {
		t.Fatal("a wrong code was accepted")
	}
	key, err := otp.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Auth2FA(t.Context(), gpa.Auth2FAReq{TwoFactorCode: key.Code(time.Now())}); err != nil {
		t.Fatalf("code from the returned URI refused: %v", err)
	}
}
