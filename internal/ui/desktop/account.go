package desktop

import (
	"context"
	"slices"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
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

// copyOf is a Copy button that puts what l shows on the clipboard (the
// CalDAV URL or username, never a secret: the password has its pwBox),
// drawn like the password's Copy.
func (w *Window) copyOf(l *textLine) *widget.Button {
	b := widget.NewButtonWithIcon("Copy", theme.ContentCopyIcon(), func() { w.app.Clipboard().SetContent(l.Text) })
	b.Importance = widget.LowImportance
	return b
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
	caldav             *toggle
	mcp                *widget.Select  // off, read-only, read-write (mcpChoices)
	mcpSilent          bool            // setAccount is selecting: no call
	agentRow           *fyne.Container // the agent command and the mode's hint, hidden when MCP is off
	agentCopy          *widget.Button
	agentHint          *widget.Label
	conn               *fyne.Container
	url, user, fp      *textLine
	urlCopy, userCopy  *widget.Button // the URL's and the username's Copy (F6)
	fpRow              *fyne.Container
	pw                 *pwBox
	newPw              *widget.Button
	progress           *fyne.Container // the running pass: a bar and what it does, hidden between passes
	progBar            *widget.ProgressBar
	progWait           *widget.ProgressBarInfinite // in place of progBar while the pass has no estimate
	progText           *widget.Label
	calGroup           *fyne.Container
	calendars          *fyne.Container
	drawnCals          []ui.CalendarView // what calendars draws, for the account drawnID
	drawnID            string
	drawnAlerts        []ctl.Alert // what alertBox draws
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
	p.mcp = widget.NewSelect(mcpLabels(), p.setMCP)
	agentCmd := mono(agentCommand)
	p.agentCopy = w.copyOf(agentCmd)
	p.agentHint = wrapped("")
	p.agentHint.Importance = widget.LowImportance
	p.agentHint.SizeName = theme.SizeNameCaptionText
	p.url, p.user, p.fp = mono(""), mono(""), mono("")
	p.urlCopy, p.userCopy = w.copyOf(p.url), w.copyOf(p.user)
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
	p.progBar, p.progWait = widget.NewProgressBar(), widget.NewProgressBarInfinite()
	p.progText = wrapped("")
	p.progText.Importance = widget.LowImportance
	p.progText.SizeName = theme.SizeNameCaptionText
	p.progress = vbox(2, container.NewStack(p.progBar, p.progWait), p.progText)
	p.showProgress(nil)

	header := container.NewBorder(nil, nil, nil, vcenter(container.NewHBox(p.signBtn, p.removeBtn)),
		vbox(0, p.addr, container.NewBorder(nil, nil, pad(0, 0, 8, 0, dotObj), nil, p.status)))
	p.agentRow = divided(vbox(0, groupRow(nil, 0, agentCmd, p.agentCopy), pad(0, 6, 4, 8, p.agentHint)))
	adapters := group("ADAPTERS",
		groupRow(adapterKey("CalDAV", "calendar apps"), 0, nil, vcenter(p.caldav)),
		divided(groupRow(adapterKey("MCP", "AI agents"), 0, nil, vcenter(p.mcp))),
		p.agentRow,
	)
	p.fpRow = divided(groupRow(key("Fingerprint"), keyWidth, p.fp))
	p.conn = group("CALDAV CONNECTION",
		groupRow(key("Server"), keyWidth, p.url, p.urlCopy),
		divided(groupRow(key("Username"), keyWidth, p.user, p.userCopy)),
		divided(groupRow(key("Password"), keyWidth, vcenter(p.pw.label), p.pw.showBtn, p.pw.copyBtn)),
		p.fpRow,
		divided(pad(2, 2, 4, 4, p.newPw)),
	)
	p.calGroup = group("CALENDARS", p.calendars)
	body := vbox(18, header, p.b.label, p.alertBox, p.progress, adapters, p.conn, p.calGroup)
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
	shifts := p.progress.Visible() != (av.Progress != nil) || !slices.Equal(av.Alerts, p.drawnAlerts)
	if shifts && av.ID == p.drawnID {
		// The rows below move: a reason shown stays where its icon was.
		p.w.tips.hide(nil)
	}
	p.showProgress(av.Progress)
	if av.SignedIn {
		p.signBtn.SetText("Sign out")
		p.signBtn.Importance = widget.MediumImportance
	} else {
		p.signBtn.SetText("Sign in")
		p.signBtn.Importance = widget.HighImportance
	}
	p.signBtn.Refresh()
	p.caldav.SetOn(av.CalDAV)
	p.mcpSilent = true
	p.mcp.SetSelected(mcpLabel(av.MCPMode))
	p.mcpSilent = false
	p.showAgentHint(av.MCPMode)
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
	p.drawCalendars(av)
	alerts := make([]fyne.CanvasObject, 0, len(av.Alerts))
	for _, a := range av.Alerts {
		msg := widget.NewLabelWithStyle(a.Message, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		msg.Wrapping = fyne.TextWrapWord
		fix := wrapped("Fix: " + a.Fix)
		alerts = append(alerts, alertBox(vbox(0, msg, fix)))
	}
	p.alertBox.Objects = alerts
	p.alertBox.Refresh()
	p.drawnAlerts = slices.Clone(av.Alerts)
}

// showProgress draws the running pass pv, or hides the progress (nil): a
// bar of its fraction, or a moving one while it has no estimate, and what
// it does under it.
func (p *accountPane) showProgress(pv *ui.ProgressView) {
	if pv == nil {
		p.stopWait()
		p.progress.Hide()
		return
	}
	p.progText.SetText(pv.Text)
	if pv.Fraction < 0 {
		p.progBar.Hide()
		p.progWait.Start() // Show alone does not restart a stopped animation
		p.progWait.Show()
	} else {
		p.stopWait()
		p.progBar.SetValue(pv.Fraction)
		p.progBar.Show()
	}
	p.progress.Show()
}

// stopWait stops and hides the moving bar. Stop first: Fyne's Hide clears
// the running flag without a refresh, so the renderer would go on
// animating, and redrawing the window, while hidden.
func (p *accountPane) stopWait() {
	p.progWait.Stop()
	p.progWait.Hide()
}

// drawCalendars draws av's calendars, unless the pane already shows these
// for this account: the window redraws every second while a pass runs, and
// rebuilding the rows would close a reason the person is reading.
func (p *accountPane) drawCalendars(av ui.AccountView) {
	if av.ID == p.drawnID && slices.Equal(av.Calendars, p.drawnCals) {
		return
	}
	p.drawnID, p.drawnCals = av.ID, slices.Clone(av.Calendars)
	p.w.tips.hide(nil) // its icon goes with the rows
	cals := make([]fyne.CanvasObject, 0, len(av.Calendars))
	for i, c := range av.Calendars {
		var tags []fyne.CanvasObject
		if c.ReadOnly {
			tags = append(tags, badge("read-only"))
			if c.Info != "" {
				tags = append(tags, vcenter(newInfoTip(p.w.tips, sentence(c.Info))))
			}
		}
		if c.LastSync != "" {
			tags = append(tags, newTextLine(c.LastSync, theme.ColorNameDisabled, theme.SizeNameCaptionText, fyne.TextStyle{}))
		}
		if c.Error != "" {
			tags = append(tags, newTextLine("error: "+c.Error, theme.ColorNameError, theme.SizeNameCaptionText, fyne.TextStyle{}))
		}
		name := newTextLine(c.Name, theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{})
		name.Truncate = true
		r := groupRow(nil, 0, container.NewBorder(nil, nil, pad(0, 0, 0, 8, calendarDot(c.Color)), nil, name), tags...)
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
}

func (p *accountPane) setCalDAV(on bool) { p.flip(p.caldav, p.w.act.SetCalDAV, on) }

// mcpChoices are the MCP selector's choices, in its order.
var mcpChoices = []struct {
	label string
	mode  service.MCPMode
}{{"Off", service.MCPOff}, {"Read-only", service.MCPReadOnly}, {"Read-write", service.MCPReadWrite}}

func mcpLabels() []string {
	out := make([]string, len(mcpChoices))
	for i, c := range mcpChoices {
		out[i] = c.label
	}
	return out
}

// agentCommand adds pcb to Claude Code, for every project, with the
// default (full) profile.
const agentCommand = "claude mcp add --scope user pcb -- pcb mcp"

// agentHints say what the agents may do under each mode that lets them see
// the account: the account's mode and the agent's profile both apply.
var agentHints = map[service.MCPMode]string{
	service.MCPReadWrite: "Agents read and edit this account, unless you add them with --profile read-only.",
	service.MCPReadOnly:  "Agents only read this account: every change is refused, whatever their profile.",
}

// showAgentHint shows the agent command and mode's hint, or hides them
// when agents do not see the account.
func (p *accountPane) showAgentHint(mode service.MCPMode) {
	hint, ok := agentHints[mode]
	if !ok {
		p.agentRow.Hide()
		return
	}
	p.agentHint.SetText(hint)
	p.agentRow.Show()
}

// mcpLabel is mode's choice; an unknown mode selects none.
func mcpLabel(mode service.MCPMode) string {
	for _, c := range mcpChoices {
		if c.mode == mode {
			return c.label
		}
	}
	return ""
}

// setMCP runs the person's choice on the MCP selector; a failure selects
// the account's mode back, unless the pane shows another account by then.
func (p *accountPane) setMCP(label string) {
	if p.mcpSilent {
		return
	}
	var mode service.MCPMode
	for _, c := range mcpChoices {
		if c.label == label {
			mode = c.mode
		}
	}
	if mode == "" || mode == p.av.MCPMode {
		return
	}
	id, prev := p.av.ID, p.av.MCPMode
	p.w.run(p.b, "Working…", func(ctx context.Context) error { return p.w.act.SetMCP(ctx, id, mode) }, func(err error) {
		if err != nil {
			p.w.showErr(err)
			if p.av.ID == id {
				p.mcpSilent = true
				p.mcp.SetSelected(mcpLabel(prev))
				p.mcpSilent = false
			}
			return
		}
		if p.av.ID == id { // the next Update brings the same; until then, a second choice compares with this one
			p.av.MCPMode = mode
			p.showAgentHint(mode)
		}
	})
}

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
