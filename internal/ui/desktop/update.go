package desktop

import (
	"fmt"
	"net/url"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// updatePanel is the window's "update available" box, at the top under the
// bridge's banner: information, not an alert (the card colour, an info
// icon). Copy update command shows only where there is a hint.
type updatePanel struct {
	w     *Window
	box   fyne.CanvasObject
	text  *widget.Label
	copy  *widget.Button
	n     ui.UpdateNotice
	later func()
}

func newUpdatePanel(w *Window) *updatePanel {
	p := &updatePanel{w: w, later: func() {}}
	p.text = widget.NewLabel("")
	p.text.Wrapping = fyne.TextWrapWord
	p.text.TextStyle = fyne.TextStyle{Bold: true}
	open := widget.NewButtonWithIcon("Open release page", theme.ComputerIcon(), func() { w.OpenURL(p.n.URL) })
	open.Importance = widget.HighImportance
	p.copy = widget.NewButtonWithIcon("Copy update command", theme.ContentCopyIcon(), func() { w.app.Clipboard().SetContent(p.n.Hint) })
	later := widget.NewButton("Later", func() { p.later() })
	later.Importance = widget.LowImportance
	icon := widget.NewIcon(theme.InfoIcon())
	body := vbox(6, p.text, container.NewHBox(open, p.copy, layout.NewSpacer(), later))
	p.box = newPanel(container.NewBorder(nil, nil, pad(0, 0, 4, 0, vcenter(icon)), nil, body), colorCard, theme.ColorNameSeparator, 10, 8, 2)
	p.box.Hide()
	return p
}

// SetUpdate shows n: the panel while n.Panel, its Later button running
// later. Main goroutine.
func (w *Window) SetUpdate(n ui.UpdateNotice, later func()) {
	p := w.upd
	p.n, p.later = n, later
	if !n.Panel {
		p.box.Hide()
		return
	}
	p.text.SetText(fmt.Sprintf("pcb %s is available (you have %s).", n.Version, n.Installed))
	if n.Hint == "" {
		p.copy.Hide()
	} else {
		p.copy.Show()
	}
	p.box.Show()
}

// OpenURL opens raw in the browser, an error in a dialog.
func (w *Window) OpenURL(raw string) {
	u, err := url.Parse(raw)
	if err == nil {
		err = w.open(u)
	}
	if err != nil {
		w.showErr(err)
	}
}
