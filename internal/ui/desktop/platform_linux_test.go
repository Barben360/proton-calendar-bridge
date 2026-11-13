//go:build linux

package desktop

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Barben360/proton-calendar-bridge/internal/dbustest"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

type call struct {
	app, icon, summary, body string
	actions                  []string
	timeout                  int32
}

type fakeNotifyServer struct {
	mu    sync.Mutex
	calls []call
	next  uint32
}

func (s *fakeNotifyServer) Notify(app string, _ uint32, icon, summary, body string, actions []string, _ map[string]dbus.Variant, timeout int32) (uint32, *dbus.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	s.calls = append(s.calls, call{app, icon, summary, body, actions, timeout})
	return 40 + s.next, nil
}

func (s *fakeNotifyServer) got() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

func startFakeNotifier(t *testing.T, addr string) (*fakeNotifyServer, *dbus.Conn) {
	t.Helper()
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := &fakeNotifyServer{}
	if err := conn.Export(s, "/org/freedesktop/Notifications", "org.freedesktop.Notifications"); err != nil {
		t.Fatal(err)
	}
	r, err := conn.RequestName("org.freedesktop.Notifications", dbus.NameFlagDoNotQueue)
	if err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("name: %v %v", r, err)
	}
	return s, conn
}

func emit(t *testing.T, c *dbus.Conn, member string, args ...any) {
	t.Helper()
	if err := c.Emit("/org/freedesktop/Notifications", "org.freedesktop.Notifications."+member, args...); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout: %s", what)
}

func TestNotifyClickOpensAccount(t *testing.T) {
	addr := dbustest.Start(t)
	srv, sc := startFakeNotifier(t, addr)
	t.Cleanup(closeNotifier)

	var mu sync.Mutex
	var clicked []string
	click := func(a string) { mu.Lock(); clicked = append(clicked, a); mu.Unlock() }
	got := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(clicked) }

	if err := Notify(addr, ui.Notification{Account: "a1", Title: "alice@example.org", Body: "sign in again — pcb login"}, click); err != nil {
		t.Fatal(err)
	}
	if err := Notify(addr, ui.Notification{Account: "a2", Title: "bob@example.org", Body: "b"}, click); err != nil {
		t.Fatal(err)
	}
	calls := srv.got()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	c := calls[0]
	if c.app != "pcb" || c.icon != "pcb-ui" || c.summary != "alice@example.org" || c.body != "sign in again — pcb login" ||
		!slices.Equal(c.actions, []string{"default", "Open"}) {
		t.Fatalf("Notify call = %+v", c)
	}
	// ids 41 (a1), 42 (a2): a click on the second opens the second account
	emit(t, sc, "ActionInvoked", uint32(42), "default")
	waitFor(t, "click", func() bool { return len(got()) == 1 })
	if g := got(); g[0] != "a2" {
		t.Fatalf("clicked %v, want [a2]", g)
	}
	// an unknown id, or another action, is ignored
	emit(t, sc, "ActionInvoked", uint32(99), "default")
	emit(t, sc, "ActionInvoked", uint32(41), "other")
	// closing forgets the id: a later click on it does nothing
	emit(t, sc, "NotificationClosed", uint32(41), uint32(2))
	emit(t, sc, "ActionInvoked", uint32(41), "default")
	emit(t, sc, "ActionInvoked", uint32(42), "default") // 42 is still ours, a sentinel after the rest
	waitFor(t, "sentinel", func() bool { return len(got()) >= 2 })
	if g := got(); len(g) != 2 || g[1] != "a2" {
		t.Fatalf("clicks = %v, want [a2 a2]", g)
	}
}

