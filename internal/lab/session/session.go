// Package session logs the lab harness into the dedicated Proton test
// account and keeps the session between runs. It never unlocks keys itself:
// API hands a probe pcb's transport on the saved session, and Elevator the
// locked scope for the key salts, proven with a password the probe gets from
// the environment and never stores.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	proton "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/atomicfile"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
)

// DefaultAppVersion is the x-pm-appversion Proton accepts for third-party
// clients (verified live in the author's proton-cal fork).
const DefaultAppVersion = "Other"

const (
	sessionFile = "session.json"
	hvFile      = "hv.json"
	hvLogFile   = "hv-log.jsonl"
)

// Creds are the test account's credentials. They come from the environment.
type Creds struct {
	Username string
	Password string
	TOTP     *otp.TOTP // nil when the account has no 2FA
}

// Options configure Login and Resume.
type Options struct {
	BaseURL      string // empty: Proton's production API
	AppVersion   string // empty: DefaultAppVersion
	Creds        Creds
	CaptchaToken string           // token of a solved human verification
	Recorder     *record.Recorder // optional
	StateDir     string
	Now          func() time.Time // nil: time.Now; used for the guard and TOTP
	// Sleep waits d or until ctx ends; nil: a real timer. Tests inject it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Stderr receives the one-line warning when a rotated token cannot be
	// saved in the background; nil: os.Stderr.
	Stderr io.Writer
}

