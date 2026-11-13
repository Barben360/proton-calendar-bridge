package ui

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Step is the screen the add-account wizard is on.
type Step string

// The wizard's steps.
const (
	StepCredentials Step = "credentials"
	StepTOTP        Step = "totp"
	StepCaptcha     Step = "captcha"
	StepDone        Step = "done"
)

// UnattendedWarning is the line pcb doctor prints when unattended relogin is
// on (one format, shared with internal/doctor): the wizard shows it next to
// the unattended-relogin checkbox.
var UnattendedWarning = doctor.UnattendedKeyringWarning("secrets.sealed")

// cancelTimeout bounds what Cancel does with the Hub.
const cancelTimeout = 10 * time.Second

// Wizard is the add-account flow of the window: a state machine with no
// widget. The window draws one screen per Step and calls the method of the
// step it is on. It is safe for concurrent use; calls that reach the Hub are
// serialised.
//
// The password lives in the Wizard from Submit until the login ends (ready,
// an error, or the wizard is cancelled): a CAPTCHA keeps it for the retry. It
// is zeroed in place then. This is best effort: service.LoginRequest takes a
// string, a copy Go cannot zero (as the seal package documents).
type Wizard struct {
	b  *Bridge
	op sync.Mutex // one Hub-reaching call at a time

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
}

// NewWizard returns a wizard on its first step.
func NewWizard(b *Bridge) *Wizard { return &Wizard{b: b, step: StepCredentials} }

// Step is the current screen.
func (w *Wizard) Step() Step {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.step
}

// Err is why the last call failed (what pcb login shows for the same
// failure), nil after a call that succeeded.
func (w *Wizard) Err() *exitcode.Error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// CaptchaURL is the verification page to open on StepCaptcha.
func (w *Wizard) CaptchaURL() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.captchaURL
}

// wipe zeroes the password in place and forgets it. Callers hold w.mu.
func (w *Wizard) wipe() {
	clear(w.pw)
	w.pw = nil
}

// fail records err, goes back to the credentials, and wipes the password.
func (w *Wizard) fail(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failLocked(err)
}

// refuse records err and changes nothing else: the bridge is not running,
// and the step, the account and the password stay for a retry or a Cancel
// once it runs again.
func (w *Wizard) refuse(err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = asExitError(err)
	return w.err
}

func (w *Wizard) failLocked(err error) error {
	w.wipe()
	w.step, w.captchaURL, w.captchaTok, w.account, w.created = StepCredentials, "", "", "", false
	w.err = asExitError(err)
	return w.err
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
// wizard keeps its own copy: wipe the argument when it returns) and the
// unattended-relogin choice. It goes to StepDone, StepTOTP or StepCaptcha, or
// stays on the credentials with Err set.
func (w *Wizard) Submit(ctx context.Context, address string, password []byte, unattended bool) error {
	w.op.Lock()
	defer w.op.Unlock()
	h, err := w.hub()
	if err != nil {
		return w.fail(err)
	}
	w.mu.Lock()
	if w.step != StepCredentials {
		got := w.step
		w.mu.Unlock()
		return wrongStep(StepCredentials, got)
	}
	w.wipe()
	w.pw = slices.Clone(password)
	w.username, w.unattended = address, unattended
	w.err = nil
	w.mu.Unlock()

	known := map[string]bool{}
	for _, a := range h.Accounts() {
		known[a.ID] = true
	}
	res, err := h.Login(ctx, service.LoginRequest{Username: address, Password: string(password), UnattendedRelogin: unattended})
	return w.settleKnown(res.Account != "" && !known[res.Account], res, err)
}

// CaptchaSolved logs in again with the CAPTCHA token the challenge returned
// and the password the wizard kept (as pcb login does once the person has
// solved it on the verification page).
func (w *Wizard) CaptchaSolved(ctx context.Context) error {
	w.op.Lock()
	defer w.op.Unlock()
	h, err := w.hub()
	if err != nil {
		return w.refuse(err)
	}
	w.mu.Lock()
	if w.step != StepCaptcha {
		got := w.step
		w.mu.Unlock()
		return wrongStep(StepCaptcha, got)
	}
	req := service.LoginRequest{Username: w.username, Password: string(w.pw), UnattendedRelogin: w.unattended, CaptchaToken: w.captchaTok}
	created := w.created
	w.err = nil
	w.mu.Unlock()
	res, err := h.Login(ctx, req)
	return w.settleKnown(created, res, err)
}

func (w *Wizard) hub() (*service.Hub, error) { return actions{w.b}.hub() }

// settleKnown is the end of a login: created says whether it added the
// account.
func (w *Wizard) settleKnown(created bool, res service.LoginResult, err error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		return w.failLocked(err)
	}
	if res.Account != "" {
		w.account = res.Account
	}
	w.created = created
	switch res.State {
	case service.StateReady:
		w.wipe()
		w.step, w.captchaURL, w.captchaTok = StepDone, "", ""
	case service.StateNeedsTOTP:
		// The Hub holds the login: the password is not needed again.
		w.wipe()
		w.step, w.captchaURL, w.captchaTok = StepTOTP, "", ""
	case service.StateNeedsCaptcha:
		// The retry needs the password: it stays until that login ends.
		w.step, w.captchaURL, w.captchaTok = StepCaptcha, res.CaptchaURL, res.CaptchaToken
	default:
		return w.failLocked(errors.New("the login ended in an unknown state: " + string(res.State)))
	}
	return nil
}

