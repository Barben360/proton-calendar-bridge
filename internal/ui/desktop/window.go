// Package desktop is the window of pcb-ui: the accounts, the CalDAV
// connection, the add-account wizard and the settings. It draws the view
// model of internal/ui and calls its actions; it holds no state of the
// bridge. It takes a fyne.App and never imports the app or driver packages,
// so it builds and tests (fyne's test driver) with CGO_ENABLED=0.
//
// Fyne's rule: a widget changes only on the main goroutine. Everything that
// can block (every Actions call, every Wizard call, Settings that opens
// something) runs in a goroutine, and its result is applied with fyne.Do.
package desktop

import (
	"context"
	"errors"
	"image/color"
	"log/slog"
	"net/url"
	"strings"
	"sync"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// Settings is what the window's settings ask of the application (restart and
// the doctor report are ui.Actions).
type Settings interface {
	Autostart() bool
	SetAutostart(on bool) error
	Level() slog.Level
	// SetLevel changes the log level of this run only.
	SetLevel(l slog.Level)
	OpenLogs() error
	OpenConfig() error
}

// WizardModel is the add-account flow the window draws: *ui.Wizard.
type WizardModel interface {
	Step() ui.Step
	Err() *exitcode.Error
	CaptchaURL() string
	Submit(ctx context.Context, address string, password []byte, unattended bool) error
	SubmitTOTP(ctx context.Context, code string) error
	CaptchaSolved(ctx context.Context) error
	Done() (ui.AccountView, string)
	Cancel()
}

var _ WizardModel = (*ui.Wizard)(nil)

const mask = "••••••••"

type paneKind int

const (
	paneEmpty paneKind = iota
	paneAccount
	paneWizard
	paneSettings
)

type row struct {
	id  string
	btn *widget.Button
	dot *canvas.Circle
}

// Window is the pcb-ui window. Every method runs on the main goroutine.
type Window struct {
	act       ui.Actions
	set       Settings
	newWizard func() WizardModel
	open      func(u *url.URL) error
	app       fyne.App
	win       fyne.Window

	view     ui.View
	visible  bool
	mode     paneKind
	selected string // the account of the account pane

	banner *widget.Label
	notice *widget.Label // the one-time no-tray line
	rows   []row
	rowBox *fyne.Container
	right  *fyne.Container

	pane     *accountPane
	wiz      *wizardPane
	wizard   WizardModel // the flow in progress, nil when none
	settings *settingsPane

	// quitOnClose, when set (no tray host: Attach), is what closing the
	// window runs instead of hiding it.
	quitOnClose func()

	wg sync.WaitGroup // goroutines in flight (tests wait on it)
	// dispatch runs a function on the main goroutine: fyne.Do. Tests
	// replace it by a queue they drain themselves (fyne's test driver runs
	// fyne.Do's function on the calling goroutine, which is not the thread
	// the test is on).
	dispatch func(func())
}

// NewWindow builds the window, hidden. newWizard starts a fresh add-account
// flow; open opens a URL in the browser.
func NewWindow(a fyne.App, act ui.Actions, set Settings, newWizard func() WizardModel, open func(u *url.URL) error) *Window {
	w := &Window{app: a, act: act, set: set, newWizard: newWizard, open: open, view: ui.View{Accounts: []ui.AccountView{}}, dispatch: fyne.Do}
	w.win = a.NewWindow("pcb")
	w.banner = widget.NewLabel("")
	w.banner.Wrapping = fyne.TextWrapWord
	w.banner.Hide()
	w.notice = widget.NewLabel("")
	w.notice.Wrapping = fyne.TextWrapWord
	w.notice.Hide()
	w.rowBox = container.NewVBox()
	w.pane = newAccountPane(w)
	w.wiz = &wizardPane{w: w}
	w.settings = newSettingsPane(w)
	w.right = container.NewStack(widget.NewLabel("Add an account to start."))

	add := widget.NewButton("+ Add account", func() { w.openWizard("") })
	gear := widget.NewButtonWithIcon("Settings", theme.SettingsIcon(), w.openSettings)
	left := container.NewBorder(nil, container.NewVBox(add, gear), nil, nil, container.NewVScroll(w.rowBox))
	split := container.NewHSplit(left, container.NewPadded(w.right))
	split.Offset = 0.3
	w.win.SetContent(container.NewBorder(container.NewVBox(w.banner, w.notice), nil, nil, nil, split))
	w.win.Resize(fyne.NewSize(800, 550))
	w.win.SetCloseIntercept(w.onClose)
	return w
}

// async runs work off the main goroutine, then done on it.
func (w *Window) async(work, done func()) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		work()
		w.dispatch(done)
	}()
}