func (o Options) sleep(ctx context.Context, d time.Duration) error {
	if o.Sleep != nil {
		return o.Sleep(ctx, d)
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o Options) warn(err error) {
	w := o.Stderr
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "pcb-lab: cannot save the rotated session tokens: %v\n", err)
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Session is a logged-in test-account session.
type Session struct {
	UID    string
	Scopes []string
	Client *proton.Client

	manager *proton.Manager
	opts    Options
}

// Close releases the client and its manager.
func (s *Session) Close() {
	if s == nil {
		return
	}
	if s.Client != nil {
		s.Client.Close()
	}
	if s.manager != nil {
		s.manager.Close()
	}
}

// stored is the content of session.json (test account only, mode 0600).
type stored struct {
	UID          string   `json:"uid"`
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	Scopes       []string `json:"scopes,omitempty"`
}

type hvInfo struct {
	Token string `json:"token"`
	URL   string `json:"url"`
	At    string `json:"at"`
}

type quietLogger struct{}

func (quietLogger) Errorf(string, ...any) {}
func (quietLogger) Warnf(string, ...any)  {}
func (quietLogger) Debugf(string, ...any) {}

func newManager(o Options) *proton.Manager {
	app := o.AppVersion
	if app == "" {
		app = DefaultAppVersion
	}
	// No automatic retry: a 429 or 5xx is reported with its Retry-After at
	// once, never waited out or resent behind the caller (a resent login
	// would spend the login budget).
	opts := []proton.Option{proton.WithAppVersion(app), proton.WithLogger(quietLogger{}), proton.WithRetryCount(0)}
	if o.BaseURL != "" {
		opts = append(opts, proton.WithHostURL(o.BaseURL))
	}
	return proton.New(opts...)
}

func scopes(scope string) []string { return strings.Fields(scope) }

// Login logs into the test account: guard, SRP, human-verification stop,
// TOTP, then persists the session. It makes at most one /auth/v4 call.
func Login(ctx context.Context, o Options) (*Session, error) {
	if o.Creds.Username == "" || o.Creds.Password == "" {
		return nil, &exitcode.Error{
			Code: exitcode.Usage,
			Err:  errors.New("missing credentials"),
			Hint: "set PCB_LAB_USERNAME and PCB_LAB_PASSWORD, e.g. pass-cli run --env-file ~/.config/pcb-live/lab.env -- pcb-lab login (lab.env: a filled copy of scripts/lab.env.example)",
		}
	}
	if err := ensureDir(o.StateDir); err != nil {
		return nil, err
	}
	release, err := loginguard.Acquire(o.StateDir, loginguard.MinInterval, o.now(), nil)
	if err != nil {
		return nil, err
	}
	defer release()
	if o.Recorder != nil {
		o.Recorder.Forbid(o.Creds.Password)
	}

	m := newManager(o)
	var (
		c    *proton.Client
		auth proton.Auth
	)
	if o.CaptchaToken != "" {
		hv := &proton.APIHVDetails{Methods: []string{"captcha"}, Token: o.CaptchaToken}
		c, auth, err = m.NewClientWithLoginWithHVToken(ctx, o.Creds.Username, []byte(o.Creds.Password), hv)
	} else {
		c, auth, err = m.NewClientWithLogin(ctx, o.Creds.Username, []byte(o.Creds.Password))
	}
	if err != nil {
		m.Close()
		return nil, loginError(o, err)
	}
	s := &Session{UID: auth.UID, Scopes: scopes(auth.Scope), Client: c, manager: m, opts: o}
	if o.Recorder != nil {
		o.Recorder.Forbid(auth.AccessToken)
		o.Recorder.Forbid(auth.RefreshToken)
	}

	if auth.TwoFA.Enabled&proton.HasTOTP != 0 {
		if err := sendTOTP(ctx, o, c); err != nil {
			s.Close()
			return nil, err
		}
	} else if auth.TwoFA.Enabled != 0 {
		s.Close()
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("the test account requires FIDO2, which the lab does not support"), Hint: "enable TOTP on the test account"}
	}

	st := stored{UID: auth.UID, AccessToken: auth.AccessToken, RefreshToken: auth.RefreshToken, Scopes: s.Scopes}
	if err := save(o.StateDir, st); err != nil {
		s.Close()
		return nil, fmt.Errorf("saving the session: %w", err)
	}
	c.AddAuthHandler(o.saveRotations(&st))
	// The challenge is solved: its file is stale.
	if err := os.Remove(filepath.Join(o.StateDir, hvFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		o.warn(err)
	}
	return s, nil
}

func sendTOTP(ctx context.Context, o Options, c *proton.Client) error {
	if o.Creds.TOTP == nil {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the test account has 2FA but no TOTP was given"), Hint: "set PCB_LAB_TOTP"}
	}
	if wait := o.Creds.TOTP.Wait(o.now()); wait > 0 {
		if err := o.sleep(ctx, wait); err != nil {
			return err
		}
	}
	code := o.Creds.TOTP.Code(o.now())
	if o.Recorder != nil {
		o.Recorder.Forbid(code)
	}
	if err := c.Auth2FA(ctx, proton.Auth2FAReq{TwoFactorCode: code}); err != nil {
		return fmt.Errorf("2FA rejected: %w", apiMessage(err))
	}
	return nil
}

// logHV appends one line per human-verification event to hv-log.jsonl (E02's
// data): when it happened, the challenge token's length and whether the login
// had sent a solved token. Never the token itself. The log outlives hv.json.
func (o Options) logHV(tokenLen int) {
	line, _ := json.Marshal(struct {
		At       string `json:"at"`
		TokenLen int    `json:"token_len"`
		Sent     bool   `json:"sent_captcha_token"`
	}{o.now().UTC().Format(time.RFC3339), tokenLen, o.CaptchaToken != ""})
	f, err := os.OpenFile(filepath.Join(o.StateDir, hvLogFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = f.Write(append(line, '\n'))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		w := o.Stderr
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintf(w, "pcb-lab: cannot append to %s: %v\n", hvLogFile, err)
	}
}

// loginError turns a failed login into a stop the caller can act on. A
// human-verification challenge is saved to hv.json and never retried.
func loginError(o Options, err error) error {
	var apiErr *proton.APIError
	if errors.As(err, &apiErr) && apiErr.IsHVError() {
		d, derr := apiErr.GetHVDetails()
		if derr != nil || d == nil || d.Token == "" {
			return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("human verification requested without a usable token")}
		}
		o.logHV(len(d.Token))
		verifyURL := "https://verify.proton.me/?methods=captcha&token=" + url.QueryEscape(d.Token)
		info := hvInfo{Token: d.Token, URL: verifyURL, At: o.now().UTC().Format(time.RFC3339)}
		if b, merr := json.MarshalIndent(info, "", "  "); merr == nil {
			_ = atomicfile.Write(filepath.Join(o.StateDir, hvFile), b)
		}
		return &exitcode.Error{
			Code: exitcode.MustAct,
			Err:  fmt.Errorf("human verification required: solve the CAPTCHA at %s", verifyURL),
			Hint: "pcb-lab login --captcha-token " + d.Token,
		}
	}
	return fmt.Errorf("login failed: %w", apiMessage(err))
}

// apiMessage reduces err to Proton's code and message, or the bare error
// text, so nothing from the request (credentials) can ride along.
func apiMessage(err error) error {
	var apiErr *proton.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("%s (Proton code %d, HTTP %d)", apiErr.Message, apiErr.Code, apiErr.Status)
	}
	return err
}

