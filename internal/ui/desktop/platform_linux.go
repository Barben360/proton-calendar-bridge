//go:build linux

package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

const (
	busTimeout = 2 * time.Second
	notifBus   = "org.freedesktop.Notifications"
	notifPath  = dbus.ObjectPath("/org/freedesktop/Notifications")
	watcherBus = "org.kde.StatusNotifierWatcher"
)

// dial connects to the bus at addr ("" is DBUS_SESSION_BUS_ADDRESS) within
// busTimeout (the connection itself outlives the timeout). It never falls back to dbus-launch.
func dial(addr string) (*dbus.Conn, error) {
	if addr == "" {
		addr = os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	}
	if addr == "" {
		return nil, errors.New("DBUS_SESSION_BUS_ADDRESS is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	defer cancel()
	type result struct {
		c   *dbus.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dbus.Connect(addr)
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() { // close a late connection
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, errors.New("the session bus did not answer")
	}
}

// TrayHost reports whether a tray host (org.kde.StatusNotifierWatcher) is on
// the session bus at addr ("" is DBUS_SESSION_BUS_ADDRESS).
func TrayHost(addr string) bool {
	c, err := dial(addr)
	if err != nil {
		return false
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	defer cancel()
	var has bool
	if err := c.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, watcherBus).Store(&has); err != nil {
		return false
	}
	return has
}

type pending struct {
	account string
	onClick func(account string)
}

// notifier is the connection to one bus: it keeps the ids of the
// notifications it sent, to route a click to its account.
type notifier struct {
	conn *dbus.Conn
	mu   sync.Mutex // ids (held across a Notify call)
	ids  map[uint32]pending

	omu   sync.Mutex
	owner string // unique name of the notification server's owner, "" if none
}

// setOwner records the server's owner; a change forgets every id (they
// belonged to the previous server).
func (n *notifier) setOwner(o string) {
	n.omu.Lock()
	changed := n.owner != o
	n.owner = o
	n.omu.Unlock()
	if changed {
		n.mu.Lock()
		clear(n.ids)
		n.mu.Unlock()
	}
}

func (n *notifier) isOwner(sender string) bool {
	n.omu.Lock()
	defer n.omu.Unlock()
	return n.owner != "" && sender == n.owner
}

// resolveOwner asks the bus who owns org.freedesktop.Notifications.
func (n *notifier) resolveOwner(ctx context.Context) error {
	var o string
	if err := n.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, notifBus).Store(&o); err != nil {
		return fmt.Errorf("no notification server: %w", err)
	}
	n.setOwner(o)
	return nil
}

var (
	notifMu sync.Mutex
	// notifiers by bus address ("" resolved to the environment's).
	notifiers = map[string]*notifier{}
)

func busKey(addr string) string {
	if addr == "" {
		return os.Getenv("DBUS_SESSION_BUS_ADDRESS")
	}
	return addr
}

// closeNotifier drops every connection (tests).
func closeNotifier() {
	notifMu.Lock()
	defer notifMu.Unlock()
	for k, n := range notifiers {
		_ = n.conn.Close()
		delete(notifiers, k)
	}
}

func getNotifier(addr string) (*notifier, error) {
	notifMu.Lock()
	defer notifMu.Unlock()
	key := busKey(addr)
	if n, ok := notifiers[key]; ok {
		return n, nil
	}
	c, err := dial(addr)
	if err != nil {
		return nil, err
	}
	if err := c.AddMatchSignal(dbus.WithMatchInterface(notifBus), dbus.WithMatchObjectPath(notifPath)); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, notifBus)); err != nil {
		_ = c.Close()
		return nil, err
	}
	n := &notifier{conn: c, ids: map[uint32]pending{}}
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	_ = n.resolveOwner(ctx) // none yet: Notify retries and fails with it
	cancel()
	sig := make(chan *dbus.Signal, 16)
	c.Signal(sig)
	go func() {
		for s := range sig {
			n.handle(s)
		}
		notifMu.Lock()
		if notifiers[key] == n {
			delete(notifiers, key) // the connection ended: the next Notify dials again
		}
		notifMu.Unlock()
	}()
	notifiers[key] = n
	return n, nil
}

