package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/auth"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

var t0 = time.Date(2026, 11, 11, 10, 0, 7, 0, time.UTC)

func manager(t *testing.T, f *protontest.Fake) *gpa.Manager {
	t.Helper()
	m := auth.NewManager(f.URL, "Other")
	t.Cleanup(m.Close)
	return m
}

// begin logs into f's account and closes the pending login with the test.
func begin(t *testing.T, f *protontest.Fake, m *gpa.Manager) *auth.Pending {
	t.Helper()
	p, err := auth.Begin(t.Context(), m, f.Username, []byte(f.Password), "")
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// client is an api.Client on f with tok and no refresh.
func client(f *protontest.Fake, tok api.Tokens) *api.Client {
	return api.New(tok, api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	})
}

func TestNewManagerSendsIdentity(t *testing.T) {
	t.Parallel()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"Code":8002,"Error":"no"}`))
	}))
	t.Cleanup(srv.Close)
	m := auth.NewManager(srv.URL, "Other")
	t.Cleanup(m.Close)
	_, _ = auth.Begin(t.Context(), m, "u", []byte("p"), "")
	if got.Get("x-pm-appversion") != "Other" || got.Get("User-Agent") != "pcb/"+buildinfo.Version {
		t.Fatalf("x-pm-appversion = %q, User-Agent = %q; want Other, pcb/%s", got.Get("x-pm-appversion"), got.Get("User-Agent"), buildinfo.Version)
	}
}

func TestBeginPasswordOnly(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	p := begin(t, f, manager(t, f))
	if p.NeedsTOTP {
		t.Error("NeedsTOTP = true for a password-only account")
	}
	if p.Tokens.UID == "" || p.Tokens.AccessToken == "" || p.Tokens.RefreshToken == "" {
		t.Errorf("tokens not all set: UID %t, access %t, refresh %t", p.Tokens.UID != "", p.Tokens.AccessToken != "", p.Tokens.RefreshToken != "")
	}
	if p.UserID != f.UserID(f.Username) {
		t.Errorf("UserID = %q, want %q", p.UserID, f.UserID(f.Username))
	}
	if n := f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("POST /auth/v4 calls = %d, want 1", n)
	}
	if err := client(f, p.Tokens).Get(t.Context(), "/core/v4/users", nil, nil); err != nil {
		t.Errorf("the tokens do not authenticate: %v", err)
	}
}

func TestBeginCaptchaReturnsChallenge(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{Captcha: "T1"})
	_, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "")
	var ce *auth.CaptchaError
	if !errors.As(err, &ce) {
		t.Fatalf("error %v is not a *CaptchaError", err)
	}
	if ce.Token != "T1" || ce.URL != "https://verify.proton.me/?methods=captcha&token=T1" {
		t.Errorf("challenge = %q at %q", ce.Token, ce.URL)
	}
	if n := f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("POST /auth/v4 calls = %d, want 1 (no retry)", n)
	}
}

func TestBeginWithCaptchaTokenSendsBothHeaders(t *testing.T) {
	t.Parallel()
	// The fake accepts the login only when the token and its type are sent.
	f := protontest.New(t, protontest.Options{Captcha: "T1"})
	p, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "T1")
	if err != nil {
		t.Fatalf("Begin with the solved token: %v", err)
	}
	p.Close()
	if n := f.Calls("POST", "/auth/v4"); n != 1 {
		t.Errorf("POST /auth/v4 calls = %d, want 1", n)
	}
}

func TestBeginRejectedCaptchaToken(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{Captcha: "T1"})
	f.FailNext("POST", "/auth/v4", http.StatusUnprocessableEntity, 12087, 0)
	_, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "T1")
	if !errors.Is(err, auth.ErrCaptchaRejected) || errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("Begin with a refused CAPTCHA token = %v, want ErrCaptchaRejected only", err)
	}
}

func TestVerifyURLEscapesTheToken(t *testing.T) {
	t.Parallel()
	if got, want := auth.VerifyURL("a b&c=d"), "https://verify.proton.me/?methods=captcha&token=a+b%26c%3Dd"; got != want {
		t.Fatalf("VerifyURL = %q, want %q", got, want)
	}
}

func TestBeginNonCaptchaMethodsUnsupported(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{Captcha: "T1", CaptchaMethods: []string{"sms", "email"}})
	_, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "")
	var ue *auth.UnsupportedVerificationError
	if !errors.As(err, &ue) {
		t.Fatalf("error %v is not an *UnsupportedVerificationError", err)
	}
	if !slices.Equal(ue.Methods, []string{"sms", "email"}) || !strings.Contains(err.Error(), "sms") || !strings.Contains(err.Error(), "email") {
		t.Errorf("methods = %q, message %q; want sms and email listed", ue.Methods, err)
	}
	var ce *auth.CaptchaError
	if errors.As(err, &ce) {
		t.Error("a non-CAPTCHA challenge was reported as a CAPTCHA")
	}
}

func TestBeginWrongPassword(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	wrong := "wrong-" + f.Password
	_, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(wrong), "")
	if !errors.Is(err, auth.ErrBadCredentials) {
		t.Fatalf("error = %v, want ErrBadCredentials", err)
	}
	if text := fmt.Sprintf("%v %+v %#v", err, err, err); strings.Contains(text, wrong) || strings.Contains(text, f.Password) {
		t.Fatalf("the error text contains the password: %s", text)
	}
}

func TestBeginRefusesTwoPassword(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{TwoPassword: true})
	p, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "")
	if !errors.Is(err, auth.ErrTwoPassword) || p != nil {
		t.Fatalf("Begin = %v, %v; want nil, ErrTwoPassword", p, err)
	}
	if n := f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Fatalf("DELETE /auth/v4 calls = %d, want 1 (the opened session revoked)", n)
	}
}

func TestBeginRefusesFIDO2Only(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{FIDO2Only: true})
	p, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "")
	if !errors.Is(err, auth.ErrFIDO2) || p != nil {
		t.Fatalf("Begin = %v, %v; want nil, ErrFIDO2", p, err)
	}
	if n := f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Fatalf("DELETE /auth/v4 calls = %d, want 1 (the opened session revoked)", n)
	}
}

func totpAccount(t *testing.T) (*protontest.Fake, *otp.TOTP) {
	t.Helper()
	f := protontest.New(t, protontest.Options{TwoFA: true, Now: func() time.Time { return t0 }})
	tp, err := otp.Parse(f.TOTPURI)
	if err != nil {
		t.Fatal(err)
	}
	return f, tp
}

func TestSubmitTOTPAcceptsCurrentCode(t *testing.T) {
	t.Parallel()
	f, tp := totpAccount(t)
	p := begin(t, f, manager(t, f))
	if !p.NeedsTOTP {
		t.Fatal("NeedsTOTP = false for a TOTP account")
	}
	if err := p.SubmitTOTP(t.Context(), tp.Code(t0)); err != nil {
		t.Fatalf("SubmitTOTP(current code): %v", err)
	}
	if p.NeedsTOTP {
		t.Error("NeedsTOTP still true after an accepted code")
	}
}

func TestSubmitTOTPRejectsWrongCode(t *testing.T) {
	t.Parallel()
	f, tp := totpAccount(t)
	p := begin(t, f, manager(t, f))
	wrong := []byte(tp.Code(t0))
	for string(wrong) == tp.Code(t0) || string(wrong) == tp.Code(t0.Add(-30*time.Second)) {
		wrong[5] = '0' + (wrong[5]-'0'+1)%10
	}
	err := p.SubmitTOTP(t.Context(), string(wrong))
	if !errors.Is(err, auth.ErrTOTPRejected) {
		t.Fatalf("SubmitTOTP(wrong) = %v, want ErrTOTPRejected", err)
	}
	if strings.Contains(err.Error(), string(wrong)) {
		t.Fatalf("the error text contains the code: %v", err)
	}
	if !p.NeedsTOTP {
		t.Error("NeedsTOTP = false after a rejected code")
	}
}

func TestSubmitTOTPKeepsRotatedTokens(t *testing.T) {
	t.Parallel()
	f, tp := totpAccount(t)
	p := begin(t, f, manager(t, f))
	before := p.Tokens
	f.ExpireAccessTokens() // the 2FA call gets a 401 and go-proton-api refreshes
	if err := p.SubmitTOTP(t.Context(), tp.Code(t0)); err != nil {
		t.Fatalf("SubmitTOTP: %v", err)
	}
	if p.Tokens.AccessToken == before.AccessToken || p.Tokens.RefreshToken == before.RefreshToken || p.Tokens.UID != before.UID {
		t.Fatal("Pending.Tokens did not follow the rotation")
	}
	if err := client(f, p.Tokens).Get(t.Context(), "/core/v4/users", nil, nil); err != nil {
		t.Fatalf("the rotated tokens do not authenticate: %v", err)
	}
}

func TestSubmitTOTPAfterRevokeIsSessionEnded(t *testing.T) {
	t.Parallel()
	f, tp := totpAccount(t)
	p := begin(t, f, manager(t, f))
	f.RevokeSessions() // 401, then the refresh go-proton-api tries is refused
	err := p.SubmitTOTP(t.Context(), tp.Code(t0))
	if !errors.Is(err, api.ErrSessionEnded) || errors.Is(err, auth.ErrTOTPRejected) {
		t.Fatalf("SubmitTOTP on a revoked session = %v, want ErrSessionEnded only", err)
	}
}

func TestAbortRevokesThePendingSession(t *testing.T) {
	t.Parallel()
	f, _ := totpAccount(t)
	p := begin(t, f, manager(t, f))
	tok := p.Tokens
	p.Abort(t.Context())
	if n := f.Calls("DELETE", "/auth/v4"); n != 1 {
		t.Fatalf("DELETE /auth/v4 calls = %d, want 1", n)
	}
	if err := client(f, tok).Get(t.Context(), "/core/v4/users", nil, nil); !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("the aborted session still answers: %v", err)
	}
}

func TestResumeAdoptsASessionWithoutSRP(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := manager(t, f)
	p := begin(t, f, m)
	tok, id := p.Tokens, p.UserID
	p.Close()
	logins := f.Calls("POST", "/auth/")
	r := auth.Resume(m, tok, id)
	if r.Tokens != tok || r.UserID != id || r.NeedsTOTP {
		t.Fatalf("Resume = {%v %q %v}, want the session's tokens and user, no 2FA", r.Tokens == tok, r.UserID, r.NeedsTOTP)
	}
	if n := f.Calls("POST", "/auth/"); n != logins {
		t.Fatalf("Resume sent %d auth calls, want none", n-logins)
	}
	r.Abort(t.Context())
	if err := client(f, tok).Get(t.Context(), "/core/v4/users", nil, nil); !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("the resumed session survived Abort: %v", err)
	}
}

func TestRefreshRotates(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := manager(t, f)
	p := begin(t, f, m)
	fresh, err := auth.Refresh(t.Context(), m, p.Tokens)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if fresh.UID != p.Tokens.UID || fresh.AccessToken == p.Tokens.AccessToken || fresh.RefreshToken == p.Tokens.RefreshToken || fresh.AccessToken == "" || fresh.RefreshToken == "" {
		t.Fatal("Refresh did not rotate both tokens on the same UID")
	}
	if err := client(f, fresh).Get(t.Context(), "/core/v4/users", nil, nil); err != nil {
		t.Fatalf("the refreshed tokens do not authenticate: %v", err)
	}
	if _, err := auth.Refresh(t.Context(), m, p.Tokens); !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("reusing the spent refresh token = %v, want ErrSessionEnded", err)
	}
}

// Proton refuses a refreshed pair's refresh token unless the refresh carries
// that pair's access token (measured live 2026-11-12, E01): pcb's client
// survives two 401 → refresh cycles in a row, the second being the one Proton
// refused.
func TestClientSurvivesTwoRefreshesInARow(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := manager(t, f)
	tok := f.Login(t, f.Username)
	c := api.New(tok, api.Options{
		BaseURL: f.URL,
		Limiter: rate.NewLimiter(rate.Inf, 1),
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		Refresh: func(ctx context.Context, stale api.Tokens) (api.Tokens, error) { return auth.Refresh(ctx, m, stale) },
	})
	seen := map[string]bool{tok.RefreshToken: true}
	for i := 1; i <= 2; i++ {
		f.ExpireAccessTokens()
		if err := c.Get(t.Context(), "/core/v4/users", nil, nil); err != nil {
			t.Fatalf("GET after expiry %d: %v", i, err)
		}
		if rt := c.Tokens().RefreshToken; seen[rt] {
			t.Fatalf("expiry %d: the tokens did not rotate", i)
		} else {
			seen[rt] = true
		}
	}
	if n := f.Calls("POST", "/auth/v4/refresh"); n != 2 {
		t.Fatalf("%d refreshes, want 2", n)
	}
}

// A manager that does not come from NewManager lacks the hook that puts the
// access token in the refresh: Refresh sends nothing rather than a refresh
// Proton would refuse, ending the session.
func TestRefreshRefusesAForeignManagerWithoutSending(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := gpa.New(gpa.WithHostURL(f.URL), gpa.WithAppVersion("Other"), gpa.WithRetryCount(0))
	t.Cleanup(m.Close)
	tok := f.Login(t, f.Username)
	_, err := auth.Refresh(t.Context(), m, tok)
	if err == nil || errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("Refresh with a foreign manager = %v, want a plain error", err)
	}
	if n := f.Calls("POST", "/auth/v4/refresh"); n != 0 {
		t.Fatalf("%d refreshes sent, want none", n)
	}
	if strings.Contains(err.Error(), tok.AccessToken) || strings.Contains(err.Error(), tok.RefreshToken) {
		t.Fatalf("the error carries a token: %v", err)
	}
}

func TestRefreshAfterRevokeIsSessionEnded(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := manager(t, f)
	p := begin(t, f, m)
	f.RevokeSessions()
	_, err := auth.Refresh(t.Context(), m, p.Tokens)
	if !errors.Is(err, api.ErrSessionEnded) || !api.IsCode(err, api.CodeRefreshInvalid) {
		t.Fatalf("Refresh after revoke = %v, want ErrSessionEnded with code 10013", err)
	}
}

func TestRefreshNetworkErrorIsTransient(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	m := auth.NewManager(url, "Other")
	t.Cleanup(m.Close)
	_, err := auth.Refresh(t.Context(), m, api.Tokens{UID: "u", AccessToken: "a", RefreshToken: "r"})
	if err == nil || errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("Refresh on a closed server = %v, want a transient error", err)
	}
}

func TestLogoutRevokes(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	p := begin(t, f, manager(t, f))
	if err := auth.Logout(t.Context(), client(f, p.Tokens)); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	err := client(f, p.Tokens).Get(t.Context(), "/core/v4/users", nil, nil)
	if !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("the same tokens after Logout = %v, want ErrSessionEnded (401)", err)
	}
}

func TestPendingAndScopeRedactSecrets(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	p := begin(t, f, manager(t, f))
	s := auth.Scope{Username: f.Username, Password: []byte(f.Password)}
	for _, v := range []any{p, *p, s, &s} {
		text := fmt.Sprintf("%v %+v %#v %s", v, v, v, v)
		for _, secret := range []string{f.Password, p.Tokens.AccessToken, p.Tokens.RefreshToken} {
			if strings.Contains(text, secret) {
				t.Fatalf("%T formats a secret: %s", v, text)
			}
		}
	}
}

// otherTransport is not an *http.Transport.
type otherTransport struct{ http.RoundTripper }

// Not parallel: it swaps http.DefaultTransport while the parallel tests wait.
func TestNewManagerToleratesAReplacedDefaultTransport(t *testing.T) {
	saved := http.DefaultTransport
	http.DefaultTransport = otherTransport{saved}
	t.Cleanup(func() { http.DefaultTransport = saved })
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"Code":8002,"Error":"no"}`))
	}))
	t.Cleanup(srv.Close)
	m := auth.NewManager(srv.URL, "Other")
	t.Cleanup(m.Close)
	if _, err := auth.Begin(t.Context(), m, "u", []byte("p"), ""); !errors.Is(err, auth.ErrBadCredentials) || calls != 1 {
		t.Fatalf("Begin = %v after %d requests, want ErrBadCredentials after 1", err, calls)
	}
}

