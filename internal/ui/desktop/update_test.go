package desktop

import (
	"slices"
	"testing"

	"fyne.io/fyne/v2/test"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

var notice = ui.UpdateNotice{Available: true, Panel: true, Version: "v0.2.0", Installed: "v0.1.0",
	URL: "https://github.com/Barben360/proton-calendar-bridge/releases/tag/v0.2.0", Hint: "curl … | sh"}

func TestUpdatePanel(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("")
	const text = "pcb v0.2.0 is available (you have v0.1.0)."
	if f.has(text) {
		t.Fatalf("panel before any notice: %v", f.texts())
	}
	laters := 0
	f.w.SetUpdate(notice, func() { laters++ })
	if !f.has(text) {
		t.Fatalf("panel missing: %v", f.texts())
	}
	f.tap("Open release page")
	if !slices.Equal(f.open, []string{notice.URL}) {
		t.Errorf("opened %v, want the release page", f.open)
	}
	f.tap("Copy update command")
	if got := f.app.Clipboard().Content(); got != notice.Hint {
		t.Errorf("clipboard = %q, want the hint", got)
	}
	f.tap("Later")
	if laters != 1 {
		t.Errorf("Later called later %d times", laters)
	}

	without := notice
	without.Hint = ""
	f.w.SetUpdate(without, func() {})
	if !f.has(text) || f.hasButton("Copy update command") {
		t.Errorf("no hint: want the panel without Copy: %v", f.texts())
	}
	hidden := notice
	hidden.Panel = false
	f.w.SetUpdate(hidden, func() {})
	if f.has(text) {
		t.Errorf("Panel false: panel still shown: %v", f.texts())
	}
	f.w.SetUpdate(ui.UpdateNotice{}, func() {})
	if f.has(text) {
		t.Errorf("zero notice: panel still shown: %v", f.texts())
	}
}

// The tray skips a menu whose account lines did not change (GNOME F4): the
// update line must still come and go.
func TestTrayUpdateLine(t *testing.T) {
	fa, tr, f, _ := trayFixture(t)
	v := twoAccounts()
	f.w.Update(v)
	tr.Update(v)
	before := fa.menus
	hidden := notice
	hidden.Panel = false // Later hides the panel, never the tray line
	tr.SetUpdate(hidden)
	got := labels(fa.menu)
	if fa.menus != before+1 || !slices.Contains(got, "Update available: v0.2.0") {
		t.Fatalf("menu = %q after SetUpdate (%d sets)", got, fa.menus-before)
	}
	if i := slices.Index(got, "Update available: v0.2.0"); got[i+1] != "Open pcb" {
		t.Errorf("menu = %q, want the update line just above Open pcb", got)
	}
	fa.menu.Items[slices.Index(got, "Update available: v0.2.0")].Action()
	if !slices.Equal(f.open, []string{notice.URL}) {
		t.Errorf("tray line opened %v", f.open)
	}
	tr.Update(v) // a bridge change keeps the line
	if !slices.Contains(labels(fa.menu), "Update available: v0.2.0") {
		t.Errorf("line lost on a redraw: %q", labels(fa.menu))
	}
	tr.SetUpdate(ui.UpdateNotice{})
	if slices.Contains(labels(fa.menu), "Update available: v0.2.0") {
		t.Errorf("line kept after the update went: %q", labels(fa.menu))
	}
}

func TestSettingsUpdateCheckSwitch(t *testing.T) {
	f := newFixture(t)
	f.w.Update(twoAccounts())
	f.w.Show("")
	f.tap("Settings")
	sw := f.toggle("Check for updates")
	if !sw.On {
		t.Fatal("switch off while the check is on")
	}
	test.Tap(sw)
	f.wait()
	if !f.set.updateOff || !slices.Equal(f.set.calls, []string{"updatecheck"}) {
		t.Errorf("calls %v, off %v", f.set.calls, f.set.updateOff)
	}
}