func (n *notifier) handle(s *dbus.Signal) {
	switch s.Name {
	case "org.freedesktop.DBus.NameOwnerChanged":
		if s.Sender != "org.freedesktop.DBus" || len(s.Body) < 3 {
			return
		}
		if name, _ := s.Body[0].(string); name == notifBus {
			o, _ := s.Body[2].(string)
			n.setOwner(o)
		}
	case notifBus + ".ActionInvoked":
		if !n.isOwner(s.Sender) || len(s.Body) < 2 {
			return
		}
		id, ok1 := s.Body[0].(uint32)
		key, ok2 := s.Body[1].(string)
		if !ok1 || !ok2 || key != "default" {
			return
		}
		n.mu.Lock()
		p, ok := n.ids[id]
		n.mu.Unlock()
		if ok {
			p.onClick(p.account)
		}
	case notifBus + ".NotificationClosed":
		if !n.isOwner(s.Sender) || len(s.Body) < 1 {
			return
		}
		if id, ok := s.Body[0].(uint32); ok {
			n.mu.Lock()
			delete(n.ids, id)
			n.mu.Unlock()
		}
	}
}

// Notify shows n on the desktop through org.freedesktop.Notifications on the
// session bus at addr ("" is DBUS_SESSION_BUS_ADDRESS). A click calls
// onClick (nil: none) with n.Account, from a goroutine of its own (go through
// fyne.Do). Only signals from the server's own connection count.
func Notify(addr string, n ui.Notification, onClick func(account string)) error {
	if onClick == nil {
		onClick = func(string) {}
	}
	nt, err := getNotifier(addr)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), busTimeout)
	defer cancel()
	if err := nt.resolveOwner(ctx); err != nil {
		return err
	}
	// Held across the call so that a click arriving right after the reply
	// finds its id.
	nt.mu.Lock()
	defer nt.mu.Unlock()
	var id uint32
	err = nt.conn.Object(notifBus, notifPath).CallWithContext(ctx, notifBus+".Notify", 0,
		"pcb", uint32(0), "pcb-ui", n.Title, n.Body, []string{"default", "Open"}, map[string]dbus.Variant{}, int32(-1)).Store(&id)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	nt.ids[id] = pending{account: n.Account, onClick: onClick}
	return nil
}

// autostartPath is $XDG_CONFIG_HOME/autostart/pcb-ui.desktop.
func autostartPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" || !filepath.IsAbs(dir) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "pcb-ui.desktop"), nil
}

// execArg quotes a path for a desktop entry's Exec key when it needs it.
func execArg(p string) string {
	if !strings.ContainsAny(p, " \t\"'\\<>~|&;$*?#()`%") {
		return p
	}
	r := strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", "$", `\\$`, "%", "%%")
	return `"` + r.Replace(p) + `"`
}

// SetAutostart writes (on) or removes the autostart entry that starts exe
// with --background at login.
func SetAutostart(on bool, exe string) error {
	path, err := autostartPath()
	if err != nil {
		return err
	}
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if !filepath.IsAbs(exe) {
		return fmt.Errorf("autostart needs the absolute path of pcb-ui, got %q", exe)
	}
	if strings.ContainsAny(exe, "\n\r") {
		return errors.New("autostart: the path of pcb-ui holds a line break")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	content := "[Desktop Entry]\nType=Application\nName=pcb\nComment=Proton calendar bridge\n" +
		"Exec=" + execArg(exe) + " --background\nIcon=pcb-ui\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
	return os.WriteFile(path, []byte(content), 0o644)
}

// Autostart reports whether the autostart entry exists.
func Autostart() bool {
	path, err := autostartPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
