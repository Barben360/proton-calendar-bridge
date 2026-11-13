package desktop

import (
	"bytes"
	"encoding/xml"
	"net/url"
	"testing"

	"fyne.io/fyne/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

type fakeTrayApp struct {
	menu *fyne.Menu
	icon fyne.Resource
}

func (f *fakeTrayApp) SetSystemTrayMenu(m *fyne.Menu)    { f.menu = m }
func (f *fakeTrayApp) SetSystemTrayIcon(r fyne.Resource) { f.icon = r }

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
