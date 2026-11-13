package desktop

import (
	"context"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// pwBox is a CalDAV password, masked, with Show and Copy. The password is
// fetched at each use and held, as the label's text, only while shown: it
// hides again when the pane changes, the window hides or the wizard leaves.
type pwBox struct {
	w *Window
	b *busy
	// fetch binds, on the main goroutine, what the call needs (the account)
	// and returns the call, which runs off it.
	fetch   func() func(ctx context.Context) (string, error)
	label   *widget.Label
	showBtn *widget.Button
	copyBtn *widget.Button
	shown   bool
	gen     int // bumped by hide: a password fetched before it never shows
	root    fyne.CanvasObject
}

func newPwBox(w *Window) *pwBox {
	p := &pwBox{w: w, label: widget.NewLabel(mask)}
	p.showBtn = widget.NewButton("Show", p.toggle)
	p.copyBtn = widget.NewButton("Copy", p.copy)
	p.root = container.NewHBox(p.label, p.showBtn, p.copyBtn)
	return p
}

// hide masks the password again and drops any fetch still out.
func (p *pwBox) hide() {
	p.gen++
	p.shown = false
	p.label.SetText(mask)
	p.showBtn.SetText("Show")
}

func (p *pwBox) reveal(pw string) {
	p.shown = true
	p.label.SetText(pw)
	p.showBtn.SetText("Hide")
}

func (p *pwBox) toggle() {
	if p.shown {
		p.hide()
		return
	}
	gen := p.gen
	fetch := p.fetch()
	var pw string
	p.w.run(p.b, "Working…", func(ctx context.Context) (err error) {
		pw, err = fetch(ctx)
		return err
	}, func(err error) {
		switch {
		case err != nil:
			p.w.showErr(err)
		case gen == p.gen:
			p.reveal(pw)
		}
	})
}

func (p *pwBox) copy() {
	fetch := p.fetch()
	var pw string
	p.w.run(p.b, "Working…", func(ctx context.Context) (err error) {
		pw, err = fetch(ctx)
		return err
	}, func(err error) {
		if err != nil {
			p.w.showErr(err)
			return
		}
		p.w.app.Clipboard().SetContent(pw)
	})
}

// accountPane is the right-hand pane of the selected account. One instance
// serves every account, so that changing account can hide the password.
type accountPane struct {
	w  *Window
	av ui.AccountView

	root                *fyne.Container
	addr, state, last   *widget.Label
	signBtn, removeBtn  *widget.Button
	caldav, mcp         *widget.Check
	silent              bool // setting the switches from the view, not the person
	conn                *fyne.Container
	url, user, fp       *widget.Label
	fpRow               *fyne.Container
	pw                  *pwBox
	newPw               *widget.Button
	calendars, alertBox *fyne.Container
	b                   *busy
}

func newAccountPane(w *Window) *accountPane {
	p := &accountPane{w: w}
	p.addr = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.state, p.last = widget.NewLabel(""), widget.NewLabel("")
	p.signBtn = widget.NewButton("Sign out", p.signInOut)
	p.removeBtn = widget.NewButton("Remove", p.remove)
	p.caldav = widget.NewCheck("CalDAV", p.setCalDAV)
	p.mcp = widget.NewCheck("MCP", p.setMCP)
	p.url, p.user, p.fp = widget.NewLabel(""), widget.NewLabel(""), widget.NewLabel("")
	p.pw = newPwBox(w)
	p.pw.fetch = func() func(context.Context) (string, error) {
		id := p.av.ID
		return func(ctx context.Context) (string, error) { return w.act.Password(ctx, id) }
	}
	p.newPw = widget.NewButton("New password", p.newPassword)
	p.b = newBusy(p.signBtn, p.removeBtn, p.caldav, p.mcp, p.newPw, p.pw.showBtn, p.pw.copyBtn)
	p.pw.b = p.b
	p.calendars, p.alertBox = container.NewVBox(), container.NewVBox()

	p.fpRow = container.NewHBox(widget.NewLabel("Fingerprint"), p.fp)
	p.conn = container.NewVBox(
		widget.NewLabelWithStyle("CalDAV connection", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewHBox(widget.NewLabel("URL"), p.url),
		container.NewHBox(widget.NewLabel("Username"), p.user),
		container.NewHBox(widget.NewLabel("Password"), p.pw.root, p.newPw),
		p.fpRow,
	)
	header := container.NewVBox(
		p.addr, container.NewHBox(p.state, p.last),
		container.NewHBox(p.signBtn, p.removeBtn), p.b.label,
	)
	body := container.NewVBox(
		header, widget.NewSeparator(),
		widget.NewLabelWithStyle("Adapters", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), p.caldav, p.mcp,
		p.conn, widget.NewSeparator(),
		widget.NewLabelWithStyle("Calendars", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), p.calendars,
		widget.NewLabelWithStyle("Alerts", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), p.alertBox,
	)
	p.root = container.NewStack(container.NewVScroll(body))
	return p
}

// setAccount draws av. A shown password hides when it is another account.
func (p *accountPane) setAccount(av ui.AccountView) {
	if av.ID != p.av.ID {
		p.pw.hide()
	}
	p.av = av
	p.addr.SetText(av.Address)
	p.state.SetText(av.Short)
	if av.LastSync != nil {
		p.last.SetText("Last sync " + av.LastSync.Local().Format("15:04:05 2 Jan"))
	} else {
		p.last.SetText("Not synced yet")
	}
	if av.SignedIn {
		p.signBtn.SetText("Sign out")
	} else {
		p.signBtn.SetText("Sign in")
	}
	p.setChecked(p.caldav, av.CalDAV)
	p.setChecked(p.mcp, av.MCP)
	p.url.SetText(av.URL)
	p.user.SetText(av.Username)
	p.fp.SetText(av.Fingerprint)
	if av.CalDAV {
		p.conn.Show()
	} else {
		p.conn.Hide()
	}
	if av.Fingerprint != "" {
		p.fpRow.Show()
	} else {
		p.fpRow.Hide()
	}
	cals := make([]fyne.CanvasObject, 0, len(av.Calendars))
	for _, c := range av.Calendars {
		parts := []string{c.Name}
		if c.Badge != "" {
			parts = append(parts, "["+c.Badge+"]")
		} else if c.ReadOnly {
			parts = append(parts, "[read-only]")
		}
		if c.LastSync != "" {
			parts = append(parts, c.LastSync)
		}
		if c.Error != "" {
			parts = append(parts, "error: "+c.Error)
		}
		cals = append(cals, wrapped(strings.Join(parts, "  ")))
	}
	p.calendars.Objects = cals
	p.calendars.Refresh()
	alerts := make([]fyne.CanvasObject, 0, len(av.Alerts))
	for _, a := range av.Alerts {
		alerts = append(alerts, wrapped(a.Message+"\nFix: "+a.Fix))
	}
	p.alertBox.Objects = alerts
	p.alertBox.Refresh()
}

func (p *accountPane) setChecked(sw *widget.Check, on bool) {
	p.silent = true
	sw.SetChecked(on)
	p.silent = false
}

func (p *accountPane) setCalDAV(on bool) { p.flip(p.caldav, p.w.act.SetCalDAV, on) }

func (p *accountPane) setMCP(on bool) { p.flip(p.mcp, p.w.act.SetMCP, on) }

// flip runs set for the person's tap on the switch sw; a failure puts sw
// back, unless the pane shows another account by then.
func (p *accountPane) flip(sw *widget.Check, set func(ctx context.Context, id string, on bool) error, on bool) {
	if p.silent {
		return
	}
	id, prev := p.av.ID, !on
	p.w.run(p.b, "Working…", func(ctx context.Context) error { return set(ctx, id, on) }, func(err error) {
		if err != nil {
			p.w.showErr(err)
			if p.av.ID == id { // the pane may show another account by now
				p.setChecked(sw, prev)
			}
		}
	})
}

func (p *accountPane) signInOut() {
	if !p.av.SignedIn {
		p.w.openWizard(p.av.Address)
		return
	}
	id := p.av.ID
	p.w.run(p.b, "Working…", func(ctx context.Context) error { return p.w.act.SignOut(ctx, id) }, p.report)
}

func (p *accountPane) report(err error) {
	if err != nil {
		p.w.showErr(err)
	}
}

func (p *accountPane) remove() {
	id, addr := p.av.ID, p.av.Address
	p.w.confirm("Remove account", "Remove "+addr+"? Its cache and its CalDAV passwords are deleted, and the session is signed out.", "Remove account", func() {
		p.w.run(p.b, "Working…", func(ctx context.Context) error { return p.w.act.Remove(ctx, id) }, p.report)
	})
}

func (p *accountPane) newPassword() {
	id := p.av.ID
	p.w.confirm("New password", "Make a new CalDAV password for "+p.av.Address+": the old password stops working at once, so update every calendar app that uses it.", "Replace password", func() {
		gen := p.pw.gen
		var pw string
		p.w.run(p.b, "Working…", func(ctx context.Context) (err error) {
			pw, err = p.w.act.NewPassword(ctx, id)
			return err
		}, func(err error) {
			switch {
			case err != nil:
				p.w.showErr(err)
			case gen == p.pw.gen:
				p.pw.reveal(pw)
			}
		})
	})
}
