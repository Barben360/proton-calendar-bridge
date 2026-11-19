package desktop

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// bridgeLab is the window on a real Bridge over the protontest fake, with a
// real Wizard: the add of the live run of 2026-11-13 (F2). It returns the
// state directory.
func bridgeLab(t *testing.T) (*fixture, *ui.Bridge, *protontest.Fake, string) {
	t.Helper()
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		t.Fatal("the session bus is set: the new state would reach the developer's desktop keyring")
	}
	pf := protontest.New(t, protontest.Options{})
	root, err := os.MkdirTemp("", "pcbd") // short: the control socket's path is bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dirs := paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "state")}
	env := func(k string) string {
		switch k {
		case "PCB_CONFIG_DIR":
			return dirs.Config
		case "PCB_STATE_DIR":
			return dirs.State
		}
		return ""
	}
	if err := os.MkdirAll(dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0") // config.toml refuses port 0: name a port free a moment ago
	if err != nil {
		t.Fatal(err)
	}
	listen := ln.Addr().String()
	_ = ln.Close()
	body := fmt.Sprintf("[proton]\nbase_url = %q\n[caldav]\nlisten = %q\n", pf.URL, listen)
	if err := os.WriteFile(config.Path(dirs), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b := ui.NewBridge(ui.BridgeOptions{Env: env})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, app: test.NewApp(), set: &fakeSettings{}}
	t.Cleanup(func() { test.NewApp() })
	f.w = NewWindow(f.app, ui.NewActions(b), f.set, func() WizardModel { return ui.NewWizard(b) }, func(*url.URL) error { return nil })
	f.w.dispatch = f.enqueue
	return f, b, pf, dirs.State
}

// followOnce is one turn of cmd/pcb-ui's follow loop: the window gets the
// view, the Notifier says what to send.
func followOnce(f *fixture, b *ui.Bridge, n *ui.Notifier) []ui.Notification {
	v := b.View(f.t.Context(), time.Now())
	f.w.Update(v)
	f.wait()
	return n.Next(v)
}

func accountsOnDisk(t *testing.T, state string) int {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(state, "accounts"))
	if err != nil {
		t.Fatal(err)
	}
	return len(ents)
}

// A first add that Proton answers with a CAPTCHA stays on the wizard's
// CAPTCHA step: pcb-ui sends no notification for it, and showing the window
// on that account (what a click on a notification or the tray line does)
// keeps the step and the add (live 2026-11-13, F2: the click deleted it).
func TestCaptchaAddSurvivesShowingItsAccount(t *testing.T) {
	f, b, pf, state := bridgeLab(t)
	pf.SetCaptchaFor(pf.Username, "tok")
	var n ui.Notifier
	followOnce(f, b, &n)
	f.w.Show("")
	f.tapAdd()
	test.Type(f.entry("name@example.org"), pf.Username)
	test.Type(f.entry("Proton password"), pf.Password)
	f.tap("Sign in")
	if !f.hasButton("Open verification") || accountsOnDisk(t, state) != 1 {
		t.Fatalf("control: not on the CAPTCHA step with the add on disk: %v", f.texts())
	}
	id := f.w.wizard.Account()
	if notes := followOnce(f, b, &n); len(notes) != 0 {
		t.Errorf("notifications for the pending add: %+v", notes)
	}
	f.w.Show(id) // the click on a notification, or the tray line of the add
	f.wait()
	followOnce(f, b, &n)
	if got := accountsOnDisk(t, state); got != 1 || len(b.Hub().Accounts()) != 1 {
		t.Fatalf("after showing the add: %d accounts on disk, %d in the Hub, want 1 and 1", got, len(b.Hub().Accounts()))
	}
	if !f.hasButton("Open verification") {
		t.Errorf("the CAPTCHA step is gone: %v", f.texts())
	}
}
