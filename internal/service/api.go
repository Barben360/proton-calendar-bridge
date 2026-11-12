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
	// SubmitTOTP finishes a login that answered needs-totp, within 5
	// minutes of it: then the login is revoked and forgotten.
	SubmitTOTP(ctx context.Context, code string) (LoginResult, error)
	// Logout revokes the session and forgets the account. During Serve it
	// first stops the sync loop and closes its cache handle (ruling M5-P10).
	// A stored session this pcb cannot read (Status.Secrets "unsupported")
	// cannot be revoked: it is forgotten only with req.Discard.
	Logout(ctx context.Context, req LogoutRequest) (LogoutResult, error)
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
	// Serve runs serve mode's sync loop until ctx ends (nil) or a person is
	// needed (that error): the sync lock held throughout, and the read
	// methods below answered from its own cache handle (ruling M3-R6). It
	// needs a ready login; a start it refuses never calls o.Ready. With
	// o.WaitForLogin it is ready once it holds the sync lock and waits for
	// a login instead (logged out, the session ended, a Logout meanwhile):
	// the reads and writes then answer ErrWaitingForLogin.
	Serve(ctx context.Context, o ServeOptions) error
	// Health is Serve's live state: the cadence, the last pass, each
	// calendar's sync, error class and write gate with its reason, the
	// render counters and the human verifications Proton asked for. It
	// calls no network. Outside Serve it is {Serve: "stopped"}; while Serve
	// waits for a login, {Serve: "waiting-for-login"}.
	Health(ctx context.Context) (Health, error)
	// Events lists a calendar's cached events. It calls Proton only when the
	// calendar is not in the cache, to tell "not synced yet" from "no such calendar".
	Events(ctx context.Context, req EventsRequest) ([]Event, error)
	// The read methods below read the event cache: during Serve, Serve's
	// own handle; otherwise a read-only handle opened for each call. They
	// call Proton only to unlock a calendar's keys and read its settings,
	// the first time the process reads that calendar. While Serve waits for
	// a login they answer ErrWaitingForLogin.

	// Collections lists the calendars of the event cache with their CTag
	// and sync token, and those not synced yet as Pending; no cache is a
	// MustAct. It may call Proton (the
	// CTag holds each calendar's settings, ruling M3-R14).
	Collections(ctx context.Context) ([]Collection, error)
	// Objects lists a calendar's objects (one per UID) with their ETags and
	// the names they are served at, and the calendar's current sync token.
	Objects(ctx context.Context, calID string) ([]ObjectInfo, string, error)
	// ObjectsByUID returns the rendered objects of uids, in their order.
	ObjectsByUID(ctx context.Context, calID string, uids []string) ([]Object, error)
	// ObjectsByName returns the rendered objects served at names (resource
	// names within the calendar), in their order: Missing for a name that
	// serves no live object. Each UID is served at one name, its alias or
	// its canonical name (ObjectName), or its alternative name when another
	// UID's alias holds its canonical name (rulings M4-R6, M4-R38); any
	// other name of it serves nothing.
	ObjectsByName(ctx context.Context, calID string, names []string) ([]Object, error)
	// Changes returns what changed in a calendar since a sync token ("" for
	// everything); a token it cannot answer from is an error whose cause is
	// ErrInvalidSyncToken.
	Changes(ctx context.Context, calID, token string) (ChangeSet, error)
	// The write methods below run only during Serve, on its cache handle
	// (otherwise ErrNotServing). A write refreshes the object's rows from
	// Proton, evaluates cond on that fresh state, sends one Sync call
	// (never sent again) and reads the rows back into the cache before it
	// answers: success only when Proton holds what was sent. Errors:
	// ErrPreconditionFailed (412), *UIDConflictError (409),
	// ErrNotWritable (403 need-privileges: the gate decided read only;
	// undecided is ErrGateUndecided, unavailable, ruling M4-R39), *WriteRefusal (403 with its
	// pcb precondition), ErrNoSuchObject and ErrNoSuchCalendar (404);
	// ErrWriteNotConfirmed, ErrCalendarPending, ErrWaitingForLogin, ErrNotServing and every
	// other error are unavailable (503, ProtonRetryAfter says how long
	// Proton asked to wait).

	// PutObject creates or replaces the object at name (a resource name
	// within the calendar: its alias or its UID's canonical name, see
	// ObjectName) with body, one VCALENDAR. A create at a name that is not
	// the canonical name of the body's UID records the name as its alias.
	PutObject(ctx context.Context, calID, name string, body []byte, cond Precondition) (PutResult, error)
	// DeleteObject deletes the object at name: every row of its UID.
	DeleteObject(ctx context.Context, calID, name string, cond Precondition) error
	// The client methods manage the CalDAV app passwords in
	// <state>/clients.json. They need no login and no network, and logout
	// leaves them (ruling M3-R17). A login that completes (Login,
	// SubmitTOTP) with no client in the list adds one named default.

	// AddClient creates a client and returns its password, this once. A bad
	// or taken name is a Usage error.
	AddClient(ctx context.Context, name string) (ClientSecret, error)
	// Clients lists the clients, without secrets.
	Clients(ctx context.Context) ([]Client, error)
	// RemoveClient deletes the client with this ID or (case-insensitive)
	// name; an unknown one is a Usage error.
	RemoveClient(ctx context.Context, idOrName string) (Client, error)
	// ShowClient returns a client's credentials, its password opened from
	// its sealed copy (decision 0013); a client made before that has none:
	// Recoverable is false, with no error. A sealed copy the key cannot
	// open is a MustAct naming the key file. An empty idOrName means the
	// client named default, else the only client; with several and no
	// default it is a MustAct. URL and Fingerprint are left empty: the
	// caller that knows the listener fills them.
	ShowClient(ctx context.Context, idOrName string) (ClientCredentials, error)
	// RotateClient gives a client (chosen as ShowClient does) a new
	// password and returns it; the old one stops passing CheckClient at once.
	RotateClient(ctx context.Context, idOrName string) (ClientSecret, error)
	// CheckClient reports whether password is a client's, for a request
	// from remote ("ip:port"). A remote with 5 failures in the last minute
	// is refused unexamined and retryAfter says for how long (CalDAV: 503 with Retry-After);
	// otherwise a refusal has retryAfter 0 (401). While clients.json cannot be
	// loaded every request is refused with a retryAfter (503) and no failure
	// is charged to the remote.
	CheckClient(ctx context.Context, password, remote string) (ok bool, retryAfter time.Duration)
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
// CaptchaToken are set with needs-captcha; LoginExpires with needs-totp:
// when the login waiting for its code ends (Login's time + 5 minutes).
type LoginResult struct {
	State        LoginState `json:"state"`
	Username     string     `json:"username,omitempty"`
	AccountID    string     `json:"account_id,omitempty"`
	CaptchaURL   string     `json:"captcha_url,omitempty"`
	CaptchaToken string     `json:"captcha_token,omitempty"`
	LoginExpires *time.Time `json:"login_expires,omitempty"`
	// DefaultClientCreated: this login created the CalDAV client named
	// default, the list being empty (decision 0013; pcb client show).
	DefaultClientCreated bool `json:"default_client_created,omitempty"`
}

