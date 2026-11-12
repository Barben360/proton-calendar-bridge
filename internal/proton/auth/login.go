// Package auth is C1's login: SRP through go-proton-api, the CAPTCHA (9001)
// stop, TOTP, refusal of the account kinds pcb does not support, token
// refresh, the locked scope and logout. Credential and verification failures
// are typed (sentinels and error types) so callers classify them without
// reading message text. No error or formatted value carries a secret.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	gpa "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
)

const (
	codePasswordWrong = 8002
	// cleanupTimeout bounds a revoke or a re-lock, which run even when the
	// caller's context is already cancelled.
	cleanupTimeout = 10 * time.Second
)

// callTimeout bounds each login call: Begin, SubmitTOTP and Scope.Unlock's
// two calls (ruling A1). A Proton that accepts the connection and never
// answers must not hold the login lock; go-proton-api's client has no
// timeout of its own. A variable for the tests.
var callTimeout = 30 * time.Second

// retryKey keys the Retry-After a login call's answers carried: the
// transport records it (go-proton-api's errors drop the headers) and
// protonError puts it in the *api.Error.
type retryKey struct{}

// callContext bounds one login call by callTimeout and records the
// Retry-After of its answers.
func callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	return context.WithValue(ctx, retryKey{}, new(atomic.Int64)), cancel
}

// retryAfter is the Retry-After the last answer of ctx's call carried.
func retryAfter(ctx context.Context) time.Duration {
	if ra, ok := ctx.Value(retryKey{}).(*atomic.Int64); ok {
		return time.Duration(ra.Load())
	}
	return 0
}

// cleanupContext keeps ctx's values but not its cancellation.
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// Credential and verification failures: the user must act (R23a). Test them
// with errors.Is.
var (
	ErrBadCredentials = errors.New("wrong Proton username or password")
	ErrTwoPassword    = errors.New("the Proton account uses two-password mode, which pcb does not support")
	ErrFIDO2          = errors.New("the Proton account's only second factor is a security key (FIDO2), which pcb does not support")
	ErrTOTPRejected   = errors.New("the 2FA code was rejected by Proton")
	// ErrCaptchaRejected: the CAPTCHA token sent to Begin is invalid or expired.
	ErrCaptchaRejected = errors.New("the CAPTCHA token was not accepted by Proton")
	ErrServerProof     = errors.New("the Proton server proof does not match: the server could not prove it knows the password")
)

// CaptchaError is a CAPTCHA to solve at URL; the next Begin passes Token.
type CaptchaError struct{ Token, URL string }

// Error implements error.
func (e *CaptchaError) Error() string {
	return "Proton asks for a CAPTCHA: solve it at " + e.URL
}

// UnsupportedVerificationError is a human verification without the captcha
// method, which pcb cannot complete.
type UnsupportedVerificationError struct{ Methods []string }

// Error implements error.
func (e *UnsupportedVerificationError) Error() string {
	return "Proton asks for human verification by " + strings.Join(e.Methods, ", ") + ", which pcb does not support"
}

// VerifyURL is the page that solves the CAPTCHA challenge token.
func VerifyURL(token string) string {
	return "https://verify.proton.me/?methods=captcha&token=" + url.QueryEscape(token)
}

// NewManager returns a go-proton-api manager for baseURL (required: its own
// default host is not ours) with pcb's identity and a silent logger. It never
// retries: one login attempt is one request, and writes are not repeated.
func NewManager(baseURL, appVersion string) *gpa.Manager {
	if appVersion == "" {
		appVersion = "Other"
	}
	return gpa.New(
		gpa.WithHostURL(baseURL),
		gpa.WithAppVersion(appVersion),
		gpa.WithLogger(silent{}),
		gpa.WithRetryCount(0),
		gpa.WithTransport(userAgent{newTransport()}),
	)
}

// newTransport clones http.DefaultTransport, or builds the same defaults when
// something replaced it with another RoundTripper.
func newTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

type silent struct{}

func (silent) Errorf(string, ...any) {}
func (silent) Warnf(string, ...any)  {}
func (silent) Debugf(string, ...any) {}

// userAgent sets pcb's User-Agent, which go-proton-api has no option for,
// and records the Retry-After of the answer for a call's callContext.
type userAgent struct{ next *http.Transport }

func (u userAgent) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", "pcb/"+buildinfo.Version)
	resp, err := u.next.RoundTrip(r)
	if ra, ok := r.Context().Value(retryKey{}).(*atomic.Int64); ok && resp != nil {
		ra.Store(int64(api.ParseRetryAfter(resp.Header, time.Now())))
	}
	return resp, err
}

// CloseIdleConnections lets Manager.Close reach the transport.
func (u userAgent) CloseIdleConnections() { u.next.CloseIdleConnections() }

// Pending is a login that passed SRP. Tokens follow go-proton-api's
// rotations. It is not safe for concurrent use; Close it when done.
type Pending struct {
	Tokens    api.Tokens
	UserID    string
	NeedsTOTP bool

	c     *gpa.Client
	ended bool // a refresh failed for good: the session is gone
}

const redacted = "auth.Pending{<redacted>}"

// String implements fmt.Stringer.
func (Pending) String() string { return redacted }

// GoString implements fmt.GoStringer (%#v).
func (Pending) GoString() string { return redacted }

// LogValue implements slog.LogValuer.
func (Pending) LogValue() slog.Value { return slog.StringValue(redacted) }

