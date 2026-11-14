package desktop

import (
	"embed"
	"slices"

	"fyne.io/fyne/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

//go:embed icons/*.svg
var iconFS embed.FS

// icon loads one of our own icons (a grid of days, four of them lit as an
// arch).
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

	// What was last set: Update skips what did not change (live
	// 2026-11-13, F4: each re-set icon is a StatusNotifierItem NewIcon
	// signal, and GNOME's AppIndicator then asks for a property Fyne's
	// systray does not export, logging PropertyNotFound every 30 s).
	icon    ui.TrayState
	iconSet bool
	menu    []string // per account line: its ID and label
	menuSet bool
}

// NewTray sets the tray up with the "ok" icon and the menu of no account.
func NewTray(a TrayApp, w *Window, quit func()) *Tray {
	t := &Tray{app: a, w: w, quit: quit}
	t.Update(ui.View{})
	return t
}

// Update redraws the icon and the menu from v, each only when it changed.
// It runs on the main goroutine.
func (t *Tray) Update(v ui.View) {
	if !t.iconSet || v.Tray != t.icon {
		t.app.SetSystemTrayIcon(trayIcon(v.Tray))
		t.icon, t.iconSet = v.Tray, true
	}
	items := make([]*fyne.MenuItem, 0, len(v.Accounts)+3)
	key := make([]string, 0, len(v.Accounts))
	for _, a := range v.Accounts {
		id, name := a.ID, a.Address
		if name == "" {
			name = id
		}
		label := name
		if a.Short != "" {
			label += " — " + a.Short
		}
		key = append(key, id+"\x00"+label)
		items = append(items, fyne.NewMenuItem(label, func() { t.w.dispatch(func() { t.w.Show(id) }) }))
	}
	if t.menuSet && slices.Equal(key, t.menu) {
		return
	}
	t.menu, t.menuSet = key, true
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

// Attach sets up how the window comes back and how the app quits. With a
// tray host (ta non-nil) the tray holds Quit, closing the window hides it,
// and the window opens unless background. With none (ta nil) the window
// opens even with background, says once that there is no tray, and closing
// it runs quit (ruling R14: a hidden window with no tray could never be
// quit, and its bridge would hold serve.lock unseen). Main goroutine.
func Attach(ta TrayApp, w *Window, quit func(), background bool) *Tray {
	if ta == nil {
		w.quitOnClose = quit
		w.HintNoTray()
		w.Show("")
		return nil
	}
	t := NewTray(ta, w, quit)
	if !background {
		w.Show("")
	}
	return t
}
