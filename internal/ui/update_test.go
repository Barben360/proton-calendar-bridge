package ui_test

import (
	"context"
	"errors"
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
	// gate, when set, holds each request until it is closed (a slow GitHub).
	gate atomic.Pointer[chan struct{}]
}

func newGH(t *testing.T) *gh {
	t.Helper()
	g := &gh{}
	g.tag.Store("v0.2.0")
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		g.n.Add(1)
		if gate := g.gate.Load(); gate != nil {
			<-*gate
		}
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
	if _, again := u.Refresh(context.Background()); again == nil {
		t.Errorf("refresh before MarkNotified: no notification, want it offered again (not yet delivered)")
	}
	u.MarkNotified("v0.2.0")
	if _, note := u.Refresh(context.Background()); note != nil {
		t.Errorf("refresh after MarkNotified notified again: %+v", note)
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
	u.MarkNotified("v0.2.0")
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
	go u.Run(ctx, time.Hour, poke, func(n ui.UpdateNotice) { shown <- n }, func(_ ui.Notification, url string) error { notes <- url; return nil })
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

// A notification that failed to send is not marked: it comes again at the
// next refresh, and stops once one is delivered.
func TestUpdatesRunRetriesAFailedNotificationUntilDelivered(t *testing.T) {
	g, c := newGH(t), newClock()
	u := newUpdates(t, g, "v0.1.0", c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shown := make(chan struct{}, 8)
	attempts := make(chan error, 8)
	failures := 1
	poke := make(chan struct{})
	go u.Run(ctx, time.Hour, poke, func(ui.UpdateNotice) { shown <- struct{}{} }, func(ui.Notification, string) error {
		var err error
		if failures > 0 {
			failures--
			err = errors.New("D-Bus: no notification server")
		}
		attempts <- err
		return err
	})
	cycle := func(what string) {
		t.Helper()
		select {
		case <-shown:
		case <-time.After(5 * time.Second):
			t.Fatalf("no refresh %s", what)
		}
	}
	attempt := func(what string) error {
		t.Helper()
		select {
		case err := <-attempts:
			return err
		case <-time.After(5 * time.Second):
			t.Fatalf("no notification %s", what)
		}
		return nil
	}
	cycle("at start")
	if err := attempt("at start"); err == nil {
		t.Fatal("first attempt should have failed")
	}
	poke <- struct{}{}
	cycle("after the first poke")
	if err := attempt("after the failed one"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	poke <- struct{}{}
	cycle("after the second poke")
	select {
	case err := <-attempts:
		t.Errorf("notified again after a delivered one (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
}

// slowRun starts Run against a GitHub that holds its answer until release.
// shown gets every show; notes every notification's URL; poke is buffered
// as the settings pane's is (a poke never blocks).
type slowRun struct {
	g       *gh
	u       *ui.Updates
	shown   chan ui.UpdateNotice
	notes   chan string
	poke    chan struct{}
	release func()
}

func startSlowRun(t *testing.T) *slowRun {
	t.Helper()
	g, c := newGH(t), newClock()
	gate := make(chan struct{})
	g.gate.Store(&gate)
	var once sync.Once
	r := &slowRun{g: g, u: newUpdates(t, g, "v0.1.0", c),
		shown: make(chan ui.UpdateNotice, 16), notes: make(chan string, 16), poke: make(chan struct{}, 1),
		release: func() { once.Do(func() { close(gate) }) }}
	t.Cleanup(r.release) // a failed run must not leave the server blocked
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.u.Run(ctx, time.Hour, r.poke, func(n ui.UpdateNotice) { r.shown <- n }, func(_ ui.Notification, url string) error { r.notes <- url; return nil })
	deadline := time.Now().Add(30 * time.Second)
	for g.n.Load() == 0 { // the check is in flight, blocked on GitHub
		if time.Now().After(deadline) {
			t.Fatal("the check never reached GitHub")
		}
		time.Sleep(time.Millisecond)
	}
	return r
}

func (r *slowRun) nextShow(t *testing.T, what string) ui.UpdateNotice {
	t.Helper()
	select {
	case n := <-r.shown:
		return n
	case <-time.After(5 * time.Second):
		t.Fatalf("no show %s", what)
	}
	return ui.UpdateNotice{}
}

func (r *slowRun) setEnabled(t *testing.T, on bool) {
	t.Helper()
	if err := r.u.SetEnabled(context.Background(), on); err != nil {
		t.Fatal(err)
	}
	r.poke <- struct{}{}
}

// The switch is turned off while a check waits on GitHub: the notice is
// cleared at once (not after the check), the late answer is neither shown
// nor notified, and its version is not marked: switched on again, the
// notification still comes.
func TestUpdatesRunSwitchOffDuringACheckClearsAtOnceAndDropsTheResult(t *testing.T) {
	r := startSlowRun(t)
	r.setEnabled(t, false)
	if n := r.nextShow(t, "while the check was in flight"); n != (ui.UpdateNotice{}) {
		t.Fatalf("after the switch-off: %+v, want the empty notice", n)
	}
	r.release() // the late answer arrives: v0.2.0
	// The shows that follow the release come from the dropped iteration and
	// from the pass after it (the switch-off poke asked for one): once the
	// second is seen, a notify in the first would have been sent.
	for i := 0; i < 2; i++ {
		if n := r.nextShow(t, "after the dropped check"); n != (ui.UpdateNotice{}) {
			t.Fatalf("shown after the switch-off: %+v", n)
		}
	}
	select {
	case url := <-r.notes:
		t.Fatalf("notified %q after the switch-off", url)
	default:
	}
	if v := r.u.Prefs.String("updateNotified"); v != "" {
		t.Fatalf("version %q marked notified by a dropped check", v)
	}
	r.setEnabled(t, true)
	for n := r.nextShow(t, "after switching on"); !n.Panel; n = r.nextShow(t, "after switching on") {
	}
	select {
	case url := <-r.notes:
		if url != r.g.releasePage("v0.2.0") {
			t.Errorf("notification URL = %q", url)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notification of the dropped version was lost")
	}
}

// Off then on again during one check: the check's answer is still wanted.
func TestUpdatesRunSwitchOffAndOnDuringACheckShowsTheResult(t *testing.T) {
	r := startSlowRun(t)
	r.setEnabled(t, false)
	if n := r.nextShow(t, "after the switch-off"); n != (ui.UpdateNotice{}) {
		t.Fatalf("after the switch-off: %+v", n)
	}
	r.setEnabled(t, true)
	r.release()
	if n := r.nextShow(t, "after the check"); !n.Panel {
		t.Fatalf("after off and on: %+v, want the panel", n)
	}
	select {
	case <-r.notes:
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	// The pass that follows the pokes ends with a show: once seen, the
	// version was offered again and must not have been notified twice.
	if n := r.nextShow(t, "after the follow-up pass"); !n.Panel {
		t.Fatalf("follow-up pass: %+v", n)
	}
	select {
	case url := <-r.notes:
		t.Errorf("notified twice (%q)", url)
	default:
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