func TestBeginRefusalReportsAFailedRevoke(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		o    protontest.Options
		want error
	}{
		"two-password": {protontest.Options{TwoPassword: true}, auth.ErrTwoPassword},
		"FIDO2 only":   {protontest.Options{FIDO2Only: true}, auth.ErrFIDO2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := protontest.New(t, c.o)
			f.FailNext("DELETE", "/auth/v4", http.StatusInternalServerError, 0, 0)
			p, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "")
			var e *api.Error
			if !errors.Is(err, c.want) || !errors.As(err, &e) || e.Status != http.StatusInternalServerError || p != nil {
				t.Fatalf("Begin = %v, %v; want nil and %v joined with the failed revoke (HTTP 500)", p, err, c.want)
			}
			if strings.Contains(err.Error(), f.Password) || strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("the error text carries a secret: %v", err)
			}
			if n := f.Calls("DELETE", "/auth/v4"); n != 1 {
				t.Fatalf("DELETE /auth/v4 calls = %d, want 1", n)
			}
		})
	}
}

func TestBeginRejectsBadLoginProof(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{BadLoginProof: true})
	p, err := auth.Begin(t.Context(), manager(t, f), f.Username, []byte(f.Password), "")
	if !errors.Is(err, auth.ErrServerProof) || p != nil {
		t.Fatalf("Begin = %v, %v; want nil, ErrServerProof", p, err)
	}
}