// Resume rebuilds a client from session.json on the saved tokens and checks
// them with one GET /core/v4/users, without refreshing: the saved scopes stay
// as saved. On a rejected access token the client refreshes itself
// (go-proton-api's refresh, which sends the access token Proton requires;
// live finding 2026-11-12, E01) and the rotated tokens are saved. A refresh
// per command would rotate the session for nothing. An ended session (see
// ended) is MustAct, to log in again; any other failure of the check (429,
// 5xx, network) is Blocked, to retry later: it says nothing about the
// session and must not cost a login.
func Resume(ctx context.Context, o Options) (*Session, error) {
	st, err := load(o.StateDir)
	if err != nil {
		return nil, err
	}
	if o.Recorder != nil {
		o.Recorder.Forbid(st.AccessToken)
		o.Recorder.Forbid(st.RefreshToken)
	}
	m := newManager(o)
	c := m.NewClient(st.UID, st.AccessToken, st.RefreshToken)
	c.AddAuthHandler(o.saveRotations(&st))
	var deauthed bool // go-proton-api de-auths on a refresh refused with 400 or 422
	c.AddDeauthHandler(func() { deauthed = true })
	if _, err := c.GetUser(ctx); err != nil {
		c.Close()
		m.Close()
		if ended(err, deauthed) {
			return nil, &exitcode.Error{
				Code: exitcode.MustAct,
				Err:  fmt.Errorf("the saved session has ended: %w", apiMessage(err)),
				Hint: "pcb-lab login",
			}
		}
		return nil, &exitcode.Error{
			Code: exitcode.Blocked,
			Err:  fmt.Errorf("the saved session could not be checked: %w", apiMessage(err)),
			Hint: "retry later, after any Retry-After; the session may still be good, so do not log in for this",
		}
	}
	return &Session{UID: st.UID, Scopes: st.Scopes, Client: c, manager: m, opts: o}, nil
}

// ended reports a session Proton ended, as pcb classifies a refresh (ruling
// R6m): code 10013 (invalid refresh token), a 401 (the refresh, or the
// request retried after it), or a refresh refused with 422 (deauthed: the
// failed call was the refresh). A 400 with another code, a 422 on the check
// itself, a 429, a 5xx or a network error leave the session alone.
func ended(err error, deauthed bool) bool {
	var apiErr *proton.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code == 10013 || apiErr.Status == http.StatusUnauthorized ||
		(deauthed && apiErr.Status == http.StatusUnprocessableEntity)
}

// saveRotations is a client's auth handler: it redacts the rotated tokens
// from the recording and saves them, warning when the write fails.
func (o Options) saveRotations(st *stored) proton.AuthHandler {
	return func(a proton.Auth) {
		if o.Recorder != nil {
			o.Recorder.Forbid(a.AccessToken)
			o.Recorder.Forbid(a.RefreshToken)
		}
		if err := saveRotation(o.StateDir, st, a); err != nil {
			o.warn(err)
		}
	}
}

var saveMu sync.Mutex

// saveRotation persists tokens rotated by Proton and reports a failed write.
func saveRotation(dir string, st *stored, a proton.Auth) error {
	saveMu.Lock()
	defer saveMu.Unlock()
	if a.AccessToken != "" {
		st.AccessToken = a.AccessToken
	}
	if a.RefreshToken != "" {
		st.RefreshToken = a.RefreshToken
	}
	return save(dir, *st)
}

func save(dir string, st stored) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, sessionFile), b)
}

func load(dir string) (stored, error) {
	var st stored
	b, err := os.ReadFile(filepath.Join(dir, sessionFile))
	if errors.Is(err, os.ErrNotExist) {
		return st, &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("no saved lab session"), Hint: "pcb-lab login"}
	}
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil || st.UID == "" || st.RefreshToken == "" {
		return st, &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the saved lab session is unreadable"), Hint: "pcb-lab login"}
	}
	return st, nil
}

func ensureDir(dir string) error {
	if dir == "" {
		return errors.New("no state directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700) // tighten a directory that already existed
}

// RedactID shortens an identifier for display: the first four characters
// and the length, never the whole value.
func RedactID(id string) string {
	if len(id) <= 4 {
		return "…"
	}
	return fmt.Sprintf("%s… (%d chars)", id[:4], len(id))
}