// busy is the waiting state of a screen: its controls are disabled and a
// label says what runs.
type busy struct {
	label *widget.Label
	ctrls []fyne.Disableable
	on    bool
}

func newBusy(ctrls ...fyne.Disableable) *busy {
	l := widget.NewLabel("")
	l.Hide()
	return &busy{label: l, ctrls: ctrls}
}

func (b *busy) set(on bool, text string) {
	b.on = on
	b.label.SetText(text)
	if on {
		b.label.Show()
	} else {
		b.label.Hide()
	}
	for _, c := range b.ctrls {
		if on {
			c.Disable()
		} else {
			c.Enable()
		}
	}
}

// run calls work off the main goroutine while b shows text, then done with
// its error on the main one. A second call while one runs is ignored.
func (w *Window) run(b *busy, text string, work func(ctx context.Context) error, done func(err error)) {
	if b.on {
		return
	}
	b.set(true, text)
	var err error
	w.async(func() { err = work(context.Background()) }, func() {
		b.set(false, "")
		done(err)
	})
}

// showErr tells the person what failed and what to do about it.
func (w *Window) showErr(err error) {
	msg := err.Error()
	var ee *exitcode.Error
	if errors.As(err, &ee) && ee.Hint != "" {
		msg += "\n" + ee.Hint
	}
	dialog.NewCustom("Something went wrong", "OK", wrapped(msg), w.win).Show()
}

func wrapped(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.Wrapping = fyne.TextWrapWord
	return l
}

// confirm asks, and calls yes when the person agrees.
func (w *Window) confirm(title, text, yes string, do func()) {
	dialog.NewCustomConfirm(title, yes, "Cancel", wrapped(text), func(ok bool) {
		if ok {
			do()
		}
	}, w.win).Show()
}

// dotColor is the colour of an account's dot.
func dotColor(s ui.TrayState) color.Color {
	switch s {
	case ui.TrayAttention:
		return color.NRGBA{R: 0xd6, G: 0x3b, B: 0x3b, A: 0xff}
	case ui.TraySyncing:
		return color.NRGBA{R: 0xe0, G: 0xa1, B: 0x00, A: 0xff}
	}
	return color.NRGBA{R: 0x2e, G: 0x9e, B: 0x4f, A: 0xff}
}

// Update draws a new view (call it through fyne.Do from another goroutine).
// What the person is doing, a wizard or a shown password, is kept.
func (w *Window) Update(v ui.View) {
	w.view = v
	var lines []string
	if v.Bridge != "" {
		lines = append(lines, v.Bridge)
	}
	for _, a := range v.Alerts {
		lines = append(lines, a.Message+" — "+a.Fix)
	}
	w.banner.SetText(strings.Join(lines, "\n"))
	if len(lines) == 0 {
		w.banner.Hide()
	} else {
		w.banner.Show()
	}
	w.drawRows()
	switch w.mode {
	case paneAccount:
		if av, ok := w.account(w.selected); ok {
			w.pane.setAccount(av)
		} else {
			w.showDefault()
		}
	case paneEmpty:
		w.showDefault()
	case paneSettings:
		w.settings.setListener(v.Listener)
	}
}

func (w *Window) account(id string) (ui.AccountView, bool) {
	for _, a := range w.view.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return ui.AccountView{}, false
}

