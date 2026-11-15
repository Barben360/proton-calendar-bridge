//go:build (linux || windows) && cgo

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
	"github.com/Barben360/proton-calendar-bridge/internal/ui/desktop"
	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

// memPrefs is Fyne's preferences in memory.
type memPrefs struct {
	mu sync.Mutex
	m  map[string]string
}

func (p *memPrefs) String(k string) string { p.mu.Lock(); defer p.mu.Unlock(); return p.m[k] }
func (p *memPrefs) SetString(k, v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]string{}
	}
	p.m[k] = v
}

// fakeWindow records what followUpdates draws and opens.
type fakeWindow struct {
	mu      sync.Mutex
	notices []ui.UpdateNotice
	later   func()
	opened  []string
}

func (w *fakeWindow) SetUpdate(n ui.UpdateNotice, later func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.notices, w.later = append(w.notices, n), later
}

func (w *fakeWindow) OpenURL(raw string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.opened = append(w.opened, raw)
}

func (w *fakeWindow) seen() []ui.UpdateNotice {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]ui.UpdateNotice(nil), w.notices...)
}

type fakeTray struct {
	mu      sync.Mutex
	notices []ui.UpdateNotice
}

func (t *fakeTray) SetUpdate(n ui.UpdateNotice) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notices = append(t.notices, n)
}

func (t *fakeTray) seen() []ui.UpdateNotice {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]ui.UpdateNotice(nil), t.notices...)
}

// fakeNotifier records each notification and its click callback.
type fakeNotifier struct {
	mu     sync.Mutex
	notes  []ui.Notification
	clicks []func(string)
	// failures is how many of the next Notify calls return an error.
	failures int
}

