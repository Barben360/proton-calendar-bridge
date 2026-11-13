// Package loginflow is the add-account flow as a state machine with no
// widget and no listener: credentials, then the 2FA code or a CAPTCHA, then
// done. pcb-ui's wizard (internal/ui) and the MCP login page
// (internal/mcp/loginpage) both drive it. It reaches the accounts through
// Hub only, and imports neither of its users, nor the daemon or the CLI
// (ruling R18: ui → daemon → mcp must not lead back to ui).
package loginflow

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Hub is what the flow calls on the accounts (service.Hub has them all):
// Accounts (which accounts existed before, and whether a login still waits
// for its 2FA code), Login (credentials, and the CAPTCHA retry), SubmitTOTP,
// and, when the flow is cancelled mid-way, Remove (an account this flow
// added), AbandonLogin (one that was ready before) or SignOut (another one
// that existed before).
type Hub interface {
	Accounts() []service.AccountInfo
	Login(ctx context.Context, req service.LoginRequest) (service.LoginResult, error)
	SubmitTOTP(ctx context.Context, selector, code string) (service.LoginResult, error)
	SignOut(ctx context.Context, selector string, req service.LogoutRequest) (service.LogoutResult, error)
	Remove(ctx context.Context, selector string, req service.LogoutRequest) (service.AccountInfo, error)
	AbandonLogin(ctx context.Context, selector string) error
}

var _ Hub = (*service.Hub)(nil)

// Step is the screen the flow is on.
type Step string

// The flow's steps.
const (
	StepCredentials Step = "credentials"
	StepTOTP        Step = "totp"
	StepCaptcha     Step = "captcha"
	StepDone        Step = "done"
)

// UnattendedWarning is the line shown next to the unattended-relogin box
// (pcb-ui's wizard, the MCP login page): pcb doctor's for a state sealed
// with the desktop keyring (doctor.UnattendedKeyringWarning, which a test
// of internal/ui compares it with).
const UnattendedWarning = "unattended relogin is on: whoever can use this desktop session's keyring and read secrets.sealed owns the Proton account"

// cancelTimeout bounds what Cancel does with the Hub.
const cancelTimeout = 10 * time.Second

// Flow is one add-account flow. Its user draws one screen per Step and
// calls the method of the step it is on. It is safe for concurrent use;
// calls that reach the Hub are serialised.
//
// The password lives in the Flow from Submit until the login ends (ready,
// an error, or the flow is cancelled): a CAPTCHA keeps it for the retry. It
// is zeroed in place then. This is best effort: service.LoginRequest takes a
// string, a copy Go cannot zero (as the seal package documents).
type Flow struct {
	hub func() (Hub, error)
	op  sync.Mutex // one Hub-reaching call at a time

	mu         sync.Mutex // guards the fields below
	step       Step
	err        *exitcode.Error
	captchaURL string
	pw         []byte // the password, until the login ends
	username   string
	unattended bool
	captchaTok string // the token of the CAPTCHA challenge, for the retry
	account    string // the local ID of the account being added
	created    bool   // Submit made that account (it did not exist)
	wasReady   bool   // that account was ready before Submit: Cancel leaves it be
}

// New returns a flow on its first step. hub gives the accounts at each
// call: an error (the desktop bridge is stopped) refuses the call with it.
func New(hub func() (Hub, error)) *Flow { return &Flow{hub: hub, step: StepCredentials} }

// Step is the current screen.
func (f *Flow) Step() Step {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.step
}

// Err is why the last call failed (what pcb login shows for the same
// failure), nil after a call that succeeded.
func (f *Flow) Err() *exitcode.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// CaptchaURL is the verification page to open on StepCaptcha.
func (f *Flow) CaptchaURL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.captchaURL
}

// Account is the local ID of the account the flow logs in, once the Hub
// named it ("" before).
func (f *Flow) Account() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.account
}

// Report records err as the flow's error and changes nothing else (a
// screen that failed to read what it shows next).
func (f *Flow) Report(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = asExitError(err)
}

// PasswordBuf is the flow's password buffer itself (not a copy), so a test
// can look at it after the flow zeroed it.
func (f *Flow) PasswordBuf() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pw
}

// wipe zeroes the password in place and forgets it. Callers hold f.mu.
func (f *Flow) wipe() {
	clear(f.pw)
	f.pw = nil
}

// fail records err, goes back to the credentials, and wipes the password.
func (f *Flow) fail(err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failLocked(err)
}

// refuse records err and changes nothing else: the Hub is not there, and
// the step, the account and the password stay for a retry or a Cancel
// once it is back.
func (f *Flow) refuse(err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = asExitError(err)
	return f.err
}

func (f *Flow) failLocked(err error) error {
	f.wipe()
	f.step, f.captchaURL, f.captchaTok, f.account, f.created, f.wasReady = StepCredentials, "", "", "", false, false
	f.err = asExitError(err)
	return f.err
}

// asExitError gives err its exit code, as pcb login does for what it prints
// (a plain error is exit 20).
func asExitError(err error) *exitcode.Error {
	var ee *exitcode.Error
	if errors.As(err, &ee) {
		return ee
	}
	return &exitcode.Error{Code: exitcode.From(err), Err: err}
}

func wrongStep(want Step, got Step) error {
	return &exitcode.Error{Code: exitcode.Usage, Err: errors.New("the wizard is on the " + string(got) + " step, not the " + string(want) + " one")}
}

