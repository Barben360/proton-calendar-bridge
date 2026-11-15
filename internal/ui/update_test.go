package ui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

// prefs is Fyne's preferences in memory.
type prefs struct {
	mu sync.Mutex
	m  map[string]string
}

func (p *prefs) String(k string) string { p.mu.Lock(); defer p.mu.Unlock(); return p.m[k] }
func (p *prefs) SetString(k, v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]string{}
	}
	p.m[k] = v
}

// gh is a fake github.com whose latest tag can change; it counts requests.
type gh struct {
	srv  *httptest.Server
	n    atomic.Int32
	tag  atomic.Value
	fail atomic.Bool
}

func newGH(t *testing.T) *gh {
	t.Helper()
	g := &gh{}
	g.tag.Store("v0.2.0")
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.n.Add(1)
		if g.fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Location", g.srv.URL+"/Barben360/proton-calendar-bridge/releases/tag/"+g.tag.Load().(string))
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// clock is a settable now.
type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time      { return time.Unix(0, c.t.Load()) }
func (c *clock) add(d time.Duration) { c.t.Add(int64(d)) }
func newClock() *clock {
	c := &clock{}
	c.t.Store(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func noEnv(string) string { return "" }
func (g *gh) releasePage(t string) string {
	return g.srv.URL + "/Barben360/proton-calendar-bridge/releases/tag/" + t
}

func newUpdates(t *testing.T, g *gh, installed string, c *clock) *ui.Updates {
	t.Helper()
	return &ui.Updates{
		Checker: update.Checker{URL: g.srv.URL + "/latest", Installed: installed},
		Path:    update.StorePath(t.TempDir()), Env: noEnv, Prefs: &prefs{}, Hint: "the hint", Now: c.now,
	}
}

func TestUpdatesShowsAnUpdateAndNotifiesOnce(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	n, note := u.Refresh(context.Background())
	want := ui.UpdateNotice{Available: true, Panel: true, Version: "v0.2.0", Installed: "v0.1.0", URL: g.releasePage("v0.2.0"), Hint: "the hint"}
	if n != want {
		t.Errorf("notice = %+v\nwant     %+v", n, want)
	}
	if note == nil || note.Title != "pcb" || note.Body != "pcb v0.2.0 is available (you have v0.1.0)." {
		t.Errorf("notification = %+v", note)
	}
	if _, note := u.Refresh(context.Background()); note != nil {
		t.Errorf("second refresh notified again: %+v", note)
	}
}

func TestUpdatesUpToDate(t *testing.T) {
	g, c := newGH(t), newClock()
	if n, note := newUpdates(t, g, "v0.2.0", c).Refresh(context.Background()); n != (ui.UpdateNotice{}) || note != nil {
		t.Errorf("up to date: %+v, %+v", n, note)
	}
}

func TestUpdatesLaterHidesThePanelFor48Hours(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	u.Refresh(context.Background())
	u.Later()
	if n, _ := u.Refresh(context.Background()); n.Panel || !n.Available {
		t.Errorf("right after Later: %+v, want available without the panel", n)
	}
	c.add(48*time.Hour - time.Second)
	if n, _ := u.Refresh(context.Background()); n.Panel {
		t.Errorf("before 48 h: panel shown")
	}
	c.add(2 * time.Second)
	n, note := u.Refresh(context.Background())
	if !n.Panel || n.Version != "v0.2.0" {
		t.Errorf("after 48 h: %+v, want the panel back for the same version", n)
	}
	if note != nil {
		t.Errorf("after 48 h: notified again: %+v", note)
	}
}

// Later survives a restart: it is in the preferences, not in memory.
func TestUpdatesLaterSurvivesARestart(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	u.Refresh(context.Background())
	u.Later()
	again := *u
	if n, _ := (&again).Refresh(context.Background()); n.Panel {
		t.Errorf("after a restart within 48 h: panel shown")
	}
}

func TestUpdatesNewerVersionDuringLater(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	u.Refresh(context.Background())
	u.Later()
	g.tag.Store("v0.3.0")
	c.add(25 * time.Hour) // past the cache, inside Later
	n, note := u.Refresh(context.Background())
	if n.Panel || n.Version != "v0.3.0" {
		t.Errorf("newer during Later: %+v, want v0.3.0 without the panel", n)
	}
	if note == nil || note.Body != "pcb v0.3.0 is available (you have v0.1.0)." {
		t.Errorf("newer during Later: notification %+v, want one for v0.3.0", note)
	}
}

func TestUpdatesSwitchedOff(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	if n, _ := u.Refresh(context.Background()); !n.Panel {
		t.Fatalf("on: %+v", n)
	}
	if !u.Enabled() {
		t.Fatal("Enabled() = false before switching off")
	}
	if err := u.SetEnabled(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if u.Enabled() {
		t.Error("Enabled() = true after switching off")
	}
	if n, note := u.Refresh(context.Background()); n != (ui.UpdateNotice{}) || note != nil {
		t.Errorf("off: %+v, %+v, want nothing", n, note)
	}
	env := newUpdates(t, g, "v0.1.0", c)
	env.Env = func(k string) string {
		if k == "PCB_UPDATE_CHECK" {
			return "off"
		}
		return ""
	}
	before := g.n.Load()
	if n, _ := env.Refresh(context.Background()); n != (ui.UpdateNotice{}) || g.n.Load() != before {
		t.Errorf("env off: %+v, %d new requests", n, g.n.Load()-before)
	}
}

func TestUpdatesDevBuildSendsNothing(t *testing.T) {
	g, c := newGH(t), newClock()
	if n, note := newUpdates(t, g, "dev", c).Refresh(context.Background()); n != (ui.UpdateNotice{}) || note != nil || g.n.Load() != 0 {
		t.Errorf("dev: %+v, %+v, %d requests", n, note, g.n.Load())
	}
}

func TestUpdatesRunShowsAtStartAndOnPoke(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shown := make(chan ui.UpdateNotice, 4)
	notes := make(chan string, 4)
	poke := make(chan struct{})
	go u.Run(ctx, time.Hour, poke, func(n ui.UpdateNotice) { shown <- n }, func(_ ui.Notification, url string) { notes <- url })
	wait := func(what string) ui.UpdateNotice {
		select {
		case n := <-shown:
			return n
		case <-time.After(5 * time.Second):
			t.Fatalf("no show %s", what)
		}
		return ui.UpdateNotice{}
	}
	if n := wait("at start"); !n.Panel {
		t.Errorf("start: %+v", n)
	}
	select {
	case url := <-notes:
		if url != g.releasePage("v0.2.0") {
			t.Errorf("notification URL = %q", url)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification at start")
	}
	u.Later()
	poke <- struct{}{}
	if n := wait("after the poke"); n.Panel {
		t.Errorf("after Later and a poke: %+v, want no panel", n)
	}
}

// Offline after the cache expired: the last answer stays on screen rather
// than the panel coming and going with the network.
func TestUpdatesKeepsTheLastAnswerWhenACheckFails(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	u.Refresh(context.Background())
	g.fail.Store(true)
	c.add(25 * time.Hour)
	if n, _ := u.Refresh(context.Background()); !n.Panel || n.Version != "v0.2.0" {
		t.Errorf("failed re-check: %+v, want the last answer", n)
	}
}

// The offline fallback reads update.json too: a page on another host there
// is never shown.
func TestUpdatesFallbackDoesNotTrustAnotherHost(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	if err := update.Edit(context.Background(), u.Path, func(s *update.State) {
		s.CheckedAt, s.Latest, s.URL = c.now().Add(-25*time.Hour), "v0.2.0", "https://evil.example/Barben360/proton-calendar-bridge/releases/tag/v0.2.0"
	}); err != nil {
		t.Fatal(err)
	}
	g.fail.Store(true)
	if n, note := u.Refresh(context.Background()); n != (ui.UpdateNotice{}) || note != nil {
		t.Errorf("tampered cache while offline: %+v, %+v, want nothing", n, note)
	}
}

// Later pressed while the clock ran a year ahead: once the clock is right
// again, the saved deadline is more than 48 h away and does not count.
func TestUpdatesIgnoresALaterDeadlineTooFarAhead(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	u.Refresh(context.Background())
	c.add(365 * 24 * time.Hour)
	u.Later()
	c.add(-365 * 24 * time.Hour)
	if n, _ := u.Refresh(context.Background()); !n.Panel {
		t.Errorf("deadline a year ahead: %+v, want the panel shown", n)
	}
}