// Begin runs SRP for username. A non-empty captchaToken is a solved CAPTCHA
// challenge. Errors: *CaptchaError, *UnsupportedVerificationError,
// ErrCaptchaRejected, ErrBadCredentials, ErrServerProof, and ErrTwoPassword or
// ErrFIDO2 once the session they opened is revoked. Begin sends one
// POST /auth/v4, within callTimeout.
func Begin(ctx context.Context, m *gpa.Manager, username string, password []byte, captchaToken string) (*Pending, error) {
	var hv *gpa.APIHVDetails
	if captchaToken != "" {
		hv = &gpa.APIHVDetails{Methods: []string{"captcha"}, Token: captchaToken}
	}
	cctx, cancel := callContext(ctx)
	c, a, err := m.NewClientWithLoginWithHVToken(cctx, username, password, hv)
	cancel()
	if err != nil {
		return nil, loginError(cctx, err)
	}
	p := &Pending{
		Tokens:    api.Tokens{UID: a.UID, AccessToken: a.AccessToken, RefreshToken: a.RefreshToken},
		UserID:    a.UserID,
		NeedsTOTP: a.TwoFA.Enabled&gpa.HasTOTP != 0,
		c:         c,
	}
	c.AddAuthHandler(func(r gpa.Auth) {
		p.Tokens.AccessToken, p.Tokens.RefreshToken = r.AccessToken, r.RefreshToken
	})
	c.AddDeauthHandler(func() { p.ended = true })
	var refused error
	switch {
	case a.PasswordMode == gpa.TwoPasswordMode:
		refused = ErrTwoPassword
	case a.TwoFA.Enabled != 0 && !p.NeedsTOTP:
		refused = ErrFIDO2
	default:
		return p, nil
	}
	if err := p.revoke(ctx); err != nil {
		refused = errors.Join(refused, err)
	}
	p.Close()
	return nil, refused
}

// revoke deletes the session, even when ctx is cancelled.
func (p *Pending) revoke(ctx context.Context) error {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	if err := p.c.AuthDelete(ctx); err != nil {
		return fmt.Errorf("revoking the session: %w", protonError(ctx, http.MethodDelete, "/auth/v4", err))
	}
	return nil
}

// loginError classifies a failed SRP login of ctx's call.
func loginError(ctx context.Context, err error) error {
	// go-proton-api compares the login's ServerProof (bytes.Equal) and, on a
	// mismatch, returns no client: the session the server may have opened
	// cannot be revoked from here. It expires server-side; its tokens are
	// never kept.
	if errors.Is(err, gpa.ErrInvalidProof) {
		return ErrServerProof
	}
	perr := protonError(ctx, http.MethodPost, "/auth/v4", err)
	var ge *gpa.APIError
	if !errors.As(err, &ge) {
		return fmt.Errorf("proton login: %w", perr)
	}
	switch {
	case ge.Code == gpa.HumanVerificationRequired:
		d, derr := ge.GetHVDetails()
		switch {
		case derr != nil || d == nil:
			return fmt.Errorf("proton login: unreadable human verification: %w", perr)
		case !slices.Contains(d.Methods, "captcha"):
			return &UnsupportedVerificationError{Methods: d.Methods}
		case d.Token == "":
			return fmt.Errorf("proton login: CAPTCHA without a token: %w", perr)
		}
		return &CaptchaError{Token: d.Token, URL: VerifyURL(d.Token)}
	case ge.Code == gpa.HumanValidationInvalidToken:
		return fmt.Errorf("%w: %w", ErrCaptchaRejected, perr)
	case wrongPassword(perr):
		return fmt.Errorf("%w: %w", ErrBadCredentials, perr)
	}
	return fmt.Errorf("proton login: %w", perr)
}

// SubmitTOTP sends the 2FA code, within callTimeout. A refused code (HTTP
// 422) is ErrTOTPRejected; a session that ended meanwhile (401, or a refused
// refresh) wraps api.ErrSessionEnded; anything else is a plain error.
func (p *Pending) SubmitTOTP(ctx context.Context, code string) error {
	if p.c == nil {
		return errors.New("auth: the login is closed")
	}
	cctx, cancel := callContext(ctx)
	err := p.c.Auth2FA(cctx, gpa.Auth2FAReq{TwoFactorCode: code})
	cancel()
	if err == nil {
		p.NeedsTOTP = false
		return nil
	}
	perr := protonError(cctx, http.MethodPost, "/auth/v4/2fa", err)
	var ge *gpa.APIError
	switch {
	case p.ended || (errors.As(err, &ge) && ge.Status == http.StatusUnauthorized):
		return fmt.Errorf("%w: %w", api.ErrSessionEnded, perr)
	case errors.As(err, &ge) && ge.Status == http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %w", ErrTOTPRejected, perr)
	}
	return fmt.Errorf("proton 2FA: %w", perr)
}

// Abort revokes the pending session (best effort, even when ctx is
// cancelled), then closes it.
func (p *Pending) Abort(ctx context.Context) {
	if p.c != nil {
		_ = p.revoke(ctx)
	}
	p.Close()
}

// Close forgets the go-proton-api client. The session stays valid.
func (p *Pending) Close() {
	if p.c != nil {
		p.c.Close()
		p.c = nil
	}
}

// Logout revokes c's session (DELETE /auth/v4).
func Logout(ctx context.Context, c *api.Client) error {
	return c.Delete(ctx, "/auth/v4", nil)
}

// protonError turns a go-proton-api failure into an *api.Error (so callers
// test codes with api.IsCode), with the Retry-After ctx's call recorded,
// and strips the request URL from transport errors. Nothing from the
// request body survives.
func protonError(ctx context.Context, method, path string, err error) error {
	var ge *gpa.APIError
	if errors.As(err, &ge) {
		return &api.Error{Method: method, Path: path, Status: ge.Status, Code: int(ge.Code), Message: ge.Message, RetryAfter: retryAfter(ctx)}
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
