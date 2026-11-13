package ui_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

func totpCode(t *testing.T, uri string) string {
	t.Helper()
	o, err := otp.Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	return o.Code(time.Now())
}

func accountDirs(t *testing.T, l *lab) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(l.dirs.State, "accounts"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// forgetLastLogin lifts the one-login-a-minute guard, which has its own
// tests: these ones log in more than once.
func (l *lab) forgetLastLogin(t *testing.T) {
	t.Helper()
	if err := os.Remove(filepath.Join(l.dirs.State, "last-login")); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func TestWizardPlainLogin(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	w := ui.NewWizard(b)
	if w.Step() != ui.StepCredentials {
		t.Fatalf("first step = %q", w.Step())
	}
	if err := w.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if w.Step() != ui.StepDone || w.Err() != nil {
		t.Fatalf("Step = %q, Err = %v", w.Step(), w.Err())
	}
	view, pw := w.Done()
	if view.ID == "" || !strings.EqualFold(view.Address, l.f.Username) {
		t.Errorf("view = %+v", view)
	}
	if want := b.Info().AccountURL(view.ID); view.URL != want || !strings.HasSuffix(want, "/dav/"+view.ID+"/") {
		t.Errorf("URL = %q, want %q (…/dav/<id>/)", view.URL, want)
	}
	if pw == "" {
		t.Fatal("no CalDAV password")
	}
	if got := l.status(t, b, view.ID, pw); got == http.StatusUnauthorized {
		t.Errorf("the shown password is refused: %d", got)
	}
	if again, err := ui.NewActions(b).Password(t.Context(), view.ID); err != nil || again != pw {
		t.Errorf("Password = %q, %v; Done showed %q", again, err, pw)
	}
	if w.PasswordBuf() != nil {
		t.Errorf("the wizard still holds the password after the login")
	}
}

func TestWizardTOTP(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	uri := l.f.SetTwoFA(l.f.Username, true)
	w := ui.NewWizard(b)
	if err := w.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if w.Step() != ui.StepTOTP {
		t.Fatalf("Step = %q, want totp", w.Step())
	}
	// A wrong code keeps the step: the Hub still holds the login.
	if err := w.SubmitTOTP(t.Context(), "000000"); err == nil {
		t.Fatal("a wrong code was accepted")
	}
	if w.Step() != ui.StepTOTP || w.Err() == nil {
		t.Fatalf("after a wrong code: Step = %q, Err = %v; want totp with an error", w.Step(), w.Err())
	}
	if err := w.SubmitTOTP(t.Context(), totpCode(t, uri)); err != nil {
		t.Fatalf("SubmitTOTP: %v", err)
	}
	if w.Step() != ui.StepDone || w.Err() != nil {
		t.Fatalf("Step = %q, Err = %v", w.Step(), w.Err())
	}
	view, pw := w.Done()
	if view.ID == "" || pw == "" {
		t.Errorf("Done = %+v, %q", view, pw)
	}
}

func TestWizardTooManyWrongCodesBackToCredentials(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.f.SetTwoFA(l.f.Username, true)
	w := ui.NewWizard(b)
	if err := w.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3 && w.Step() == ui.StepTOTP; i++ {
		_ = w.SubmitTOTP(t.Context(), "000000")
	}
	if w.Step() != ui.StepCredentials || w.Err() == nil {
		t.Errorf("after the tries ran out: Step = %q, Err = %v; want credentials with an error", w.Step(), w.Err())
	}
	if n := len(b.Hub().Accounts()); n != 0 {
		t.Errorf("%d accounts left", n)
	}
}

func TestWizardKeepsPasswordForCaptchaThenZeroes(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.f.SetCaptchaFor(l.f.Username, "tok")
	w := ui.NewWizard(b)
	pw := []byte(l.f.Password)
	if err := w.Submit(t.Context(), l.f.Username, pw, false); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if w.Step() != ui.StepCaptcha || w.CaptchaURL() == "" {
		t.Fatalf("Step = %q, CaptchaURL = %q", w.Step(), w.CaptchaURL())
	}
	buf := w.PasswordBuf()
	if string(buf) != l.f.Password {
		t.Fatalf("the wizard dropped the password before the CAPTCHA retry: %q", buf)
	}
	l.forgetLastLogin(t)
	if err := w.CaptchaSolved(t.Context()); err != nil {
		t.Fatalf("CaptchaSolved: %v", err)
	}
	if w.Step() != ui.StepDone {
		t.Fatalf("Step = %q (Err %v), want done", w.Step(), w.Err())
	}
	if !allZero(buf) || len(buf) == 0 {
		t.Errorf("the password buffer is not all zero after the login: %v", buf)
	}
	if w.PasswordBuf() != nil {
		t.Errorf("the wizard still points at a password")
	}
}

func TestWizardZeroesOnError(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	w := ui.NewWizard(b)
	if err := w.Submit(t.Context(), l.f.Username, []byte("wrong password"), false); err == nil {
		t.Fatal("a wrong password logged in")
	}
	if w.Step() != ui.StepCredentials || w.Err() == nil {
		t.Errorf("Step = %q, Err = %v", w.Step(), w.Err())
	}
	if w.PasswordBuf() != nil {
		t.Errorf("the wizard still holds the password after an error")
	}
}

// The error of a CAPTCHA retry zeroes the buffer that was kept for it.
func TestWizardZeroesTheBufferWhenTheCaptchaRetryFails(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.f.SetCaptchaFor(l.f.Username, "tok")
	w := ui.NewWizard(b)
	if err := w.Submit(t.Context(), l.f.Username, []byte("wrong password"), false); err != nil || w.Step() != ui.StepCaptcha {
		t.Fatalf("Submit: %v, Step %q (the fake must challenge before it checks the password)", err, w.Step())
	}
	buf := w.PasswordBuf()
	l.forgetLastLogin(t)
	if err := w.CaptchaSolved(t.Context()); err == nil {
		t.Fatal("the retry logged in with a wrong password")
	}
	if !allZero(buf) || len(buf) == 0 || w.Step() != ui.StepCredentials || w.Err() == nil {
		t.Errorf("after the failed retry: zero = %v, Step = %q, Err = %v", allZero(buf), w.Step(), w.Err())
	}
}

func TestWizardErrorIsTheCLIText(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	env := func(k string) string {
		switch k {
		case "PCB_USERNAME":
			return l.f.Username
		case "PCB_PASSWORD":
			return "wrong password"
		}
		return l.env(k)
	}
	var out, errb bytes.Buffer
	code := cli.Main(t.Context(), []string{"login", "--non-interactive", "--json"}, cli.IO{Stdout: &out, Stderr: &errb, Env: env})
	var e struct {
		Error *struct {
			Code    int
			Message string
			Hint    string
		}
	}
	if err := json.Unmarshal(out.Bytes(), &e); err != nil || e.Error == nil {
		t.Fatalf("pcb login (exit %d): %v: %q", code, err, out.String())
	}
	l.forgetLastLogin(t)
	w := ui.NewWizard(b)
	_ = w.Submit(t.Context(), l.f.Username, []byte("wrong password"), false)
	got := w.Err()
	if got == nil {
		t.Fatal("no error")
	}
	if got.Error() != e.Error.Message || got.Hint != e.Error.Hint || int(got.Code) != e.Error.Code {
		t.Errorf("wizard: code %d %q hint %q\ncli:    code %d %q hint %q", got.Code, got.Error(), got.Hint, e.Error.Code, e.Error.Message, e.Error.Hint)
	}
	if e.Error.Message == "" {
		t.Error("control: the CLI error has no message")
	}
}

func TestWizardCancelDuringTOTPLeavesNoAccount(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	uri := l.f.SetTwoFA(l.f.Username, true)
	w := ui.NewWizard(b)
	if err := w.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil || w.Step() != ui.StepTOTP {
		t.Fatalf("Submit: %v, Step %q", err, w.Step())
	}
	if len(accountDirs(t, l)) == 0 {
		t.Fatal("control: the pending add has no directory")
	}
	w.Cancel()
	if n := len(b.Hub().Accounts()); n != 0 {
		t.Errorf("%d accounts after Cancel", n)
	}
	if dirs := accountDirs(t, l); len(dirs) != 0 {
		t.Errorf("account directories after Cancel: %v", dirs)
	}
	if w.Step() != ui.StepCredentials || w.PasswordBuf() != nil {
		t.Errorf("Step = %q after Cancel", w.Step())
	}
	// The held login is gone: a late code finds nothing to finish.
	if err := w.SubmitTOTP(t.Context(), totpCode(t, uri)); err == nil {
		t.Error("SubmitTOTP after Cancel succeeded")
	}
	if n := len(b.Hub().Accounts()); n != 0 {
		t.Errorf("%d accounts after a late code", n)
	}
}

func TestWizardCancelDuringCaptchaLeavesNoAccountAndZeroes(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.f.SetCaptchaFor(l.f.Username, "tok")
	w := ui.NewWizard(b)
	if err := w.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil || w.Step() != ui.StepCaptcha {
		t.Fatalf("Submit: %v, Step %q", err, w.Step())
	}
	buf := w.PasswordBuf()
	w.Cancel()
	if !allZero(buf) || len(buf) == 0 {
		t.Errorf("the password survives Cancel: %v", buf)
	}
	if dirs := accountDirs(t, l); len(dirs) != 0 {
		t.Errorf("account directories after Cancel: %v", dirs)
	}
}

// Cancelling the 2FA step of an account that already existed (signed out)
// must not delete it.
func TestWizardCancelKeepsAnExistingAccount(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	id := l.login(t, b)
	if err := ui.NewActions(b).SignOut(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	l.f.SetTwoFA(l.f.Username, true)
	l.forgetLastLogin(t)
	w := ui.NewWizard(b)
	if err := w.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false); err != nil || w.Step() != ui.StepTOTP {
		t.Fatalf("Submit: %v, Step %q", err, w.Step())
	}
	w.Cancel()
	list := b.Hub().Accounts()
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("accounts after Cancel = %+v, want the existing %s", list, id)
	}
}

func TestWizardRefusesWhileTheBridgeIsNotRunning(t *testing.T) {
	l := newLab(t)
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env}) // never started
	w := ui.NewWizard(b)
	err := w.Submit(context.Background(), l.f.Username, []byte(l.f.Password), false)
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.Blocked || !strings.Contains(err.Error(), "the bridge is not running") || ee.Hint == "" {
		t.Errorf("Submit = %v, want the Blocked not-running error with a hint", err)
	}
	if w.Step() != ui.StepCredentials {
		t.Errorf("Step = %q", w.Step())
	}
	if w.Err() == nil || w.Err().Error() != err.Error() {
		t.Errorf("Err = %v, want the Submit error", w.Err())
	}
}

