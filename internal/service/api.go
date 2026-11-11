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
	// Sync brings the event cache up to date with Proton. A calendar that
	// failed is in the result, and the error is a Partial *exitcode.Error.
	Sync(ctx context.Context, req SyncRequest) (SyncResult, error)
	// WatchSync repeats Sync every interval (0: DefaultWatchInterval) until
	// ctx ends (nil: a normal stop) or a person is needed (that error), calling
	// onPass after every pass.
	WatchSync(ctx context.Context, req SyncRequest, interval time.Duration, onPass func(WatchPass)) error
	// Events lists a calendar's cached events. It calls Proton only when the
	// calendar is not in the cache, to tell "not synced yet" from "no such calendar".
	Events(ctx context.Context, req EventsRequest) ([]Event, error)
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
	// Sync is the event cache's state; nil while there is no cache.
	Sync *SyncStatus `json:"sync,omitempty"`
}

// SyncStatus is what the event cache holds, per calendar.
type SyncStatus struct {
	Calendars []CalendarStatus `json:"calendars"`
	// Error is set when a cache exists but cannot be read (secret-free).
	Error string `json:"error,omitempty"`
}

// CalendarStatus is one cached calendar. SyncedAt is its last successful
// pass and FullAt its last full listing (nil: never). Error is its last
// failure ("" if none).
type CalendarStatus struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Events   int        `json:"events"`
	SyncedAt *time.Time `json:"synced_at,omitempty"`
	FullAt   *time.Time `json:"full_at,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// SyncRequest asks for a sync pass; Full lists every calendar in full.
type SyncRequest struct {
	Full bool `json:"full,omitempty"`
}

// SyncResult is what a pass did, one entry per calendar.
type SyncResult struct {
	Calendars []CalendarSync `json:"calendars"`
}

// WatchPass is one pass of WatchSync: its result and error (as Sync
// returns them, except that a hint only a one-shot sync needs, "retry in
// X", is dropped), and the wait before the next pass, 0 when the watch
// stops on Err (WatchSync then returns it).
type WatchPass struct {
	Result SyncResult
	Err    error
	Next   time.Duration
}

// CalendarSync is one calendar's outcome. Mode is initial, incremental or
// full; Events is the calendar's row count after the pass; Error is the
// secret-free reason the calendar failed, "" if it synced.
type CalendarSync struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Mode    string `json:"mode"`
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
	Deleted int    `json:"deleted"`
	Events  int    `json:"events"`
	Error   string `json:"error,omitempty"`
}

// EventsRequest selects cached events: Calendar is an ID or an exact name;
// From and To keep the events overlapping [From, To), and the recurring
// masters starting before To whatever From says. Timed events and all-day
// events with no date bound are compared as instants. FromDate and ToDate
// (YYYY-MM-DD) are the same bounds as dates: when set they replace From and
// To for all-day events, which Proton stores as UTC midnights of their
// dates, so a bound written as a date in a zone west of UTC does not shift
// an all-day event by a day.
type EventsRequest struct {
	Calendar string     `json:"calendar"`
	From     *time.Time `json:"from,omitempty"`
	To       *time.Time `json:"to,omitempty"`
	FromDate string     `json:"from_date,omitempty"`
	ToDate   string     `json:"to_date,omitempty"`
}

// Event is a cached event row (its content stays encrypted in the cache).
type Event struct {
	ID         string    `json:"id"`
	UID        string    `json:"uid"`
	CalendarID string    `json:"calendar_id"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	AllDay     bool      `json:"all_day"`
	// StartDate and EndDate (YYYY-MM-DD, from the UTC instants) are set for
	// an all-day event; EndDate is exclusive: the day after its last day.
	StartDate    string     `json:"start_date,omitempty"`
	EndDate      string     `json:"end_date,omitempty"`
	Recurring    bool       `json:"recurring"`
	RecurrenceID *time.Time `json:"recurrence_id,omitempty"`
	LastEdit     time.Time  `json:"last_edit"`
	ETag         string     `json:"etag"`
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
