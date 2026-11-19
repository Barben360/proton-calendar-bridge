package desktop

import (
	"bytes"
	"encoding/xml"
	"net/url"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/driver/software"
	"fyne.io/fyne/v2/test"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// Fyne draws SVG with a narrower renderer than a browser's: an icon it cannot
// draw comes out as an empty square in the tray, the dock and the window. (A
// single bad element is skipped, the rest still drawn: this guards the whole.)
func TestIconsDrawInFyne(t *testing.T) {
	test.NewTempApp(t)
	for _, n := range []string{"app", "ok", "syncing", "attention"} {
		img := canvas.NewImageFromResource(icon(n))
		img.FillMode = canvas.ImageFillContain
		c := software.NewTransparentCanvas()
		c.SetPadded(false)
		c.SetContent(img)
		c.Resize(fyne.NewSquareSize(48))
		shot := c.Capture()
		b := shot.Bounds()
		drawn := 0
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if _, _, _, a := shot.At(x, y).RGBA(); a > 0x8000 {
					drawn++
				}
			}
		}
		// the page alone covers well over half of the square
		if total := b.Dx() * b.Dy(); drawn*2 < total {
			t.Fatalf("%s: Fyne drew %d of %d pixels, want at least half", n, drawn, total)
		}
	}
}

type fakeTrayApp struct {
	menu         *fyne.Menu
	icon         fyne.Resource
	menus, icons int // the calls
}

func (f *fakeTrayApp) SetSystemTrayMenu(m *fyne.Menu)    { f.menu = m; f.menus++ }
func (f *fakeTrayApp) SetSystemTrayIcon(r fyne.Resource) { f.icon = r; f.icons++ }

func trayFixture(t *testing.T) (*fakeTrayApp, *Tray, *fixture, *int) {
	t.Helper()
	f := newFixture(t)
	f.w.dispatch = func(fn func()) { fn() }
	fa := &fakeTrayApp{}
	quits := 0
	return fa, NewTray(fa, f.w, func() { quits++ }), f, &quits
}

func labels(m *fyne.Menu) []string {
	var out []string
	for _, it := range m.Items {
		if it.IsSeparator {
			out = append(out, "---")
		} else {
			out = append(out, it.Label)
		}
	}
	return out
}

func TestTrayMenuFromView(t *testing.T) {
	fa, tr, f, quits := trayFixture(t)
	v := twoAccounts()
	f.w.Update(v)
	tr.Update(v)
	var want []string
	for _, a := range v.Accounts {
		want = append(want, a.Address+" — "+a.Short)
	}
	want = append(want, "---", "Open pcb", "Quit")
	got := labels(fa.menu)
	if len(got) != len(want) {
		t.Fatalf("menu = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("menu = %q, want %q", got, want)
		}
	}
	// the second account's item opens the window on that account
	fa.menu.Items[1].Action()
	if !f.w.visible || f.w.selected != v.Accounts[1].ID {
		t.Fatalf("visible=%v selected=%q, want the window on %q", f.w.visible, f.w.selected, v.Accounts[1].ID)
	}
	f.w.Hide()
	fa.menu.Items[len(fa.menu.Items)-2].Action()
	if !f.w.visible {
		t.Fatal("Open pcb did not show the window")
	}
	if *quits != 0 {
		t.Fatal("quit called early")
	}
	fa.menu.Items[len(fa.menu.Items)-1].Action()
	if *quits != 1 {
		t.Fatalf("Quit called quit %d times", *quits)
	}
}

func TestTrayMenuWithoutAccounts(t *testing.T) {
	fa, tr, _, _ := trayFixture(t)
	tr.Update(ui.View{})
	got := labels(fa.menu)
	if len(got) != 2 || got[0] != "Open pcb" || got[1] != "Quit" {
		t.Fatalf("menu = %q, want [Open pcb Quit] (no lone separator)", got)
	}
}

func TestTrayIconFollowsState(t *testing.T) {
	fa, tr, _, _ := trayFixture(t)
	seen := map[string][]byte{}
	for state, name := range map[ui.TrayState]string{ui.TrayOK: "ok.svg", ui.TraySyncing: "syncing.svg", ui.TrayAttention: "attention.svg"} {
		tr.Update(ui.View{Tray: state})
		if fa.icon.Name() != name {
			t.Fatalf("state %v: icon %q, want %q", state, fa.icon.Name(), name)
		}
		seen[name] = fa.icon.Content()
	}
	if bytes.Equal(seen["ok.svg"], seen["syncing.svg"]) || bytes.Equal(seen["ok.svg"], seen["attention.svg"]) || bytes.Equal(seen["syncing.svg"], seen["attention.svg"]) {
		t.Fatal("two states share the same artwork")
	}
}

func TestIconsAreValidOwnArtwork(t *testing.T) {
	for _, n := range []string{"app", "ok", "syncing", "attention"} {
		b := icon(n).Content()
		d := xml.NewDecoder(bytes.NewReader(b))
		root := ""
		for {
			tok, err := d.Token()
			if err != nil {
				if err.Error() != "EOF" {
					t.Fatalf("%s: %v", n, err)
				}
				break
			}
			if se, ok := tok.(xml.StartElement); ok && root == "" {
				root = se.Name.Local
			}
		}
		if root != "svg" {
			t.Fatalf("%s: root %q", n, root)
		}
		if bytes.Contains(bytes.ToLower(b), []byte("proton")) {
			t.Fatalf("%s mentions Proton", n)
		}
	}
	if AppIcon().Name() != "app.svg" {
		t.Fatal("app icon name")
	}
}