func TestAbortRevokesEvenWhenCancelled(t *testing.T) {
	t.Parallel()
	f, _ := totpAccount(t)
	p := begin(t, f, manager(t, f))
	tok := p.Tokens
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p.Abort(ctx)
	if err := client(f, tok).Get(t.Context(), "/core/v4/users", nil, nil); !errors.Is(err, api.ErrSessionEnded) {
		t.Fatalf("the session aborted with a cancelled context still answers: %v", err)
	}
}

func TestRefreshEndsTheSessionOnlyOnRefusal(t *testing.T) {
	t.Parallel()
	f := protontest.New(t, protontest.Options{})
	m := manager(t, f)
	stale := api.Tokens{UID: "u", AccessToken: "a", RefreshToken: "r"}
	for _, c := range []struct {
		status, code int
		ended        bool
	}{
		{http.StatusUnauthorized, 0, true},
		{http.StatusUnprocessableEntity, 0, true},
		{http.StatusBadRequest, api.CodeRefreshInvalid, true},
		{http.StatusBadRequest, 2001, false},
		{http.StatusBadRequest, 5003, false},
		{http.StatusForbidden, 0, false},
		{http.StatusTooManyRequests, 2028, false},
		{http.StatusInternalServerError, 0, false},
	} {
		f.FailNext("POST", "/auth/v4/refresh", c.status, c.code, 0)
		_, err := auth.Refresh(t.Context(), m, stale)
		if err == nil || errors.Is(err, api.ErrSessionEnded) != c.ended {
			t.Errorf("refresh answered %d/%d: err = %v, session ended = %t, want %t", c.status, c.code, err, errors.Is(err, api.ErrSessionEnded), c.ended)
		}
	}
}

func TestSubmitTOTPRejectedOnlyOn422(t *testing.T) {
	t.Parallel()
	f, tp := totpAccount(t)
	p := begin(t, f, manager(t, f))
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		f.FailNext("POST", "/auth/v4/2fa", status, protontest.CodeTOTPWrong, 0)
		err := p.SubmitTOTP(t.Context(), tp.Code(t0))
		if err == nil || errors.Is(err, auth.ErrTOTPRejected) || errors.Is(err, api.ErrSessionEnded) {
			t.Errorf("2FA answered %d: err = %v, want a plain error", status, err)
		}
	}
}
