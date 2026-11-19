package desktop

import (
	"context"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// ReportsModel is the crash reports the window asks about: *ui.Reports.
type ReportsModel interface {
	// Pending is how many reports wait and whether one is a fatal crash.
	Pending() (n int, crashed bool)
	// Text is the waiting reports, exactly as they would be sent.
	Text() string
	Send(ctx context.Context) error
	AlwaysSend(ctx context.Context) error
	DontSend(ctx context.Context) error
}

const (
	reportsCrashed = "pcb stopped unexpectedly"
	reportsError   = "pcb ran into an unexpected error"
	reportsAsk     = "A report with no event, name, address, secret or path can help fix it. Nothing is sent until you agree."
)

func reportsTitle(crashed bool) string {
	if crashed {
		return reportsCrashed
	}
	return reportsError
}

// reportAnswers are the four buttons of the dialog and the panel. Show
// report opens the exact report, read-only; the others answer, then done
// runs on the main goroutine (an error is shown first). While an answer
// runs the buttons are off.
type reportAnswers struct {
	box  *fyne.Container
	btns []*widget.Button
}

func (w *Window) newReportAnswers(done func()) *reportAnswers {
	a := &reportAnswers{}
	answer := func(do func(ctx context.Context) error) func() {
		return func() {
			a.disable(true)
			var err error
			w.async(func() { err = do(context.Background()) }, func() {
				a.disable(false)
				if err != nil {
					w.showErr(err)
					return
				}
				done()
			})
		}
	}
	show := widget.NewButtonWithIcon("Show report", theme.VisibilityIcon(), w.showReport)
	send := widget.NewButton("Send", answer(func(ctx context.Context) error { return w.reports.Send(ctx) }))
	send.Importance = widget.HighImportance
	always := widget.NewButton("Always send", answer(func(ctx context.Context) error { return w.reports.AlwaysSend(ctx) }))
	dont := widget.NewButton("Don't send", answer(func(ctx context.Context) error { return w.reports.DontSend(ctx) }))
	dont.Importance = widget.LowImportance
	a.btns = []*widget.Button{show, send, always, dont}
	a.box = container.NewHBox(show, send, always, dont)
	return a
}

// disable turns the answering buttons off or on (Show report stays on).
func (a *reportAnswers) disable(off bool) {
	for _, b := range a.btns[1:] {
		if off {
			b.Disable()
		} else {
			b.Enable()
		}
	}
}

// showReport opens the waiting reports, read-only and scrollable.
func (w *Window) showReport() {
	l := widget.NewLabel(w.reports.Text())
	l.TextStyle = fyne.TextStyle{Monospace: true}
	sc := container.NewScroll(l)
	sc.SetMinSize(fyne.NewSize(560, 360))
	dialog.NewCustom("Crash report", "Close", sc, w.win).Show()
}

// SetReports gives the window the crash reports it asks about and shows
// them in the Settings switch. Main goroutine, once, before Show.
func (w *Window) SetReports(m ReportsModel) { w.reports = m }

// AskReports asks what to do with the waiting reports in a dialog, over
// the window (shown if hidden). Closing it changes nothing. Main goroutine.
func (w *Window) AskReports() {
	if w.reports == nil {
		return
	}
	_, crashed := w.reports.Pending()
	var dlg dialog.Dialog
	a := w.newReportAnswers(func() { dlg.Hide() })
	dlg = dialog.NewCustom(reportsTitle(crashed), "Close", container.NewVBox(wrapped(reportsAsk), a.box), w.win)
	w.Show("")
	dlg.Show()
}

// reportPanel is the window's "a report was saved" box, under the update
// panel's style: information, not an alert.
type reportPanel struct {
	box  fyne.CanvasObject
	text *widget.Label
}

func newReportPanel(w *Window) *reportPanel {
	p := &reportPanel{}
	p.text = widget.NewLabel("")
	p.text.Wrapping = fyne.TextWrapWord
	p.text.TextStyle = fyne.TextStyle{Bold: true}
	a := w.newReportAnswers(func() { p.box.Hide() })
	icon := widget.NewIcon(theme.InfoIcon())
	body := vbox(6, p.text, wrapped(reportsAsk), a.box)
	p.box = newPanel(container.NewBorder(nil, nil, pad(0, 0, 4, 0, vcenter(icon)), nil, body), colorCard, theme.ColorNameSeparator, 10, 8, 2)
	p.box.Hide()
	return p
}

// ShowReportPanel shows the panel for the waiting reports (a report was
// saved while pcb-ui runs). Main goroutine.
func (w *Window) ShowReportPanel() {
	if w.reports == nil {
		return
	}
	n, crashed := w.reports.Pending()
	if n == 0 {
		w.repPanel.box.Hide()
		return
	}
	w.repPanel.text.SetText(reportsTitle(crashed))
	w.repPanel.box.Show()
}
