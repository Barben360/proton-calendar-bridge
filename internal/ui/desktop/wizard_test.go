package desktop

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// fakeWizard steps credentials, TOTP, CAPTCHA, done. next is the step the
// next successful call lands on.
type fakeWizard struct {
	mu       sync.Mutex
	step     ui.Step
	err      *exitcode.Error
	calls    []string
	pw       string // what Submit received
	unatt    bool
	script   map[string]ui.Step // call name -> step after
	block    chan struct{}
	doneErr  *exitcode.Error
	canceled chan struct{}
}

func (f *fakeWizard) enter(call string) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if s, ok := f.script[call]; ok {
		f.step = s
	}
}

func (f *fakeWizard) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeWizard) Step() ui.Step {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.step == "" {
		return ui.StepCredentials
	}
	return f.step
}
func (f *fakeWizard) Err() *exitcode.Error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doneErr != nil {
		return f.doneErr
	}
	return f.err
}
func (f *fakeWizard) CaptchaURL() string { return "https://verify.example/captcha?token=t" }
func (f *fakeWizard) Submit(_ context.Context, address string, password []byte, unattended bool) error {
	f.mu.Lock()
	f.pw, f.unatt = string(password), unattended
	f.mu.Unlock()
	f.enter("submit " + address)
	return nil
}
func (f *fakeWizard) SubmitTOTP(_ context.Context, code string) error {
	f.enter("totp " + code)
	return nil
}
func (f *fakeWizard) CaptchaSolved(context.Context) error { f.enter("captcha solved"); return nil }
func (f *fakeWizard) Done() (ui.AccountView, string) {
	return ui.AccountView{ID: "new1", Address: "carol@example.org", URL: "http://127.0.0.1:8080/", Username: "carol@example.org"}, "wizard-pw"
}
func (f *fakeWizard) Cancel() {
	f.enter("cancel")
	if f.canceled != nil {
		close(f.canceled)
	}
}

var _ WizardModel = (*ui.Wizard)(nil)

func TestWizardScreensFollowSteps(t *testing.T) {
	f := newFixture(t)
	f.wiz.script = map[string]ui.Step{
		"submit carol@example.org": ui.StepTOTP,
		"totp 123456":              ui.StepCaptcha,
		"captcha solved":           ui.StepDone,
	}
	f.w.Update(twoAccounts())
	f.w.Show("a1")
	f.tap("+ Add account")
	if !f.has("Repair the session without me") || !f.has(ui.UnattendedWarning) {
		t.Errorf("credentials screen lacks the unattended checkbox and its warning: %v", f.texts())
	}
	test.Type(f.entry("name@example.org"), "carol@example.org")
	pw := f.entry("Proton password")
	test.Type(pw, "hunter2")
	test.Tap(f.check("Repair the session without me"))
	f.tap("Sign in")
	if got := f.wiz.got(); !slices.Equal(got, []string{"submit carol@example.org"}) {
		t.Fatalf("calls = %v", got)
	}
	if f.wiz.pw != "hunter2" || !f.wiz.unatt {
		t.Errorf("Submit got pw=%q unattended=%v", f.wiz.pw, f.wiz.unatt)
	}
	if !f.has("2FA code") {
		t.Fatalf("not on the TOTP screen: %v", f.texts())
	}
	test.Type(f.entry("123456"), "123456")
	f.tap("Verify")
	if !f.hasButton("Open verification") {
		t.Fatalf("not on the CAPTCHA screen: %v", f.texts())
	}
	f.tap("Open verification")
	if !slices.Equal(f.open, []string{"https://verify.example/captcha?token=t"}) {
		t.Errorf("open called with %v", f.open)
	}
	f.tap("I solved it")
	for _, s := range []string{"http://127.0.0.1:8080/", "carol@example.org", mask} {
		if !f.has(s) {
			t.Errorf("done screen lacks %q: %v", s, f.texts())
		}
	}
	f.tap("Show")
	if !f.has("wizard-pw") {
		t.Errorf("Show did not reveal the password: %v", f.texts())
	}
	f.tap("Copy")
	if got := f.app.Clipboard().Content(); got != "wizard-pw" {
		t.Errorf("clipboard = %q", got)
	}
	f.tap("Done")
	if f.w.wizard != nil {
		t.Error("wizard still open after Done")
	}
	if got := f.wiz.got(); slices.Contains(got, "cancel") {
		t.Errorf("Done cancelled the finished wizard: %v", got)
	}
}

func TestWizardClearsPasswordEntryOnSubmit(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	test.Type(f.entry("name@example.org"), "carol@example.org")
	pw := f.entry("Proton password")
	test.Type(pw, "hunter2")
	f.wiz.block = make(chan struct{})
	test.Tap(f.button("Sign in"))
	if pw.Text != "" {
		t.Errorf("password entry still holds %q while Submit runs", pw.Text)
	}
	if !f.has("Working…") {
		t.Errorf("no Working…: %v", f.texts())
	}
	close(f.wiz.block)
	f.wait()
	if f.wiz.pw != "hunter2" {
		t.Errorf("Submit got %q: the copy was taken after the clear", f.wiz.pw)
	}
}