func TestNotifyIgnoresForgedSignals(t *testing.T) {
	addr := dbustest.Start(t)
	_, sc := startFakeNotifier(t, addr)
	t.Cleanup(closeNotifier)
	var mu sync.Mutex
	var clicked []string
	click := func(a string) { mu.Lock(); clicked = append(clicked, a); mu.Unlock() }
	got := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(clicked) }
	if err := Notify(addr, ui.Notification{Account: "a1", Title: "t"}, click); err != nil { // id 41
		t.Fatal(err)
	}
	// another process on the bus forges a click on the live id
	evil, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = evil.Close() }()
	if err := evil.Emit("/org/freedesktop/Notifications", "org.freedesktop.Notifications.ActionInvoked", uint32(41), "default"); err != nil {
		t.Fatal(err)
	}
	// the real server's later signal proves the forged one was processed first
	emit(t, sc, "ActionInvoked", uint32(41), "default")
	waitFor(t, "real click", func() bool { return len(got()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	if g := got(); len(g) != 1 {
		t.Fatalf("clicks = %v, want exactly the real one", g)
	}
}

func TestNotifyForgetsIdsWhenServerChanges(t *testing.T) {
	addr := dbustest.Start(t)
	_, sc := startFakeNotifier(t, addr)
	t.Cleanup(closeNotifier)
	var mu sync.Mutex
	n := 0
	click := func(string) { mu.Lock(); n++; mu.Unlock() }
	count := func() int { mu.Lock(); defer mu.Unlock(); return n }
	if err := Notify(addr, ui.Notification{Account: "a1"}, click); err != nil { // id 41
		t.Fatal(err)
	}
	if err := Notify(addr, ui.Notification{Account: "old2"}, click); err != nil { // id 42
		t.Fatal(err)
	}
	_ = sc.Close() // the server goes away, a new one starts (ids restart)
	_, sc2 := startFakeNotifier(t, addr)
	if err := Notify(addr, ui.Notification{Account: "a2"}, click); err != nil { // id 41 again, a2
		t.Fatal(err)
	}
	emit(t, sc2, "ActionInvoked", uint32(42), "default") // 42 was the old server's: stale
	emit(t, sc2, "ActionInvoked", uint32(41), "default")
	waitFor(t, "click", func() bool { return count() >= 1 })
	time.Sleep(100 * time.Millisecond)
	if count() != 1 {
		t.Fatal("stale entry fired")
	}
}

func TestNotifyNilOnClick(t *testing.T) {
	addr := dbustest.Start(t)
	_, sc := startFakeNotifier(t, addr)
	t.Cleanup(closeNotifier)
	if err := Notify(addr, ui.Notification{Account: "a1"}, nil); err != nil {
		t.Fatal(err)
	}
	emit(t, sc, "ActionInvoked", uint32(41), "default") // must not panic
	time.Sleep(100 * time.Millisecond)
}

func TestNotifyWithoutBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	if err := Notify("", ui.Notification{Title: "x"}, func(string) {}); err == nil {
		t.Fatal("Notify with no bus must fail")
	}
}

func TestTrayHostFalseWithoutWatcher(t *testing.T) {
	addr := dbustest.Start(t)
	if TrayHost(addr) {
		t.Fatal("no watcher on the bus, TrayHost must be false")
	}
	conn, err := dbus.Connect(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if r, err := conn.RequestName("org.kde.StatusNotifierWatcher", dbus.NameFlagDoNotQueue); err != nil || r != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal(r, err)
	}
	if !TrayHost(addr) {
		t.Fatal("a watcher owns its name, TrayHost must be true")
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	if TrayHost("") {
		t.Fatal("no bus at all: false")
	}
}

func TestAutostartWritesDesktopFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	path := filepath.Join(cfg, "autostart", "pcb-ui.desktop")
	if Autostart() {
		t.Fatal("autostart on before it was set")
	}
	if err := SetAutostart(true, "/opt/pcb/bin/pcb-ui"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\nExec=/opt/pcb/bin/pcb-ui --background\n") || !strings.HasPrefix(string(b), "[Desktop Entry]\n") {
		t.Fatalf("desktop file:\n%s", b)
	}
	if !Autostart() {
		t.Fatal("Autostart() false after SetAutostart(true)")
	}
	if err := SetAutostart(false, "/opt/pcb/bin/pcb-ui"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}
	if Autostart() {
		t.Fatal("Autostart() true after off")
	}
	if err := SetAutostart(false, ""); err != nil {
		t.Fatalf("off twice: %v", err)
	}
}

func TestAutostartDefaultDirAndQuoting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	if err := SetAutostart(true, "/opt/my apps/pcb-ui"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "autostart", "pcb-ui.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "\nExec=\"/opt/my apps/pcb-ui\" --background\n") {
		t.Fatalf("a path with a space must be quoted:\n%s", b)
	}
	if err := SetAutostart(true, "relative/pcb-ui"); err == nil {
		t.Fatal("a relative exe must be refused")
	}
}

func TestAutostartExecEscaping(t *testing.T) {
	cases := map[string]string{
		`/o/a\b c`: `Exec="/o/a\\\\b c" --background`,
		`/o/q"x y`: `Exec="/o/q\\"x y" --background`,
		"/o/b`x y": "Exec=\"/o/b\\\\`x y\" --background",
		`/o/$x y`:  `Exec="/o/\\$x y" --background`,
		`/o/100%`:  `Exec="/o/100%%" --background`,
	}
	for exe, want := range cases {
		cfg := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", cfg)
		if err := SetAutostart(true, exe); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(cfg, "autostart", "pcb-ui.desktop"))
		if !strings.Contains(string(b), "\n"+want+"\n") {
			t.Errorf("exe %q: want line %s in\n%s", exe, want, b)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := SetAutostart(true, "/o/a\nExec=evil"); err == nil {
		t.Fatal("a newline in the path must be refused")
	}
}

func TestAutostartDirPrivateAndOffKeepsOthers(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	if err := SetAutostart(true, "/opt/pcb-ui"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg, "autostart")
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("autostart dir: %v %v", fi, err)
	}
	other := filepath.Join(dir, "other.desktop")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetAutostart(false, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("off removed another file: %v", err)
	}
}
