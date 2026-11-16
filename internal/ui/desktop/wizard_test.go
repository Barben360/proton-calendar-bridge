package desktop

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

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
	pw       string             // what Submit received
	script   map[string]ui.Step // call name -> step after
	block    chan struct{}
	doneErr  *exitcode.Error
	canceled chan struct{}
	account  string        // what Account returns: the add's local ID
	wait     time.Duration // what CaptchaWait returns
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
func (f *fakeWizard) Account() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.account
}
func (f *fakeWizard) CaptchaURL() string { return "https://verify.example/captcha?token=t" }
func (f *fakeWizard) Submit(_ context.Context, address string, password []byte) error {
	f.mu.Lock()
	f.pw = string(password)
	f.mu.Unlock()
	f.enter("submit " + address)
	return nil
}
func (f *fakeWizard) SubmitTOTP(_ context.Context, code string) error {
	f.enter("totp " + code)
	return nil
}
func (f *fakeWizard) CaptchaSolved(context.Context) error { f.enter("captcha solved"); return nil }
func (f *fakeWizard) CaptchaWait() time.Duration          { return f.wait }
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
	f.tapAdd()
	// Unattended relogin is pcb login --unattended-relogin only: the
	// window offers no box for it and says nothing of it.
	var checks []string
	walk(f.roots(), func(o fyne.CanvasObject) {
		if c, ok := o.(*widget.Check); ok {
			checks = append(checks, c.Text)
		}
	})
	if len(checks) != 0 || f.has("Repair the session") || f.has("If ticked") || f.has("unattended") {
		t.Errorf("credentials screen offers unattended relogin: checks %q, texts %v", checks, f.texts())
	}
	test.Type(f.entry("name@example.org"), "carol@example.org")
	pw := f.entry("Proton password")
	test.Type(pw, "hunter2")
	f.tap("Sign in")
	if got := f.wiz.got(); !slices.Equal(got, []string{"submit carol@example.org"}) {
		t.Fatalf("calls = %v", got)
	}
	if f.wiz.pw != "hunter2" {
		t.Errorf("Submit got pw=%q", f.wiz.pw)
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
	test.Tap(f.w.wiz.done.copyBtn)
	f.wait()
	if got := f.app.Clipboard().Content(); got != "wizard-pw" {
		t.Errorf("clipboard = %q", got)
	}
	// The URL and the username have their own Copy (F6).
	if got := len(f.copies()); got != 3 {
		t.Errorf("Copy buttons on the Done screen: %d, want 3", got)
	}
	test.Tap(f.w.wiz.urlCopy)
	if got := f.app.Clipboard().Content(); got != "http://127.0.0.1:8080/" {
		t.Errorf("URL Copy: clipboard = %q", got)
	}
	test.Tap(f.w.wiz.userCopy)
	if got := f.app.Clipboard().Content(); got != "carol@example.org" {
		t.Errorf("username Copy: clipboard = %q", got)
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
	f.tapAdd()
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

// After I solved it, the window says what it waits for: the rest of the
// CAPTCHA retry's spacing when the CAPTCHA was solved fast (rounded up to
// whole seconds), then Proton.
func TestWizardShowsWaitingAfterCaptcha(t *testing.T) {
	for _, tc := range []struct {
		wait time.Duration
		text string
	}{
		{0, "Waiting for Proton…"},
		{5*time.Second + 200*time.Millisecond, "Retrying in 6 s, then waiting for Proton…"},
	} {
		f := newFixture(t)
		f.wiz.step = ui.StepCaptcha
		f.wiz.wait = tc.wait
		f.w.Update(twoAccounts())
		f.w.Show("")
		f.tapAdd()
		f.wiz.block = make(chan struct{})
		test.Tap(f.button("I solved it"))
		if !f.has(tc.text) {
			t.Errorf("wait %v: no %q: %v", tc.wait, tc.text, f.texts())
		}
		if !f.button("I solved it").Disabled() {
			t.Errorf("wait %v: button enabled while waiting", tc.wait)
		}
		close(f.wiz.block)
		f.wait()
	}
}

func TestWizardShowsError(t *testing.T) {
	f := newFixture(t)
	f.wiz.err = &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("wrong password"), Hint: "check it"}
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tapAdd()
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
			f.tapAdd()
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
	f.tapAdd()
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
	f.tapAdd()
	f.tapAdd()
	if got := f.wiz.got(); !slices.Equal(got, []string{"cancel"}) {
		t.Errorf("calls = %v, want exactly one cancel of the first flow", got)
	}
}

func TestAddAccountAfterDoneHidesThePassword(t *testing.T) {
	f := newFixture(t)
	f.wiz.step = ui.StepDone
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tapAdd()
	f.tap("Show")
	pb := f.w.wiz.done
	if pb == nil || pb.label.Text != "wizard-pw" {
		t.Fatal("setup: Done password not shown")
	}
	f.tapAdd()
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
	f.tapAdd()
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
	f.tapAdd()
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

// A Sign in in flight (the Hub call runs, the step is still the
// credentials) is kept like a 2FA or CAPTCHA step: neither an agent's
// login nor a notification click cancels it (review M5).
func TestShowKeepsASignInInFlight(t *testing.T) {
	for _, tc := range []struct {
		name string
		show func(w *Window)
	}{
		{"agent login", func(w *Window) { w.ShowLogin("dave@example.org") }},
		{"another account", func(w *Window) { w.Show("b2") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.wiz.script = map[string]ui.Step{"submit carol@example.org": ui.StepTOTP, "cancel": ui.StepCredentials}
			f.w.Update(twoAccounts())
			f.w.Show("")
			f.tapAdd()
			f.entry("name@example.org").SetText("carol@example.org")
			f.entry("Proton password").SetText("pw")
			f.wiz.block = make(chan struct{})
			test.Tap(f.button("Sign in")) // no wait: the call stays in flight
			if !f.has("Working…") {
				t.Fatalf("control: the Sign in is not in flight: %v", f.texts())
			}
			tc.show(f.w)
			close(f.wiz.block)
			f.wait()
			if slices.Contains(f.wiz.got(), "cancel") {
				t.Errorf("the Sign in in flight was cancelled: %v", f.wiz.got())
			}
			f.entry("123456") // its 2FA screen follows
		})
	}
}

// withPending is twoAccounts plus the wizard's own add, pending.
func withPending() ui.View {
	v := twoAccounts()
	v.Accounts = append(v.Accounts, ui.AccountView{ID: "new1", Address: "carol@example.org", Short: "needs CAPTCHA", Dot: ui.TrayAttention, Pending: true})
	return v
}

// A wizard waiting for the person (the 2FA code or the CAPTCHA) is kept when
// the window is asked to show an account: a notification click, a tray menu
// line, or a tap on the add's own row. Leaving it would cancel it, and Cancel
// removes the account it created (live 2026-11-13, F2: clicking pcb-ui's own
// needs-captcha notification deleted the add).
func TestShowKeepsAWizardWaitingForThePerson(t *testing.T) {
	for _, tc := range []struct {
		step   ui.Step
		screen string
	}{{ui.StepTOTP, "Verify"}, {ui.StepCaptcha, "Open verification"}} {
		for name, open := range map[string]func(*fixture){
			"notification of the add": func(f *fixture) { f.w.Show("new1") },
			"another account":         func(f *fixture) { f.w.Show("a1") },
			"the add's row":           func(f *fixture) { test.Tap(f.w.rows[2].btn) },
		} {
			t.Run(string(tc.step)+"/"+name, func(t *testing.T) {
				f := newFixture(t)
				f.wiz.step, f.wiz.account = tc.step, "new1"
				f.w.Update(withPending())
				f.w.Show("")
				f.tapAdd()
				f.w.Update(withPending()) // the add appears in the list, pending
				open(f)
				f.wait()
				if got := f.wiz.got(); slices.Contains(got, "cancel") {
					t.Errorf("the login waiting for the person was cancelled: %v", got)
				}
				if f.w.mode != paneWizard || f.w.wizard == nil || !f.hasButton(tc.screen) {
					t.Errorf("mode %v: want the wizard still on its screen %q; visible %v", f.w.mode, tc.screen, f.texts())
				}
				if !f.w.visible {
					t.Error("the window is not shown")
				}
			})
		}
	}
}

// On the credentials screen nothing is lost yet: a notification for another
// account opens that account, as before.
func TestShowLeavesAWizardOnItsFirstScreen(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tapAdd()
	f.w.Show("a1")
	f.wait()
	if f.w.mode != paneAccount || f.w.selected != "a1" {
		t.Errorf("mode %v selected %q: want account a1", f.w.mode, f.w.selected)
	}
}