// SubmitTOTP sends the 2FA code. A wrong code keeps StepTOTP while the Hub
// still holds the login (it allows a few tries), else the wizard is back on
// the credentials.
func (w *Wizard) SubmitTOTP(ctx context.Context, code string) error {
	w.op.Lock()
	defer w.op.Unlock()
	h, err := w.hub()
	if err != nil {
		return w.refuse(err)
	}
	w.mu.Lock()
	if w.step != StepTOTP {
		got := w.step
		w.mu.Unlock()
		return wrongStep(StepTOTP, got)
	}
	id, created := w.account, w.created
	w.err = nil
	w.mu.Unlock()

	res, err := h.SubmitTOTP(ctx, id, code)
	if err != nil && heldForCode(h, id) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.err = asExitError(err)
		return w.err
	}
	return w.settleKnown(created, res, err)
}

// heldForCode reports whether the Hub still waits for the 2FA code of id.
func heldForCode(h *service.Hub, id string) bool {
	for _, a := range h.Accounts() {
		if a.ID == id {
			return a.Phase == string(service.StateNeedsTOTP)
		}
	}
	return false
}

// Done is the account just added and its CalDAV password (the default
// client's, created when missing), on StepDone; zero values otherwise. The
// password is read from the Hub at each call, never kept.
func (w *Wizard) Done() (AccountView, string) {
	w.op.Lock()
	defer w.op.Unlock()
	w.mu.Lock()
	step, id := w.step, w.account
	w.mu.Unlock()
	if step != StepDone {
		return AccountView{}, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	var view AccountView
	for _, a := range w.b.View(ctx, time.Now()).Accounts {
		if a.ID == id {
			view = a
		}
	}
	pw, err := NewActions(w.b).Password(ctx, id)
	if err != nil {
		w.mu.Lock()
		w.err = asExitError(err)
		w.mu.Unlock()
		return view, ""
	}
	return view, pw
}

// Cancel abandons the wizard: the password is zeroed, and an account this
// wizard added that never finished its first login is removed (a pending 2FA
// or CAPTCHA add, with its directory); an account that existed before is
// only signed out, which drops the login the Hub holds for it. It waits for
// a call in flight (bounded by that call's ctx), so a UI must call it off the
// UI thread.
func (w *Wizard) Cancel() {
	w.op.Lock()
	defer w.op.Unlock()
	w.mu.Lock()
	step, id, created := w.step, w.account, w.created
	w.wipe()
	w.step, w.err, w.captchaURL, w.captchaTok, w.account, w.created = StepCredentials, nil, "", "", "", false
	w.mu.Unlock()
	if id == "" || (step != StepTOTP && step != StepCaptcha) {
		return
	}
	h, err := w.hub()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	if created {
		_, _ = h.Remove(ctx, id, service.LogoutRequest{})
		return
	}
	_, _ = h.SignOut(ctx, id, service.LogoutRequest{})
}
