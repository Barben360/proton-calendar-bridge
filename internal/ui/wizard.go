package ui

import (
	"context"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/loginflow"
)

// Step is the screen the add-account wizard is on (loginflow's steps).
type Step = loginflow.Step

// The wizard's steps.
const (
	StepCredentials = loginflow.StepCredentials
	StepTOTP        = loginflow.StepTOTP
	StepCaptcha     = loginflow.StepCaptcha
	StepDone        = loginflow.StepDone
)

// UnattendedWarning is the line pcb doctor prints when unattended relogin is
// on (one format, shared with internal/doctor): the wizard shows it next to
// the unattended-relogin checkbox.
var UnattendedWarning = doctor.UnattendedKeyringWarning("secrets.sealed")

// doneTimeout bounds what Done reads from the Bridge.
const doneTimeout = 10 * time.Second

// Wizard is the add-account flow of the window: the shared
// loginflow.Flow (its steps, Submit, SubmitTOTP, CaptchaSolved, Cancel)
// over the Bridge's Hub, plus Done, which reads the new account's
// connection details from the Bridge. While the bridge is stopped every
// call is refused with the not-running error and the flow keeps its step.
type Wizard struct {
	*loginflow.Flow
	b *Bridge
}

// NewWizard returns a wizard on its first step.
func NewWizard(b *Bridge) *Wizard {
	return &Wizard{Flow: loginflow.New(b.loginHub), b: b}
}

// loginHub is the running daemon's Hub for the flow, or the not-running
// error (never a typed nil).
func (b *Bridge) loginHub() (loginflow.Hub, error) {
	h, err := actions{b}.hub()
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Done is the account just added and its CalDAV password (the default
// client's, created when missing), on StepDone; zero values otherwise. The
// password is read from the Hub at each call, never kept.
func (w *Wizard) Done() (AccountView, string) {
	if w.Step() != StepDone {
		return AccountView{}, ""
	}
	id := w.Account()
	ctx, cancel := context.WithTimeout(context.Background(), doneTimeout)
	defer cancel()
	var view AccountView
	for _, a := range w.b.View(ctx, time.Now()).Accounts {
		if a.ID == id {
			view = a
		}
	}
	pw, err := NewActions(w.b).Password(ctx, id)
	if err != nil {
		w.Report(err)
		return view, ""
	}
	return view, pw
}