// LogoutRequest asks for a logout. Discard forgets a stored session this
// pcb cannot read (written by a newer pcb, or sealed by another sealer)
// although it cannot be revoked; without it such a logout is refused.
type LogoutRequest struct {
	Discard bool `json:"discard,omitempty"`
}

// SecretsUnsupported is Status.Secrets for a store this pcb cannot read:
// written by a newer pcb, or sealed by another sealer.
const SecretsUnsupported = "unsupported"

// LogoutResult says what Logout did. Revoked is false when Proton could not
// be told (the local secrets are gone anyway).
// SyncRunning: another pcb process held the sync lock (pcb sync, its
// watch, or pcb serve); a pass of it may write the event cache again.
type LogoutResult struct {
	WasLoggedIn bool `json:"was_logged_in"`
	Revoked     bool `json:"revoked"`
	SyncRunning bool `json:"sync_running,omitempty"`
}

// Status is the login as the files describe it. Phase is the store's phase
// (logged-out, ready, needs-captcha, needs-login) or needs-totp while a login
// waits for its code. Secrets is "sealed", "none", "unreadable",
// "unsupported" (written by a newer pcb, or sealed by another sealer: Reason
// names which) or "rolled-back" (secrets.sealed older than the last one pcb
// wrote).
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
	// LoginExpires is set with needs-totp: when the login waiting for its
	// code ends.
	LoginExpires *time.Time `json:"login_expires,omitempty"`
	// Sync is the event cache's state; nil while there is no cache.
	Sync *SyncStatus `json:"sync,omitempty"`
	// ClientsFile is the path of clients.json when it exists but cannot be
	// loaded (corrupt, unreadable): every CalDAV request then answers 503.
	ClientsFile string `json:"clients_file,omitempty"`
}

// SyncStatus is what the event cache holds, per calendar.
type SyncStatus struct {
	Calendars []CalendarStatus `json:"calendars"`
	// Error is set when a cache exists but cannot be read (secret-free).
	Error string `json:"error,omitempty"`
}