func TestWizardRefusesOutOfOrderCalls(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	w := ui.NewWizard(b)
	if err := w.SubmitTOTP(t.Context(), "123456"); err == nil {
		t.Error("SubmitTOTP on the credentials step succeeded")
	}
	if err := w.CaptchaSolved(t.Context()); err == nil {
		t.Error("CaptchaSolved on the credentials step succeeded")
	}
	if v, pw := w.Done(); v.ID != "" || pw != "" {
		t.Errorf("Done before the end = %+v, %q", v, pw)
	}
}

func TestWizardCopiesThePassword(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.f.SetCaptchaFor(l.f.Username, "tok")
	w := ui.NewWizard(b)
	pw := []byte(l.f.Password)
	if err := w.Submit(t.Context(), l.f.Username, pw, false); err != nil {
		t.Fatal(err)
	}
	for i := range pw { // the caller wipes its own copy
		pw[i] = 0
	}
	l.forgetLastLogin(t)
	if err := w.CaptchaSolved(t.Context()); err != nil || w.Step() != ui.StepDone {
		t.Errorf("CaptchaSolved = %v, Step %q: the wizard relied on the caller's buffer", err, w.Step())
	}
}

func TestWizardUnattendedWarningIsDoctors(t *testing.T) {
	if ui.UnattendedWarning != doctor.UnattendedKeyringWarning || !strings.HasPrefix(ui.UnattendedWarning, "unattended relogin is on") {
		t.Errorf("UnattendedWarning = %q", ui.UnattendedWarning)
	}
}

func TestWizardIsSafeConcurrently(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.f.SetTwoFA(l.f.Username, true)
	w := ui.NewWizard(b)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Submit(t.Context(), l.f.Username, []byte(l.f.Password), false)
			_ = w.Step()
			_ = w.Err()
			_ = w.CaptchaURL()
			_ = w.SubmitTOTP(t.Context(), "000000")
			w.Done()
			w.Cancel()
		}()
	}
	wg.Wait()
	if n := len(b.Hub().Accounts()); n != 0 {
		t.Errorf("%d accounts left after the cancels", n)
	}
}