func (w *Window) drawRows() {
	w.rows = w.rows[:0]
	objs := make([]fyne.CanvasObject, 0, len(w.view.Accounts))
	for _, a := range w.view.Accounts {
		id := a.ID
		name := a.Address
		if name == "" {
			name = id
		}
		btn := widget.NewButton(name, func() { w.selectAccount(id) })
		btn.Alignment = widget.ButtonAlignLeading
		btn.Importance = widget.LowImportance
		if w.mode == paneAccount && id == w.selected {
			btn.Importance = widget.MediumImportance
		}
		dot := canvas.NewCircle(dotColor(a.Dot))
		dot.Resize(fyne.NewSquareSize(12))
		w.rows = append(w.rows, row{id: id, btn: btn, dot: dot})
		objs = append(objs, container.NewBorder(nil, nil, container.NewGridWrap(fyne.NewSquareSize(14), dot), nil, btn))
	}
	w.rowBox.Objects = objs
	w.rowBox.Refresh()
}

func (w *Window) setRight(o fyne.CanvasObject) {
	w.right.Objects = []fyne.CanvasObject{o}
	w.right.Refresh()
}

// leave runs what changing pane means for the pane being left: a shown
// password hides, a wizard in progress is cancelled (off the main goroutine:
// Cancel waits for a call in flight).
func (w *Window) leave(next paneKind) {
	w.pane.pw.hide()
	if next != paneWizard {
		w.closeWizard()
	}
}

// closeWizard drops the wizard in progress. Unless it finished, it is
// cancelled, which drops the login the Hub holds for it.
func (w *Window) closeWizard() {
	m := w.wizard
	if m == nil {
		return
	}
	w.wizard = nil
	w.wiz.reset()
	if m.Step() == ui.StepDone {
		return
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		m.Cancel()
	}()
}

// showDefault shows the selected account, else the first, else nothing.
func (w *Window) showDefault() {
	if _, ok := w.account(w.selected); ok {
		w.selectAccount(w.selected)
	} else if len(w.view.Accounts) > 0 {
		w.selectAccount(w.view.Accounts[0].ID)
	} else {
		w.leave(paneEmpty)
		w.mode, w.selected = paneEmpty, ""
		if w.view.Bridge != "" {
			// A stopped bridge has no accounts to show, not none to have.
			w.setRight(wrapped(stoppedPane))
		} else {
			w.setRight(widget.NewLabel("Add an account to start."))
		}
	}
}

// stoppedPane is the right pane while the bridge is stopped (the banner says
// why).
const stoppedPane = "The bridge is stopped. Fix what the banner above says, then Settings → Restart bridge."

func (w *Window) selectAccount(id string) {
	av, ok := w.account(id)
	if !ok {
		return
	}
	w.leave(paneAccount)
	w.mode, w.selected = paneAccount, id
	w.pane.setAccount(av)
	w.setRight(w.pane.root)
	w.drawRows()
}

func (w *Window) openSettings() {
	w.leave(paneSettings)
	w.mode = paneSettings
	w.settings.open(w.view.Listener)
	w.setRight(w.settings.root)
	w.drawRows()
}

func (w *Window) openWizard(address string) {
	w.leave(paneWizard)
	w.closeWizard() // a flow still in progress is cancelled, a shown password hides
	w.mode = paneWizard
	w.wizard = w.newWizard()
	w.wiz.start(address)
	w.setRight(w.wiz.root)
	w.drawRows()
}

// Show shows the window, on account when it names one the window knows.
func (w *Window) Show(account string) {
	if account != "" {
		w.selectAccount(account)
	} else if w.mode == paneEmpty || w.mode == paneAccount {
		w.showDefault()
	}
	w.visible = true
	w.win.Show()
	w.win.RequestFocus()
}

// onClose is what closing the window does: it hides (the app keeps running
// in the tray), or quits when there is no tray to come back from (R14).
func (w *Window) onClose() {
	w.Hide() // at once: the quit's drain may take a few seconds
	if w.quitOnClose != nil {
		w.quitOnClose()
	}
}

// Hide hides the window: a shown password hides and a wizard in progress is
// cancelled (nothing lingers in a window nobody sees).
func (w *Window) Hide() {
	w.visible = false
	w.win.Hide()
	w.pane.pw.hide()
	if w.mode == paneWizard {
		w.closeWizard()
		w.showDefault()
	}
}