func (f *fakeNotifier) Notify(note ui.Notification, onClick func(string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notes, f.clicks = append(f.notes, note), append(f.clicks, onClick)
	if f.failures > 0 {
		f.failures--
		return errors.New("no notification service")
	}
	return nil
}

func (f *fakeNotifier) sent() ([]ui.Notification, []func(string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ui.Notification(nil), f.notes...), append([]func(string){}, f.clicks...)
}

// waitFor polls cond until it holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// followRig runs followUpdates against a fake github.com whose latest
// release is v0.2.0 and the installed version v0.1.0.
type followRig struct {
	win   *fakeWindow
	notif *fakeNotifier
	poke  chan struct{}
	page  string
}

func startFollow(t *testing.T, tray updateTray, failures int) *followRig {
	t.Helper()
	test.NewApp() // fyne.Do needs a current app; the test driver runs it inline
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", srv.URL+"/Barben360/proton-calendar-bridge/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	r := &followRig{win: &fakeWindow{}, notif: &fakeNotifier{failures: failures}, poke: make(chan struct{}, 1),
		page: srv.URL + "/Barben360/proton-calendar-bridge/releases/tag/v0.2.0"}
	u := &ui.Updates{
		Checker: update.Checker{URL: srv.URL + "/latest", Installed: "v0.1.0"},
		Path:    update.StorePath(t.TempDir()), Env: func(string) string { return "" },
		Prefs: &memPrefs{}, Now: time.Now,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		followUpdates(ctx, u, r.poke, r.win, tray, r.notif, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func TestFollowUpdatesDrawsTheNoticeEverywhereAndNotifiesOnce(t *testing.T) {
	tray := &fakeTray{}
	r := startFollow(t, tray, 0)

	waitFor(t, "the notice in the window and the tray", func() bool { return len(r.win.seen()) >= 1 && len(tray.seen()) >= 1 })
	for name, n := range map[string]ui.UpdateNotice{"window": r.win.seen()[0], "tray": tray.seen()[0]} {
		if !n.Available || !n.Panel || n.Version != "v0.2.0" || n.URL != r.page {
			t.Errorf("%s notice = %+v, want an available v0.2.0 with the panel and %s", name, n, r.page)
		}
	}
	// Run shows the notice, then notifies: wait for the notification.
	waitFor(t, "the notification", func() bool { n, _ := r.notif.sent(); return len(n) >= 1 })
	notes, clicks := r.notif.sent()
	if len(notes) != 1 || notes[0].Code != "update-available" {
		t.Fatalf("notifications = %+v, want exactly one update-available", notes)
	}

	// Later: the panel goes, the tray line stays, and no second notification.
	r.win.mu.Lock()
	later := r.win.later
	r.win.mu.Unlock()
	if later == nil {
		t.Fatal("the window got no Later callback")
	}
	later()
	waitFor(t, "the refresh after Later", func() bool { return len(r.win.seen()) >= 2 && len(tray.seen()) >= 2 })
	if n := r.win.seen()[1]; n.Panel || !n.Available {
		t.Errorf("window after Later = %+v, want the panel hidden", n)
	}
	if n := tray.seen()[1]; n.Panel || !n.Available {
		t.Errorf("tray after Later = %+v, want the line kept, the panel flag off", n)
	}
	// Run shows, then notifies, in one iteration: once a third notice is
	// drawn, the iteration that drew the second has finished notifying.
	pokeLoop(r.poke)
	waitFor(t, "a third refresh", func() bool { return len(r.win.seen()) >= 3 })
	if notes, _ := r.notif.sent(); len(notes) != 1 {
		t.Errorf("%d notifications after Later and a refresh, want still 1", len(notes))
	}

	// A click on the notification opens the release page.
	clicks[0]("")
	r.win.mu.Lock()
	opened := append([]string(nil), r.win.opened...)
	r.win.mu.Unlock()
	if len(opened) != 1 || opened[0] != r.page {
		t.Errorf("opened %v after the click, want [%s]", opened, r.page)
	}
}

func TestFollowUpdatesWithoutATray(t *testing.T) {
	r := startFollow(t, nil, 0) // no tray host: an untyped nil, as main passes it
	waitFor(t, "the notice in the window", func() bool { return len(r.win.seen()) >= 1 })
	if n := r.win.seen()[0]; !n.Panel || n.Version != "v0.2.0" {
		t.Errorf("window notice = %+v", n)
	}
	waitFor(t, "the notification", func() bool { n, _ := r.notif.sent(); return len(n) == 1 })
}

// A notification that failed is sent again at the next refresh, and once it
// went through, never again: followUpdates must hand the notifier's error
// back to Run, which marks the version notified only on success.
func TestFollowUpdatesResendsAFailedNotificationOnce(t *testing.T) {
	r := startFollow(t, &fakeTray{}, 1)
	waitFor(t, "the failed notification", func() bool { n, _ := r.notif.sent(); return len(n) >= 1 })
	// Each poke forces a pass; wait for the notice it draws, so the
	// pass before it has finished notifying.
	seen := len(r.win.seen())
	pokeLoop(r.poke)
	waitFor(t, "a second refresh", func() bool { return len(r.win.seen()) > seen })
	waitFor(t, "the resent notification", func() bool { n, _ := r.notif.sent(); return len(n) >= 2 })
	seen = len(r.win.seen())
	pokeLoop(r.poke)
	waitFor(t, "a third refresh", func() bool { return len(r.win.seen()) > seen })
	seen = len(r.win.seen())
	pokeLoop(r.poke)
	waitFor(t, "a fourth refresh", func() bool { return len(r.win.seen()) > seen })
	if n, _ := r.notif.sent(); len(n) != 2 {
		t.Errorf("%d notifications, want 2 (one failed, one resent, none after)", len(n))
	}
}

func TestTrayOrNil(t *testing.T) {
	if got := trayOrNil(nil); got != nil {
		t.Errorf("trayOrNil(nil) = %#v, want a nil interface (followUpdates tests it against nil)", got)
	}
	tray := &desktop.Tray{}
	if got := trayOrNil(tray); got != updateTray(tray) {
		t.Errorf("trayOrNil(tray) = %#v, want the tray itself", got)
	}
}