func TestWizardShowsWaitingAfterCaptcha(t *testing.T) {
	f := newFixture(t)
	f.wiz.step = ui.StepCaptcha
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	f.wiz.block = make(chan struct{})
	test.Tap(f.button("I solved it"))
	if !f.has("Waiting for Proton (up to a minute)…") {
		t.Errorf("no waiting text: %v", f.texts())
	}
	if !f.button("I solved it").Disabled() {
		t.Error("button enabled while waiting")
	}
	close(f.wiz.block)
	f.wait()
}

func TestWizardShowsError(t *testing.T) {
	f := newFixture(t)
	f.wiz.err = &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("wrong password"), Hint: "check it"}
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	test.Type(f.entry("name@example.org"), "carol@example.org")
	f.tap("Sign in")
	if !f.has("wrong password") || !f.has("check it") {
		t.Errorf("error not shown: %v", f.texts())
	}
}

func TestWizardCancelledWhenLeft(t *testing.T) {
	for name, leave := range map[string]func(*fixture){
		"cancel button": func(f *fixture) { f.tap("Cancel") },
		"other account": func(f *fixture) { test.Tap(f.w.rows[0].btn); f.wait() },
		"hide":          func(f *fixture) { f.w.Hide(); f.wait() },
		"settings":      func(f *fixture) { f.tap("Settings") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.wiz.step = ui.StepTOTP
			f.w.Update(twoAccounts())
			f.w.Show("")
			f.tap("+ Add account")
			leave(f)
			f.wait()
			if got := f.wiz.got(); !slices.Equal(got, []string{"cancel"}) {
				t.Errorf("calls = %v, want one cancel", got)
			}
			if f.w.wizard != nil {
				t.Error("wizard still held")
			}
		})
	}
}

func TestWizardCancelRunsOffTheCallerGoroutine(t *testing.T) {
	f := newFixture(t)
	f.wiz.step = ui.StepTOTP
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	f.wiz.block = make(chan struct{})
	done := make(chan struct{})
	go func() { f.w.Hide(); close(done) }()
	select { // Hide returns although Cancel is still blocked
	case <-done:
	case <-time.After(2 * time.Second):
		close(f.wiz.block)
		t.Fatal("Hide waited for Cancel: it ran on the caller's goroutine")
	}
	close(f.wiz.block)
	f.wait()
	if got := f.wiz.got(); !slices.Equal(got, []string{"cancel"}) {
		t.Errorf("calls = %v", got)
	}
}

func TestAddAccountDuringWizardCancelsTheOldOne(t *testing.T) {
	f := newFixture(t)
	f.wiz.step = ui.StepTOTP
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	f.tap("+ Add account")
	if got := f.wiz.got(); !slices.Equal(got, []string{"cancel"}) {
		t.Errorf("calls = %v, want exactly one cancel of the first flow", got)
	}
}

func TestAddAccountAfterDoneHidesThePassword(t *testing.T) {
	f := newFixture(t)
	f.wiz.step = ui.StepDone
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	f.tap("Show")
	pb := f.w.wiz.done
	if pb == nil || pb.label.Text != "wizard-pw" {
		t.Fatal("setup: Done password not shown")
	}
	f.tap("+ Add account")
	if pb.label.Text != mask {
		t.Errorf("Done password still displayed after starting another wizard: %q", pb.label.Text)
	}
}

func TestDoneErrorIsShown(t *testing.T) {
	f := newFixture(t)
	f.wiz.step = ui.StepDone
	f.wiz.doneErr = &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("bridge stopped"), Hint: "restart it"}
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	if !f.has("bridge stopped") || !f.has("restart it") {
		t.Errorf("Done error not shown with its hint: %v", f.texts())
	}
}

// An agent's login tool (pcb-ui is the daemon): the window shows the
// add-account wizard with the username filled in.
func TestShowLoginOpensTheWizardPrefilled(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.ShowLogin("carol@example.org")
	f.wait()
	if !f.w.visible {
		t.Error("the window is not shown")
	}
	if f.w.mode != paneWizard || f.w.wizard == nil {
		t.Fatalf("mode %v: want the wizard", f.w.mode)
	}
	if got := f.entry("name@example.org").Text; got != "carol@example.org" {
		t.Errorf("address = %q, want the username filled in", got)
	}
	if !f.hasButton("Sign in") {
		t.Errorf("no Sign in: %v", f.texts())
	}
}

// A login the person is in the middle of is not replaced by an agent's:
// the window only comes to the front.
func TestShowLoginKeepsALoginInProgress(t *testing.T) {
	f := newFixture(t)
	f.wiz.script = map[string]ui.Step{"submit carol@example.org": ui.StepTOTP, "cancel": ui.StepCredentials}
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("+ Add account")
	f.entry("name@example.org").SetText("carol@example.org")
	f.entry("Proton password").SetText("pw")
	f.tap("Sign in")
	if f.wiz.Step() != ui.StepTOTP {
		t.Fatalf("control: step %q", f.wiz.Step())
	}
	f.w.ShowLogin("dave@example.org")
	f.wait()
	if slices.Contains(f.wiz.got(), "cancel") {
		t.Errorf("the login in progress was cancelled: %v", f.wiz.got())
	}
	f.entry("123456") // the 2FA screen is still there
	// Once that login is left, the wizard opens for the agent's username.
	f.w.Hide()
	f.wait()
	f.w.ShowLogin("dave@example.org")
	f.wait()
	if got := f.entry("name@example.org").Text; got != "dave@example.org" {
		t.Errorf("address = %q, want dave", got)
	}
}
