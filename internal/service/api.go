// Package service is C3: the one API the CLI (and, later, the socket
// adapters) call. Every request and result is a plain JSON struct, so the
// same API can cross a socket unchanged, and every error a method returns is
// an *exitcode.Error with a secret-free message and a hint.
package service

import (
	"context"
	"log/slog"
	"time"
)

// API is what the CLI calls. Local implements it in process.
type API interface {
	// Login runs the login state machine: ready, needs-totp or needs-captcha.
	Login(ctx context.Context, req LoginRequest) (LoginResult, error)
	// SubmitTOTP finishes a login that answered needs-totp.
	SubmitTOTP(ctx context.Context, code string) (LoginResult, error)
	// Logout revokes the session and forgets the account.
	Logout(ctx context.Context) (LogoutResult, error)
	// Status describes the login without any network call.
	Status(ctx context.Context) (Status, error)
	// Calendars lists the account's calendars.
	Calendars(ctx context.Context) ([]Calendar, error)
	// Close releases what the service holds, aborting a login waiting for a code.
	Close() error
}

// LoginRequest asks for a login. TOTP is a 6-8 digit code or an otpauth://
// URI; CaptchaToken is the token of a CAPTCHA the user solved;
// UnattendedRelogin stores the credentials for relogin without a person
// (it needs the TOTP as an otpauth:// URI when the account has 2FA).
type LoginRequest struct {
	Username          string `json:"username"`
	Password          string `json:"password"`
	TOTP              string `json:"totp,omitempty"`
	CaptchaToken      string `json:"captcha_token,omitempty"`
	UnattendedRelogin bool   `json:"unattended_relogin,omitempty"`
}

const redactedRequest = "service.LoginRequest(redacted)"

// String hides the credentials from fmt.
func (LoginRequest) String() string { return redactedRequest }

// GoString hides the credentials from %#v.
func (LoginRequest) GoString() string { return redactedRequest }

// LogValue hides the credentials from slog.
func (LoginRequest) LogValue() slog.Value { return slog.StringValue(redactedRequest) }

// LoginState is where a login stands.
type LoginState string

// The login states.
const (
	StateReady        LoginState = "ready"
	StateNeedsTOTP    LoginState = "needs-totp"
	StateNeedsCaptcha LoginState = "needs-captcha"
)

// LoginResult is the outcome of Login or SubmitTOTP. CaptchaURL and
// CaptchaToken are set with needs-captcha.
type LoginResult struct {
	State        LoginState `json:"state"`
	Username     string     `json:"username,omitempty"`
	AccountID    string     `json:"account_id,omitempty"`
	CaptchaURL   string     `json:"captcha_url,omitempty"`
	CaptchaToken string     `json:"captcha_token,omitempty"`
}

// LogoutResult says what Logout did. Revoked is false when Proton could not
// be told (the local secrets are gone anyway).
type LogoutResult struct {
	WasLoggedIn bool `json:"was_logged_in"`
	Revoked     bool `json:"revoked"`
}

// Status is the login as the files describe it. Phase is the store's phase
// (logged-out, ready, needs-captcha, needs-login) or needs-totp while a login
// waits for its code. Secrets is "sealed", "none" or "unreadable".
type Status struct {
	Phase       string     `json:"phase"`
	Reason      string     `json:"reason,omitempty"`
	Username    string     `json:"username,omitempty"`
	AccountID   string     `json:"account_id,omitempty"`
	Secrets     string     `json:"secrets"`
	KeyFile     string     `json:"key_file"`
	Unattended  bool       `json:"unattended"`
	CaptchaURL  string     `json:"captcha_url,omitempty"`
	LastLogin   *time.Time `json:"last_login,omitempty"`
	LastRelogin *time.Time `json:"last_relogin,omitempty"`
}

// Calendar is one calendar of the account. Keys is "ok" or "error".
type Calendar struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Color       string `json:"color,omitempty"`
	Type        string `json:"type"`
	ReadOnly    bool   `json:"read_only"`
	Default     bool   `json:"default"`
	Keys        string `json:"keys"`
	KeysError   string `json:"keys_error,omitempty"`
}
