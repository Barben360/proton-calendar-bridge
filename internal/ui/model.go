package ui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// TrayState is the tray icon, and the colour of an account's dot.
type TrayState string

// The tray states.
const (
	TrayOK        TrayState = "ok"
	TraySyncing   TrayState = "syncing"
	TrayAttention TrayState = "attention"
)

// View is what the tray and the window show, built from the bridge's status.
type View struct {
	Tray TrayState
	// Bridge is empty while the bridge runs, and "bridge stopped: <why>"
	// once the daemon ended by itself.
	Bridge   string
	Accounts []AccountView
	Listener *ctl.Listener
	Alerts   []ctl.Alert
}

// AccountView is one account of the window. Short is its one-line state.
// Username is what a CalDAV client signs in with (the account's address).
type AccountView struct {
	ID, Address, Short         string
	Dot                        TrayState
	SignedIn, Pending, CalDAV  bool
	LastSync                   *time.Time
	Calendars                  []CalendarView
	Alerts                     []ctl.Alert
	URL, Username, Fingerprint string
}

// CalendarView is one calendar of an account. Badge is the write gate's
// reason while the gate is not open.
type CalendarView struct {
	Name                   string
	ReadOnly               bool
	Badge, LastSync, Error string
}

// fallbackInterval is the cadence assumed for a pass whose loop did not say
// how long it waits (the idle one: the longest).
const fallbackInterval = service.IdleInterval

// Build turns the status into a View at now.
func Build(s ctl.Status, now time.Time) View {
	v := View{Tray: TrayOK, Listener: s.Listener, Alerts: s.Alerts, Accounts: []AccountView{}}
	attention := hasError(s.Alerts)
	syncing := false
	for _, as := range s.Accounts {
		a := accountView(as, s.Listener, now)
		attention = attention || a.Dot == TrayAttention
		syncing = syncing || a.Dot == TraySyncing
		v.Accounts = append(v.Accounts, a)
	}
	switch {
	case attention:
		v.Tray = TrayAttention
	case syncing:
		v.Tray = TraySyncing
	}
	return v
}

func hasError(alerts []ctl.Alert) bool {
	for _, a := range alerts {
		if a.Severity == ctl.SeverityError {
			return true
		}
	}
	return false
}

func accountView(as ctl.AccountStatus, ln *ctl.Listener, now time.Time) AccountView {
	a := AccountView{
		ID: as.ID, Address: as.Address, Username: as.Address, URL: as.URL,
		SignedIn: as.Phase == "ready", Pending: as.Pending, CalDAV: as.CalDAV,
		Alerts: as.Alerts, Calendars: []CalendarView{},
	}
	if a.Address == "" {
		a.Address, a.Username = as.Login.Address, as.Login.Address
	}
	if ln != nil && ln.TLS {
		a.Fingerprint = ln.Fingerprint
	}
	stale := false
	if h := as.Health; h != nil {
		interval := fallbackInterval
		if h.IntervalSeconds > 0 {
			interval = time.Duration(h.IntervalSeconds) * time.Second
		}
		stale = h.LastPass == nil || now.Sub(h.LastPass.At) > interval
		a.LastSync = lastSync(h)
		for _, c := range h.Calendars {
			a.Calendars = append(a.Calendars, calendarView(c, now))
		}
	} else {
		stale = true
	}
	a.Dot = TrayOK
	switch {
	case hasError(as.Alerts):
		a.Dot = TrayAttention
	case a.SignedIn && stale:
		a.Dot = TraySyncing
	}
	a.Short = short(as, a, stale, now)
	return a
}

// lastSync is when the account last got through a pass: its last pass when
// it succeeded, else the newest successful sync of a calendar.
func lastSync(h *service.Health) *time.Time {
	if p := h.LastPass; p != nil && p.OK {
		at := p.At
		return &at
	}
	var best *time.Time
	for _, c := range h.Calendars {
		if c.SyncedAt != nil && (best == nil || c.SyncedAt.After(*best)) {
			at := *c.SyncedAt
			best = &at
		}
	}
	return best
}

func calendarView(c service.CalendarHealth, now time.Time) CalendarView {
	cv := CalendarView{Name: c.Name, ReadOnly: c.Gate != "open", Error: c.Error}
	if cv.ReadOnly {
		cv.Badge = c.GateReason
	}
	if c.SyncedAt != nil {
		cv.LastSync = "synced " + ago(now.Sub(*c.SyncedAt))
	}
	return cv
}

// ago is "just now", "<n> min ago" or "<n> h ago".
func ago(d time.Duration) string {
	switch m := int(d / time.Minute); {
	case m < 1:
		return "just now"
	case m < 60:
		return fmt.Sprintf("%d min ago", m)
	default:
		return fmt.Sprintf("%d h ago", m/60)
	}
}

