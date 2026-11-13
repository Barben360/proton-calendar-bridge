package desktop

import (
	"embed"

	"fyne.io/fyne/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

//go:embed icons/*.svg
var iconFS embed.FS

// icon loads one of our own icons (a calendar page with a bridge arc).
func icon(name string) fyne.Resource {
	b, err := iconFS.ReadFile("icons/" + name + ".svg")
	if err != nil {
		panic(err) // embedded: a missing file is a build error
	}
	return fyne.NewStaticResource(name+".svg", b)
}

// AppIcon is the application icon (the file make install-ui installs).
func AppIcon() fyne.Resource { return icon("app") }

func trayIcon(s ui.TrayState) fyne.Resource {
	switch s {
	case ui.TrayAttention:
		return icon("attention")
	case ui.TraySyncing:
		return icon("syncing")
	}
	return icon("ok")
}

// TrayApp is what the tray asks of the application: Fyne's desktop.App.
type TrayApp interface {
	SetSystemTrayMenu(*fyne.Menu)
	SetSystemTrayIcon(fyne.Resource)
}

// Tray is the system tray icon and its menu.
type Tray struct {
	app  TrayApp
	w    *Window
	quit func()
}

// NewTray sets the tray up with the "ok" icon and the menu of no account.
func NewTray(a TrayApp, w *Window, quit func()) *Tray {
	t := &Tray{app: a, w: w, quit: quit}
	t.Update(ui.View{})
	return t
}

// Update redraws the icon and the menu from v. It runs on the main goroutine.
func (t *Tray) Update(v ui.View) {
	t.app.SetSystemTrayIcon(trayIcon(v.Tray))
	items := make([]*fyne.MenuItem, 0, len(v.Accounts)+3)
	for _, a := range v.Accounts {
		id, name := a.ID, a.Address
		if name == "" {
			name = id
		}
		label := name
		if a.Short != "" {
			label += " — " + a.Short
		}
		items = append(items, fyne.NewMenuItem(label, func() { t.w.dispatch(func() { t.w.Show(id) }) }))
	}
	if len(items) > 0 {
		items = append(items, fyne.NewMenuItemSeparator())
	}
	items = append(items,
		fyne.NewMenuItem("Open pcb", func() { t.w.dispatch(func() { t.w.Show("") }) }),
		fyne.NewMenuItem("Quit", func() { t.quit() }),
	)
	t.app.SetSystemTrayMenu(fyne.NewMenu("pcb", items...))
}

// NoTrayLine is the one-time line shown when no tray host answers.
const NoTrayLine = "your desktop shows no tray icons; on GNOME install the AppIndicator extension"

const noTrayPref = "noTrayLineShown"

// HintNoTray shows NoTrayLine in the window, once: it reports whether it did
// (the line is remembered in the Fyne preferences). Main goroutine.
func (w *Window) HintNoTray() bool {
	p := w.app.Preferences()
	if p.Bool(noTrayPref) {
		return false
	}
	p.SetBool(noTrayPref, true)
	w.notice.SetText(NoTrayLine)
	w.notice.Show()
	return true
}
