// Package session logs the lab harness into the dedicated Proton test
// account and keeps the session between runs. It never unlocks keys.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	proton "github.com/ProtonMail/go-proton-api"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
)

// DefaultAppVersion is the x-pm-appversion Proton accepts for third-party
// clients (verified live in the author's proton-cal fork).
const DefaultAppVersion = "Other"

const (
	sessionFile = "session.json"
	hvFile      = "hv.json"
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
	opts := []proton.Option{proton.WithAppVersion(app), proton.WithLogger(quietLogger{})}
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
			Hint: "set PCB_LAB_USERNAME and PCB_LAB_PASSWORD, e.g. pass-cli run --env-file scripts/lab.env.example -- pcb-lab login",
		}
	}
	if err := ensureDir(o.StateDir); err != nil {
		return nil, err
	}
	if err := checkGuard(o.StateDir, o.now()); err != nil {
		return nil, err
	}
	if err := stampLogin(o.StateDir, o.now()); err != nil {
		return nil, fmt.Errorf("recording the login time: %w", err)
	}
	if o.Recorder != nil {
		o.Recorder.Forbid(o.Creds.Password)
	}

	m := newManager(o)
	var (
		c    *proton.Client
		auth proton.Auth
		err  error
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
	s := &Session{UID: auth.UID, Scopes: scopes(auth.Scope), Client: c, manager: m}
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
	c.AddAuthHandler(func(a proton.Auth) { saveRotation(o.StateDir, &st, a) })
	if err := save(o.StateDir, st); err != nil {
		s.Close()
		return nil, fmt.Errorf("saving the session: %w", err)
	}
	return s, nil
}

func sendTOTP(ctx context.Context, o Options, c *proton.Client) error {
	if o.Creds.TOTP == nil {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the test account has 2FA but no TOTP was given"), Hint: "set PCB_LAB_TOTP"}
	}
	if wait := o.Creds.TOTP.Wait(o.now()); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
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

// loginError turns a failed login into a stop the caller can act on. A
// human-verification challenge is saved to hv.json and never retried.
func loginError(o Options, err error) error {
	var apiErr *proton.APIError
	if errors.As(err, &apiErr) && apiErr.IsHVError() {
		d, derr := apiErr.GetHVDetails()
		if derr != nil || d == nil || d.Token == "" {
			return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("human verification requested without a usable token")}
		}
		verifyURL := "https://verify.proton.me/?methods=captcha&token=" + url.QueryEscape(d.Token)
		info := hvInfo{Token: d.Token, URL: verifyURL, At: o.now().UTC().Format(time.RFC3339)}
		if b, merr := json.MarshalIndent(info, "", "  "); merr == nil {
			_ = writeFile(filepath.Join(o.StateDir, hvFile), b)
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

// Resume rebuilds a client from session.json by refreshing its tokens, and
// persists the rotated tokens.
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
	c, auth, err := m.NewClientWithRefresh(ctx, st.UID, st.RefreshToken)
	if err != nil {
		m.Close()
		return nil, &exitcode.Error{
			Code: exitcode.MustAct,
			Err:  fmt.Errorf("the saved session cannot be refreshed: %w", apiMessage(err)),
			Hint: "pcb-lab login",
		}
	}
	if o.Recorder != nil {
		o.Recorder.Forbid(auth.AccessToken)
		o.Recorder.Forbid(auth.RefreshToken)
	}
	if sc := scopes(auth.Scope); len(sc) > 0 {
		st.Scopes = sc
	}
	saveRotation(o.StateDir, &st, auth)
	c.AddAuthHandler(func(a proton.Auth) { saveRotation(o.StateDir, &st, a) })
	return &Session{UID: st.UID, Scopes: st.Scopes, Client: c, manager: m}, nil
}

var saveMu sync.Mutex

// saveRotation persists tokens rotated by Proton. A failed write is
// ignored: the next run falls back to a login.
func saveRotation(dir string, st *stored, a proton.Auth) {
	saveMu.Lock()
	defer saveMu.Unlock()
	if a.AccessToken != "" {
		st.AccessToken = a.AccessToken
	}
	if a.RefreshToken != "" {
		st.RefreshToken = a.RefreshToken
	}
	_ = save(dir, *st)
}

func save(dir string, st stored) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, sessionFile), b)
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
	return os.MkdirAll(dir, 0o700)
}

// writeFile writes atomically with mode 0600.
func writeFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // gone after a successful rename
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// RedactID shortens an identifier for display: the first four characters
// and the length, never the whole value.
func RedactID(id string) string {
	if len(id) <= 4 {
		return "…"
	}
	return fmt.Sprintf("%s… (%d chars)", id[:4], len(id))
}