// CalendarStatus is one cached calendar. SyncedAt is its last successful
// pass and FullAt its last full listing (nil: never). Error is its last
// sync failure ("" if none).
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
	// Render is the process's render counters after the pass; set by
	// Serve only (ruling M3-R34: pcb serve logs them).
	Render *RenderCounts
}

// RenderCounts is what the renders of this process met, in memory only, as
// of now (ruling M3-R34): only the process that serves can see it, so
// pcb serve logs it and pcb status does not show it (M5's local socket
// will). Since the keys were last loaded: Undecryptable counts the rows
// served as placeholders (ruling M3-R4), Unverified the rows rendered with
// X-PCB-INTEGRITY:unverified (ruling M3-R33), DuplicateMasters the masters
// left out (ruling M3-R3; a second exception of one RECURRENCE-ID counts
// with them), DroppedAlarms the malformed reminders left out of events that
// still render; KeysErrors are the IDs of the calendars whose keys
// did not unlock (every event of them is a placeholder), sorted, nil if
// none.
type RenderCounts struct {
	Undecryptable    int      `json:"undecryptable"`
	Unverified       int      `json:"unverified"`
	DuplicateMasters int      `json:"duplicate_masters"`
	DroppedAlarms    int      `json:"dropped_alarms"`
	KeysErrors       []string `json:"keys_errors"`
}

// Zero reports whether every counter is zero.
func (c RenderCounts) Zero() bool {
	return c.Undecryptable == 0 && c.Unverified == 0 && c.DuplicateMasters == 0 && c.DroppedAlarms == 0 && len(c.KeysErrors) == 0
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
	// ErrorClass sorts Error for Health (errorClass): rate-limited,
	// proton-unavailable, network, session, refused, deferred, cache or other.
	ErrorClass string `json:"error_class,omitempty"`
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
	// ReadOnly follows the write gate (ruling M4-R1): false only for a normal
	// calendar whose keys unlocked, our member has the Write bit and the
	// member passphrase signature verified.
	ReadOnly  bool   `json:"read_only"`
	Default   bool   `json:"default"`
	Keys      string `json:"keys"`
	KeysError string `json:"keys_error,omitempty"`
}

// Collection is a synced calendar as the read API serves it. CTag is
// "<epoch>:<seq>:<settings hash>": it changes with any row of the calendar,
// the calendar settings and keys the rendering uses, and the cache's life.
// SyncToken is "urn:pcb:sync:1:<epoch>:<seq>:<gen>" (gen names the render
// inputs, ruling M3-R11).
type Collection struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
	// ReadOnly follows the write gate (ruling M4-R1), as Calendar.ReadOnly;
	// true as well when the keys are not loaded.
	ReadOnly bool `json:"read_only"`
	// Pending: the cache knows the calendar but has not synced it yet (a
	// first pass is on its way to it, or it failed): no CTag, no sync token,
	// and its reads return ErrCalendarPending (ruling M3-R25).
	Pending bool `json:"pending,omitempty"`
	// PrivilegesUnknown: the gate was never decided for this calendar
	// (ruling M4-R40): no privileges are advertised (ReadOnly is true for
	// callers that need a value). Thunderbird keeps a read-only flag for
	// good, so one failed bootstrap must not set it.
	PrivilegesUnknown bool `json:"privileges_unknown,omitempty"`
	// WriteGateUndecided: the gate cannot be decided now (ruling M4-R39:
	// the keys did not unlock, the passphrase signature cannot be checked):
	// ReadOnly is the last decided value, and a write answers unavailable
	// (503), not ErrNotWritable.
	WriteGateUndecided bool   `json:"write_gate_undecided,omitempty"`
	CTag               string `json:"ctag"`
	SyncToken          string `json:"sync_token"`
}

// ObjectInfo names one object: the UID of its rows, the resource name it is
// served at within its calendar (ObjectsByName says which) and its ETag
// (quoted, as HTTP sends it; "" for a deletion).
type ObjectInfo struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
	ETag string `json:"etag,omitempty"`
}

// Object is a rendered object: a VCALENDAR (CRLF line ends) holding the
// UID's master and exceptions, decrypted, and the name it is served at.
// Missing: the UID has no live row (ObjectsByName: or the name serves no
// UID, then UID is ""). Data is shared with the service's in-memory cache:
// never modify it.
type Object struct {
	UID     string `json:"uid"`
	Name    string `json:"name"`
	ETag    string `json:"etag,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// ChangeSet is what changed since a sync token: the objects to fetch, the
// UIDs with no live row left (at the name each was served at, ETag ""),
// and the token to ask from next.
type ChangeSet struct {
	Changed []ObjectInfo `json:"changed"`
	Deleted []ObjectInfo `json:"deleted"`
	Token   string       `json:"token"`
}