// Submit logs in (or adds) the account address, with the password (the
// flow keeps its own copy: wipe the argument when it returns) and the
// unattended-relogin choice. It goes to StepDone, StepTOTP or StepCaptcha, or
// stays on the credentials with Err set.
func (f *Flow) Submit(ctx context.Context, address string, password []byte, unattended bool) error {
	f.op.Lock()
	defer f.op.Unlock()
	h, err := f.hub()
	if err != nil {
		return f.fail(err)
	}
	f.mu.Lock()
	if f.step != StepCredentials {
		got := f.step
		f.mu.Unlock()
		return wrongStep(StepCredentials, got)
	}
	f.wipe()
	f.pw = slices.Clone(password)
	f.username, f.unattended = address, unattended
	f.err = nil
	f.mu.Unlock()

	known := map[string]string{} // local ID → phase
	for _, a := range h.Accounts() {
		known[a.ID] = a.Phase
	}
	res, err := h.Login(ctx, service.LoginRequest{Username: address, Password: string(password), UnattendedRelogin: unattended})
	phase, existed := known[res.Account]
	if err == nil && res.Account != "" {
		f.mu.Lock()
		f.wasReady = existed && phase == string(service.StateReady)
		f.mu.Unlock()
	}
	return f.settle(res.Account != "" && !existed, res, err)
}

// CaptchaSolved logs in again with the CAPTCHA token the challenge returned
// and the password the flow kept (as pcb login does once the person has
// solved it on the verification page).
func (f *Flow) CaptchaSolved(ctx context.Context) error {
	f.op.Lock()
	defer f.op.Unlock()
	h, err := f.hub()
	if err != nil {
		return f.refuse(err)
	}
	f.mu.Lock()
	if f.step != StepCaptcha {
		got := f.step
		f.mu.Unlock()
		return wrongStep(StepCaptcha, got)
	}
	req := service.LoginRequest{Username: f.username, Password: string(f.pw), UnattendedRelogin: f.unattended, CaptchaToken: f.captchaTok}
	created := f.created
	f.err = nil
	f.mu.Unlock()
	res, err := h.Login(ctx, req)
	return f.settle(created, res, err)
}

// settle is the end of a login: created says whether it added the account.
func (f *Flow) settle(created bool, res service.LoginResult, err error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		return f.failLocked(err)
	}
	if res.Account != "" {
		f.account = res.Account
	}
	f.created = created
	switch res.State {
	case service.StateReady:
		f.wipe()
		f.step, f.captchaURL, f.captchaTok = StepDone, "", ""
	case service.StateNeedsTOTP:
		// The Hub holds the login: the password is not needed again.
		f.wipe()
		f.step, f.captchaURL, f.captchaTok = StepTOTP, "", ""
	case service.StateNeedsCaptcha:
		// The retry needs the password: it stays until that login ends.
		f.step, f.captchaURL, f.captchaTok = StepCaptcha, res.CaptchaURL, res.CaptchaToken
	default:
		return f.failLocked(errors.New("the login ended in an unknown state: " + string(res.State)))
	}
	return nil
}

// SubmitTOTP sends the 2FA code. A wrong code keeps StepTOTP while the Hub
// still holds the login (it allows a few tries), else the flow is back on
// the credentials.
func (f *Flow) SubmitTOTP(ctx context.Context, code string) error {
	f.op.Lock()
	defer f.op.Unlock()
	h, err := f.hub()
	if err != nil {
		return f.refuse(err)
	}
	f.mu.Lock()
	if f.step != StepTOTP {
		got := f.step
		f.mu.Unlock()
		return wrongStep(StepTOTP, got)
	}
	id, created := f.account, f.created
	f.err = nil
	f.mu.Unlock()

	res, err := h.SubmitTOTP(ctx, id, code)
	if err != nil && heldForCode(h, id) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.err = asExitError(err)
		return f.err
	}
	return f.settle(created, res, err)
}

// heldForCode reports whether the Hub still waits for the 2FA code of id.
func heldForCode(h Hub, id string) bool {
	for _, a := range h.Accounts() {
		if a.ID == id {
			return a.Phase == string(service.StateNeedsTOTP)
		}
	}
	return false
}

// Cancel abandons the flow: the password is zeroed, and an account this
// flow added that never finished its first login is removed (a pending 2FA
// or CAPTCHA add, with its directory); an account that existed before is
// only signed out, which drops the login the Hub holds for it, unless it
// was ready before Submit: its stored session still works, so only the
// login the Hub holds for it is dropped (AbandonLogin). It waits for
// a call in flight (bounded by that call's ctx), so a UI must call it off the
// UI thread.
func (f *Flow) Cancel() {
	f.op.Lock()
	defer f.op.Unlock()
	f.mu.Lock()
	step, id, created, wasReady := f.step, f.account, f.created, f.wasReady
	f.wipe()
	f.step, f.err, f.captchaURL, f.captchaTok, f.account, f.created, f.wasReady = StepCredentials, nil, "", "", "", false, false
	f.mu.Unlock()
	if id == "" || (step != StepTOTP && step != StepCaptcha) {
		return
	}
	h, err := f.hub()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	switch {
	case wasReady:
		_ = h.AbandonLogin(ctx, id)
	case created:
		_, _ = h.Remove(ctx, id, service.LogoutRequest{})
	default:
		_, _ = h.SignOut(ctx, id, service.LogoutRequest{})
	}
}
