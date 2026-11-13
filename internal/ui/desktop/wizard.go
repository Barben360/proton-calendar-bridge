package desktop

import (
	"context"
	"net/url"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// wizardPane draws the screen of the add-account wizard's current step. The
// screen is built again after each call, from Step() and Err(). The password
// is read from its entry into a byte slice at the Sign in tap, and the entry
// is cleared at once: no string of it is kept.
type wizardPane struct {
	w       *Window
	root    *fyne.Container
	address string // the address typed (not a secret), kept across screens
	done    *pwBox // the Done screen's password
}

func (p *wizardPane) model() WizardModel { return p.w.wizard }

// start shows the first screen, with address filled in when it is a sign-in.
func (p *wizardPane) start(address string) {
	p.address = address
	p.root = container.NewStack()
	p.render()
}

// reset forgets the screen, hiding a password the Done screen showed.
func (p *wizardPane) reset() {
	if p.done != nil {
		p.done.hide()
		p.done = nil
	}
	p.address = ""
}

func (p *wizardPane) set(o fyne.CanvasObject) {
	p.root.Objects = []fyne.CanvasObject{container.NewVScroll(o)}
	p.root.Refresh()
}

func title(s string) fyne.CanvasObject {
	return widget.NewLabelWithStyle(s, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
}

func (p *wizardPane) errLabel() fyne.CanvasObject {
	e := p.model().Err()
	if e == nil {
		return widget.NewLabel("")
	}
	msg := e.Error()
	if e.Hint != "" {
		msg += "\n" + e.Hint
	}
	return wrapped(msg)
}

// call runs a wizard call off the main goroutine, then draws the next screen
// (unless the wizard was left meanwhile).
func (p *wizardPane) call(b *busy, text string, f func(ctx context.Context, m WizardModel) error) {
	m := p.model()
	p.w.run(b, text, func(ctx context.Context) error { return f(ctx, m) }, func(error) {
		if p.w.wizard == m {
			p.render()
		}
	})
}

func (p *wizardPane) render() {
	switch p.model().Step() {
	case ui.StepTOTP:
		p.totp()
	case ui.StepCaptcha:
		p.captcha()
	case ui.StepDone:
		p.finish()
	default:
		p.credentials()
	}
}

func (p *wizardPane) cancelBtn() *widget.Button {
	return widget.NewButton("Cancel", func() {
		p.w.closeWizard()
		p.w.showDefault()
	})
}

func (p *wizardPane) credentials() {
	addr := widget.NewEntry()
	addr.PlaceHolder = "name@example.org"
	addr.SetText(p.address)
	pw := widget.NewPasswordEntry()
	pw.PlaceHolder = "Proton password"
	un := widget.NewCheck("Repair the session without me", nil)
	warn := wrapped(ui.UnattendedWarning)
	cancel := p.cancelBtn()
	var b *busy
	sign := widget.NewButton("Sign in", func() {
		p.address = strings.TrimSpace(addr.Text)
		secret := []byte(pw.Text)
		pw.SetText("") // the entry never holds it past the tap
		address, unattended := p.address, un.Checked
		p.call(b, "Working…", func(ctx context.Context, m WizardModel) error {
			defer clear(secret)
			return m.Submit(ctx, address, secret, unattended)
		})
	})
	sign.Importance = widget.HighImportance
	b = newBusy(addr, pw, un, sign, cancel)
	p.set(container.NewVBox(title("Add an account"), p.errLabel(), addr, pw, un, warn, container.NewHBox(sign, cancel), b.label))
}

func (p *wizardPane) totp() {
	code := widget.NewEntry()
	code.PlaceHolder = "123456"
	cancel := p.cancelBtn()
	var b *busy
	verify := widget.NewButton("Verify", func() {
		c := strings.TrimSpace(code.Text)
		p.call(b, "Working…", func(ctx context.Context, m WizardModel) error { return m.SubmitTOTP(ctx, c) })
	})
	verify.Importance = widget.HighImportance
	b = newBusy(code, verify, cancel)
	p.set(container.NewVBox(title("Two-factor code"), wrapped("Enter your 2FA code."), p.errLabel(), code, container.NewHBox(verify, cancel), b.label))
}

func (p *wizardPane) captcha() {
	cancel := p.cancelBtn()
	var b *busy
	openBtn := widget.NewButton("Open verification", func() {
		u, err := url.Parse(p.model().CaptchaURL())
		if err == nil {
			err = p.w.open(u)
		}
		if err != nil {
			p.w.showErr(err)
		}
	})
	solved := widget.NewButton("I solved it", func() {
		p.call(b, "Waiting for Proton (up to a minute)…", func(ctx context.Context, m WizardModel) error { return m.CaptchaSolved(ctx) })
	})
	solved.Importance = widget.HighImportance
	b = newBusy(openBtn, solved, cancel)
	p.set(container.NewVBox(title("Human verification"),
		wrapped("A verification is needed. Open it, solve it in the browser, then come back and press I solved it."),
		p.errLabel(), container.NewHBox(openBtn, solved, cancel), b.label))
}

// finish loads the new account, then draws the Done screen.
func (p *wizardPane) finish() {
	m := p.model()
	b := newBusy()
	b.set(true, "Working…")
	p.set(container.NewVBox(title("Account added"), b.label))
	var av ui.AccountView
	p.w.async(func() { av, _ = m.Done() }, func() {
		if p.w.wizard != m {
			return
		}
		if e := m.Err(); e != nil {
			p.w.showErr(e)
			p.drawFailed(e)
			return
		}
		p.drawDone(m, av)
	})
}

func (p *wizardPane) drawDone(m WizardModel, av ui.AccountView) {
	pw := newPwBox(p.w)
	pw.fetch = func() func(context.Context) (string, error) {
		return func(context.Context) (string, error) {
			_, secret := m.Done()
			if secret == "" {
				if e := m.Err(); e != nil {
					return "", e
				}
			}
			return secret, nil
		}
	}
	closeBtn := widget.NewButton("Done", func() {
		p.w.closeWizard()
		if _, ok := p.w.account(av.ID); ok {
			p.w.selectAccount(av.ID)
		} else {
			p.w.showDefault()
		}
	})
	closeBtn.Importance = widget.HighImportance
	b := newBusy(pw.showBtn, pw.copyBtn, closeBtn)
	pw.b = b
	p.done = pw
	p.set(container.NewVBox(title("Account added"),
		wrapped("Use these in your calendar app."),
		container.NewHBox(widget.NewLabel("URL"), widget.NewLabel(av.URL)),
		container.NewHBox(widget.NewLabel("Username"), widget.NewLabel(av.Username)),
		container.NewHBox(widget.NewLabel("Password"), pw.root),
		closeBtn, b.label))
}

// drawFailed is the Done screen when the account's details could not be read.
func (p *wizardPane) drawFailed(e error) {
	closeBtn := widget.NewButton("Done", func() {
		p.w.closeWizard()
		p.w.showDefault()
	})
	p.set(container.NewVBox(title("Account added"), wrapped("The account is added, but its connection details could not be read: "+e.Error()), closeBtn))
}
