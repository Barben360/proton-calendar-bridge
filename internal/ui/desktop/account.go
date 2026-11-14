package desktop

import (
	"context"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
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
}

func newPwBox(w *Window) *pwBox {
	p := &pwBox{w: w, label: widget.NewLabel(mask)}
	p.label.TextStyle = fyne.TextStyle{Monospace: true}
	p.showBtn = widget.NewButtonWithIcon("Show", theme.VisibilityIcon(), p.toggle)
	p.copyBtn = widget.NewButtonWithIcon("Copy", theme.ContentCopyIcon(), p.copy)
	p.showBtn.Importance, p.copyBtn.Importance = widget.LowImportance, widget.LowImportance
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

	root               *fyne.Container
	addr, status       *widget.Label
	statusDot          *canvas.Circle
	signBtn, removeBtn *widget.Button
	caldav, mcp        *toggle
	conn               *fyne.Container
	url, user, fp      *textLine
	fpRow              *fyne.Container
	pw                 *pwBox
	newPw              *widget.Button
	calGroup           *fyne.Container
	calendars          *fyne.Container
	alertBox           *fyne.Container
	b                  *busy
}

// keyWidth is the width of a grouped row's key column.
const keyWidth = 110

func key(s string) fyne.CanvasObject {
	return newTextLine(s, theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{})
}

func mono(s string) *textLine {
	l := newTextLine(s, theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{Monospace: true})
	l.Truncate = true
	return l
}

// adapterKey is a switch's name and, muted, what it is for.
func adapterKey(name, what string) fyne.CanvasObject {
	return container.NewHBox(key(name), newTextLine("· "+what, theme.ColorNameDisabled, theme.SizeNameText, fyne.TextStyle{}))
}

func newAccountPane(w *Window) *accountPane {
	p := &accountPane{w: w}
	p.addr = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	p.addr.SizeName = theme.SizeNameSubHeadingText
	p.addr.Truncation = fyne.TextTruncateEllipsis
	p.status = widget.NewLabel("")
	p.status.Importance = widget.LowImportance
	var dotObj fyne.CanvasObject
	p.statusDot, dotObj = dot(dotColor(ui.TrayOK), 8)
	p.signBtn = widget.NewButton("Sign out", p.signInOut)
	p.removeBtn = widget.NewButton("Remove", p.remove)
	p.removeBtn.Importance = widget.DangerImportance
	p.caldav = newToggle("CalDAV", p.setCalDAV)
	p.mcp = newToggle("MCP", p.setMCP)
	p.url, p.user, p.fp = mono(""), mono(""), mono("")
	p.pw = newPwBox(w)
	p.pw.fetch = func() func(context.Context) (string, error) {
		id := p.av.ID
		return func(ctx context.Context) (string, error) { return w.act.Password(ctx, id) }
	}
	p.newPw = widget.NewButtonWithIcon("New password", theme.ViewRefreshIcon(), p.newPassword)
	p.newPw.Alignment = widget.ButtonAlignLeading
	p.newPw.Importance = widget.LowImportance
	p.b = newBusy(p.signBtn, p.removeBtn, p.caldav, p.mcp, p.newPw, p.pw.showBtn, p.pw.copyBtn)
	p.pw.b = p.b
	p.calendars, p.alertBox = vbox(0), vbox(8)

	header := container.NewBorder(nil, nil, nil, vcenter(container.NewHBox(p.signBtn, p.removeBtn)),
		vbox(0, p.addr, container.NewBorder(nil, nil, pad(0, 0, 8, 0, dotObj), nil, p.status)))
	adapters := group("ADAPTERS",
		groupRow(adapterKey("CalDAV", "calendar apps"), 0, nil, vcenter(p.caldav)),
		divided(groupRow(adapterKey("MCP", "AI agents"), 0, nil, vcenter(p.mcp))),
	)
	p.fpRow = divided(groupRow(key("Fingerprint"), keyWidth, p.fp))
	p.conn = group("CALDAV CONNECTION",
		groupRow(key("Server"), keyWidth, p.url),
		divided(groupRow(key("Username"), keyWidth, p.user)),
		divided(groupRow(key("Password"), keyWidth, vcenter(p.pw.label), p.pw.showBtn, p.pw.copyBtn)),
		p.fpRow,
		divided(pad(2, 2, 4, 4, p.newPw)),
	)
	p.calGroup = group("CALENDARS", p.calendars)
	body := vbox(18, header, p.b.label, p.alertBox, adapters, p.conn, p.calGroup)
	p.root = container.NewStack(container.NewVScroll(pad(4, 24, 28, 28, body)))
	return p
}