func TestNoTrayLineShownOnce(t *testing.T) {
	f := newFixture(t)
	if !f.w.HintNoTray() {
		t.Fatal("the first call must show the line")
	}
	if !f.has(NoTrayLine) {
		t.Fatalf("window does not show %q: %q", NoTrayLine, f.texts())
	}
	// a new run: same preferences, a new window
	w2 := NewWindow(f.app, f.act, f.set, func() WizardModel { return f.wiz }, func(*url.URL) error { return nil })
	if w2.HintNoTray() {
		t.Fatal("the line was shown a second time")
	}
	if w2.notice.Visible() {
		t.Fatal("second window shows the line")
	}
}

// Ruling R14: with a tray host, closing the window hides it (Quit is in the
// tray menu); with none, closing it quits (the tray Quit's drain), since a
// hidden window with no tray could never be quit.
func TestCloseQuitsOnlyWithoutATray(t *testing.T) {
	t.Run("tray host", func(t *testing.T) {
		f := newFixture(t)
		quits := 0
		fa := &fakeTrayApp{}
		if tr := Attach(fa, f.w, func() { quits++ }, false); tr == nil || fa.menu == nil {
			t.Fatal("Attach with a tray host set up no tray")
		}
		if !f.w.visible {
			t.Fatal("the window is not shown without --background")
		}
		f.w.onClose()
		if quits != 0 || f.w.visible {
			t.Errorf("closing with a tray: quits = %d, visible = %v; want it hidden, not quit", quits, f.w.visible)
		}
	})
	t.Run("tray host, background", func(t *testing.T) {
		f := newFixture(t)
		Attach(&fakeTrayApp{}, f.w, func() {}, true)
		if f.w.visible {
			t.Error("--background with a tray host showed the window")
		}
	})
	t.Run("no tray host", func(t *testing.T) {
		f := newFixture(t)
		quits := 0
		if tr := Attach(nil, f.w, func() { quits++ }, true); tr != nil {
			t.Fatal("Attach without a tray host returned a tray")
		}
		if !f.w.visible || !f.has(NoTrayLine) {
			t.Fatalf("without a tray the window must open (even --background) with the no-tray line: visible %v, %v", f.w.visible, f.texts())
		}
		f.w.onClose()
		if quits != 1 {
			t.Errorf("closing without a tray quit %d times, want once", quits)
		}
	})
}

// The 30 s refresh re-sent an unchanged icon (StatusNotifierItem NewIcon)
// and menu, and GNOME's AppIndicator logged PropertyNotFound at each one
// (live 2026-11-13, F4): an unchanged view sets nothing; a new state sets
// the icon only, a new line the menu only.
func TestTrayUpdateSkipsWhatDidNotChange(t *testing.T) {
	fa, tr, _, _ := trayFixture(t)
	icons, menus := fa.icons, fa.menus
	v := twoAccounts()
	tr.Update(v)
	if fa.icons != icons+1 || fa.menus != menus+1 {
		t.Fatalf("first view: %d icon and %d menu sets, want 1 and 1", fa.icons-icons, fa.menus-menus)
	}
	for range 3 {
		tr.Update(twoAccounts()) // the 30 s tick, nothing changed
	}
	if fa.icons != icons+1 || fa.menus != menus+1 {
		t.Errorf("unchanged views: %d icon and %d menu sets, want still 1 and 1", fa.icons-icons, fa.menus-menus)
	}
	v.Tray = ui.TrayOK
	tr.Update(v)
	if fa.icons != icons+2 || fa.menus != menus+1 {
		t.Errorf("new tray state: %d icon and %d menu sets, want 2 and 1", fa.icons-icons, fa.menus-menus)
	}
	v.Accounts[0].Short = "synced 2 min ago"
	tr.Update(v)
	if fa.icons != icons+2 || fa.menus != menus+2 {
		t.Errorf("new account line: %d icon and %d menu sets, want 2 and 2", fa.icons-icons, fa.menus-menus)
	}
	if got := labels(fa.menu)[0]; got != "alice@example.org — synced 2 min ago" {
		t.Errorf("menu line = %q", got)
	}
}

// An autostarted pcb-ui can beat the tray host (the AppIndicator extension,
// plasmashell) to the session bus: the probe is retried, not read once.
func TestWaitForTrayHostRetriesUntilItAnswers(t *testing.T) {
	calls := 0
	probe := func() bool { calls++; return calls == 3 }
	if !WaitForTrayHost(probe, time.Second, time.Millisecond) {
		t.Fatal("a host that appeared on the third probe was missed")
	}
	if calls != 3 {
		t.Errorf("probed %d times, want it to stop at the first answer (3)", calls)
	}
}

func TestWaitForTrayHostGivesUpAfterTheWait(t *testing.T) {
	calls := 0
	start := time.Now()
	if WaitForTrayHost(func() bool { calls++; return false }, 30*time.Millisecond, 5*time.Millisecond) {
		t.Fatal("reported a host that never answered")
	}
	if calls < 2 {
		t.Errorf("probed %d times, want it retried", calls)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("waited %v, want it bounded by the wait", d)
	}
	// wait 0 is one probe and no sleep.
	calls = 0
	if WaitForTrayHost(func() bool { calls++; return false }, 0, time.Hour) || calls != 1 {
		t.Errorf("wait 0: %d probes, want exactly one", calls)
	}
}