// short is the account's one-line state, the most pressing first.
func short(as ctl.AccountStatus, a AccountView, stale bool, now time.Time) string {
	switch {
	case as.Stopped != nil:
		return "stopped: " + as.Stopped.Message
	case as.Login.Secrets == "locked":
		return "keyring locked"
	}
	switch as.Phase {
	case "needs-login", "logged-out":
		return "needs login"
	case "needs-totp":
		return "needs 2FA code"
	case "needs-captcha":
		return "needs CAPTCHA"
	}
	switch {
	case !a.CalDAV:
		return "CalDAV off"
	case stale || a.LastSync == nil:
		return "syncing…"
	}
	return "synced " + ago(now.Sub(*a.LastSync))
}

// View is the bridge's status as the window shows it. Once the daemon ended
// by itself it carries the reason, with the attention tray, and no accounts
// (the Hub is closed).
func (b *Bridge) View(ctx context.Context, now time.Time) View {
	if err := b.Err(); err != nil {
		return View{Tray: TrayAttention, Bridge: "bridge stopped: " + err.Error(), Accounts: []AccountView{}, Alerts: []ctl.Alert{}}
	}
	hub := b.Hub()
	if hub == nil || b.Info().Addr == "" { // not started, or stopped
		return Build(ctl.Status{}, now)
	}
	d := ctl.NewDaemon(kind, "", "", now)
	return Build(ctl.BuildStatus(ctx, hub, d, b.Info().CTL(), now), now)
}

// Actions are what the window and the tray ask of the bridge.
type Actions interface {
	SignOut(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	SetCalDAV(ctx context.Context, id string, on bool) error
	// Password is the account's CalDAV password (the client default,
	// created when there is none).
	Password(ctx context.Context, id string) (string, error)
	// NewPassword replaces it: the old one stops working at once.
	NewPassword(ctx context.Context, id string) (string, error)
	Restart(ctx context.Context) error
	// Doctor runs the checks pcb doctor runs on the machine.
	Doctor(ctx context.Context) ([]doctor.Result, error)
}

// defaultClient is the CalDAV client the desktop app shows the password of.
const defaultClient = "default"

type actions struct{ b *Bridge }

// NewActions acts on b.
func NewActions(b *Bridge) Actions { return actions{b} }

var errNotRunning = errors.New("the bridge is not running")

// hub is the running daemon's Hub, or the error an action returns while the
// daemon ended or was stopped (its Hub is closed).
func (a actions) hub() (*service.Hub, error) {
	h := a.b.Hub()
	if h == nil || a.b.Err() != nil || a.b.Info().Addr == "" {
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errNotRunning, Hint: "restart the bridge (Settings → Restart bridge), then try again"}
	}
	return h, nil
}

func (a actions) api(id string) (service.API, error) {
	h, err := a.hub()
	if err != nil {
		return nil, err
	}
	api, _, err := h.Account(id)
	return api, err
}

func (a actions) SignOut(ctx context.Context, id string) error {
	h, err := a.hub()
	if err != nil {
		return err
	}
	_, err = h.SignOut(ctx, id, service.LogoutRequest{})
	return err
}

func (a actions) Remove(ctx context.Context, id string) error {
	h, err := a.hub()
	if err != nil {
		return err
	}
	_, err = h.Remove(ctx, id, service.LogoutRequest{})
	return err
}

func (a actions) SetCalDAV(ctx context.Context, id string, on bool) error {
	h, err := a.hub()
	if err != nil {
		return err
	}
	_, err = h.SetCalDAV(ctx, id, on)
	return err
}

func (a actions) Password(ctx context.Context, id string) (string, error) {
	api, err := a.api(id)
	if err != nil {
		return "", err
	}
	list, err := api.Clients(ctx)
	if err != nil {
		return "", err
	}
	have := false
	for _, c := range list {
		have = have || strings.EqualFold(c.Name, defaultClient)
	}
	if !have {
		s, err := api.AddClient(ctx, defaultClient)
		return s.Password, err
	}
	cr, err := api.ShowClient(ctx, defaultClient)
	if err != nil {
		return "", err
	}
	if !cr.Recoverable || cr.Password == "" { // made before passwords were kept sealed
		return "", &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the CalDAV password cannot be shown: it was made before pcb kept a copy"),
			Hint: "choose New password for a new one"}
	}
	return cr.Password, nil
}

func (a actions) NewPassword(ctx context.Context, id string) (string, error) {
	api, err := a.api(id)
	if err != nil {
		return "", err
	}
	s, err := api.RotateClient(ctx, defaultClient)
	return s.Password, err
}

func (a actions) Restart(ctx context.Context) error { return a.b.Restart(ctx) }

func (a actions) Doctor(ctx context.Context) ([]doctor.Result, error) {
	env := a.b.o.Env
	host := install.OSHost()
	host.Env = env
	kr := a.b.o.Keyring
	if kr == nil {
		kr = doctor.SystemKeyring()
	}
	var live *doctor.Live
	if i := a.b.Info(); i.Addr != "" {
		live = &doctor.Live{Addr: i.Addr, URL: i.Base, TLS: i.TLS, Fingerprint: i.Fingerprint, NotAfter: i.CertNotAfter}
	}
	return doctor.Run(ctx, doctor.Checks(env, host, kr, live), nil), nil
}