// setAccount draws av. A shown password hides when it is another account.
func (p *accountPane) setAccount(av ui.AccountView) {
	if av.ID != p.av.ID {
		p.pw.hide()
	}
	p.av = av
	p.addr.SetText(av.Address)
	last := "not synced yet"
	if av.LastSync != nil {
		last = "last sync " + av.LastSync.Local().Format("15:04:05 2 Jan")
	}
	p.status.SetText(av.Short + " · " + last)
	p.statusDot.FillColor = dotColor(av.Dot)
	p.statusDot.Refresh()
	if av.SignedIn {
		p.signBtn.SetText("Sign out")
		p.signBtn.Importance = widget.MediumImportance
	} else {
		p.signBtn.SetText("Sign in")
		p.signBtn.Importance = widget.HighImportance
	}
	p.signBtn.Refresh()
	p.caldav.SetOn(av.CalDAV)
	p.mcp.SetOn(av.MCP)
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
	for i, c := range av.Calendars {
		var tags []fyne.CanvasObject
		switch {
		case c.Badge != "":
			tags = append(tags, badge(c.Badge))
		case c.ReadOnly:
			tags = append(tags, badge("read-only"))
		}
		if c.LastSync != "" {
			tags = append(tags, newTextLine(c.LastSync, theme.ColorNameDisabled, theme.SizeNameCaptionText, fyne.TextStyle{}))
		}
		if c.Error != "" {
			tags = append(tags, newTextLine("error: "+c.Error, theme.ColorNameError, theme.SizeNameCaptionText, fyne.TextStyle{}))
		}
		name := newTextLine(c.Name, theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{})
		name.Truncate = true
		r := groupRow(nil, 0, name, tags...)
		if i > 0 {
			r = divided(r)
		}
		cals = append(cals, r)
	}
	p.calendars.Objects = cals
	p.calendars.Refresh()
	if len(cals) == 0 {
		p.calGroup.Hide()
	} else {
		p.calGroup.Show()
	}
	alerts := make([]fyne.CanvasObject, 0, len(av.Alerts))
	for _, a := range av.Alerts {
		msg := widget.NewLabelWithStyle(a.Message, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		msg.Wrapping = fyne.TextWrapWord
		fix := wrapped("Fix: " + a.Fix)
		alerts = append(alerts, alertBox(vbox(0, msg, fix)))
	}
	p.alertBox.Objects = alerts
	p.alertBox.Refresh()
}

func (p *accountPane) setCalDAV(on bool) { p.flip(p.caldav, p.w.act.SetCalDAV, on) }

func (p *accountPane) setMCP(on bool) { p.flip(p.mcp, p.w.act.SetMCP, on) }

// flip runs set for the person's tap on the switch sw; a failure puts sw
// back, unless the pane shows another account by then.
func (p *accountPane) flip(sw *toggle, set func(ctx context.Context, id string, on bool) error, on bool) {
	id, prev := p.av.ID, !on
	p.w.run(p.b, "Working…", func(ctx context.Context) error { return set(ctx, id, on) }, func(err error) {
		if err != nil {
			p.w.showErr(err)
			if p.av.ID == id { // the pane may show another account by now
				sw.SetOn(prev)
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
