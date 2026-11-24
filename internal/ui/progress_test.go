package ui_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

func intp(n int) *int { return &n }

// The stale check leaves a margin over the interval: a pass ends a little
// more than one interval after the previous one, and the window must not
// show "syncing…" in between.
func TestStaleCheckHasAMargin(t *testing.T) {
	for _, tc := range []struct {
		age   time.Duration
		want  ui.TrayState
		short string
	}{
		{31 * time.Second, ui.TrayOK, "synced just now"}, // the usual gap: interval + the pass's time
		{45 * time.Second, ui.TrayOK, "synced just now"}, // interval + half of it: still on time
		{46 * time.Second, ui.TraySyncing, "syncing…"},   // past the margin
		{2 * time.Minute, ui.TraySyncing, "syncing…"},    // late
	} {
		v := ui.Build(status(ready("a", tc.age, 30)), now)
		if v.Tray != tc.want || v.Accounts[0].Short != tc.short {
			t.Errorf("last pass %v ago, interval 30 s: tray %q, %q; want %q, %q", tc.age, v.Tray, v.Accounts[0].Short, tc.want, tc.short)
		}
	}
}

// running is a signed-in account on time whose pass p runs.
func running(p service.PassProgress) ui.View {
	a := ready("a", 10*time.Second, 30)
	a.Health.Pass = &p
	return ui.Build(status(a), now)
}

// A pass shows nothing for ProgressDelay; the window still redraws every
// second meanwhile, so it shows the pass as soon as the delay is over.
func TestQuickPassShowsNoProgress(t *testing.T) {
	v := running(service.PassProgress{StartedAt: *ago(ui.ProgressDelay - time.Millisecond), Calendar: 1, Calendars: 2, Mode: "incremental"})
	if a := v.Accounts[0]; a.Progress != nil || a.Dot != ui.TrayOK || a.Short != "synced just now" {
		t.Errorf("pass under the delay: progress %+v, dot %q, %q; want none, ok, synced", a.Progress, a.Dot, a.Short)
	}
	if !v.PassRunning || v.RefreshAfter() != ui.ProgressTick {
		t.Errorf("pass under the delay: running %v, refresh %v; want true, %v", v.PassRunning, v.RefreshAfter(), ui.ProgressTick)
	}
	at := running(service.PassProgress{StartedAt: *ago(ui.ProgressDelay), Calendar: 1, Calendars: 2, Mode: "incremental"})
	if at.Accounts[0].Progress == nil {
		t.Error("pass running for exactly ProgressDelay: no progress shown")
	}
	other := ready("b", 10*time.Second, 30)
	busy := ready("a", 10*time.Second, 30)
	busy.Health.Pass = &service.PassProgress{StartedAt: *ago(time.Second), Calendars: 1}
	if two := ui.Build(status(busy, other), now); !two.PassRunning {
		t.Error("a pass on the first of two accounts: no redraw every second")
	}
	if two := ui.Build(status(other, busy), now); !two.PassRunning {
		t.Error("a pass on the second of two accounts: no redraw every second")
	}
	idle := ui.Build(status(ready("a", 10*time.Second, 30)), now)
	if idle.PassRunning || idle.RefreshAfter() != 0 {
		t.Errorf("no pass: running %v, refresh %v; want false, 0", idle.PassRunning, idle.RefreshAfter())
	}
}

// A long pass shows what it does, where, how far and for how long. A pass
// that lists calendars turns the account to syncing (without a percentage,
// which would rebuild the tray's menu at every page); one that only reads
// change feeds leaves it as it was.
func TestRunningPassProgress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		p        service.PassProgress
		text     string
		fraction float64
		short    string
		dot      ui.TrayState
	}{
		{"daily full listing", service.PassProgress{StartedAt: *ago(72 * time.Second), Calendar: 1, Calendars: 4, CalendarName: "Perso",
			Mode: "full", LastFullAt: ago(32*time.Hour + 72*time.Second), EventsFetched: 412, EventsExpected: intp(1030)},
			"Full sync (last one 32 h ago) · Perso (1/4) · 412 of 1030 events · 1 min 12 s", 0.4, "syncing…", ui.TraySyncing},
		{"first sync, no estimate", service.PassProgress{StartedAt: *ago(5 * time.Second), Calendar: 2, Calendars: 3, CalendarName: "Sport",
			Mode: "initial", EventsFetched: 300},
			"First sync · Sport (2/3) · 300 events · 5 s", -1, "syncing…", ui.TraySyncing},
		{"slow incremental pass", service.PassProgress{StartedAt: *ago(3 * time.Second), Calendar: 2, Calendars: 2, CalendarName: "Sport",
			Mode: "incremental", EventsExpected: intp(0)},
			"Checking for changes · Sport (2/2) · 3 s", -1, "synced just now", ui.TrayOK},
		{"incremental calendar of a pass that listed another", service.PassProgress{StartedAt: *ago(40 * time.Second), Calendar: 2, Calendars: 2, CalendarName: "Sport",
			Mode: "incremental", EventsFetched: 150, EventsExpected: intp(150)},
			"Checking for changes · Sport (2/2) · 150 of 150 events · 40 s", 1, "syncing…", ui.TraySyncing},
		{"before the calendars are known", service.PassProgress{StartedAt: *ago(2 * time.Hour)},
			"Starting sync · 2 h 0 min", -1, "synced just now", ui.TrayOK},
		{"before the first calendar", service.PassProgress{StartedAt: *ago(3 * time.Second), Calendars: 2, EventsExpected: intp(170)},
			"Starting sync · 0 of 170 events · 3 s", 0, "syncing…", ui.TraySyncing},
		{"unnamed calendar, listed again within the minute", service.PassProgress{StartedAt: *ago(4 * time.Second), Calendar: 1, Calendars: 3,
			Mode: "full", LastFullAt: ago(34 * time.Second), EventsExpected: intp(10)},
			"Full sync · calendar 1/3 · 0 of 10 events · 4 s", 0, "syncing…", ui.TraySyncing},
		{"listing past its estimate", service.PassProgress{StartedAt: *ago(10 * time.Second), Calendar: 1, Calendars: 1, CalendarName: "Perso",
			Mode: "full", EventsFetched: 120, EventsExpected: intp(100)},
			"Full sync · Perso (1/1) · 100 of 100 events · 10 s", 1, "syncing…", ui.TraySyncing},
	} {
		v := running(tc.p)
		a := v.Accounts[0]
		if a.Progress == nil {
			t.Errorf("%s: no progress shown", tc.name)
			continue
		}
		if a.Progress.Text != tc.text || a.Progress.Fraction != tc.fraction || a.Short != tc.short || a.Dot != tc.dot || v.Tray != tc.dot {
			t.Errorf("%s: %q, %v, %q, dot %q, tray %q;\nwant %q, %v, %q, %q both", tc.name, a.Progress.Text, a.Progress.Fraction, a.Short, a.Dot, v.Tray, tc.text, tc.fraction, tc.short, tc.dot)
		}
	}
}

// A signed-out account shows no progress, and what it needs outranks a pass.
func TestProgressOnlyForSignedInAccounts(t *testing.T) {
	a := ready("a", 10*time.Second, 30)
	a.Phase, a.Login.Phase = "needs-login", "needs-login"
	a.Health.Pass = &service.PassProgress{StartedAt: *ago(time.Minute), Calendars: 1}
	got := ui.Build(status(a), now).Accounts[0]
	if got.Progress != nil || !strings.Contains(got.Short, "needs login") {
		t.Errorf("signed out: progress %+v, %q; want none, needs login", got.Progress, got.Short)
	}
}

// While a pass runs the window redraws every ProgressTick on its own;
// otherwise only a change redraws it; the end of ctx ends the wait.
func TestWaitRedraw(t *testing.T) {
	changes := make(chan struct{}, 1)
	ctx := t.Context()
	pass := ui.View{PassRunning: true}
	start := time.Now()
	if !ui.WaitRedraw(ctx, pass, changes) {
		t.Fatal("pass running: the wait reported the end of ctx")
	}
	if d := time.Since(start); d < ui.ProgressTick/2 || d > 5*ui.ProgressTick {
		t.Errorf("pass running: redrew after %v, want about %v", d, ui.ProgressTick)
	}

	short, cancel := context.WithTimeout(ctx, 3*ui.ProgressTick)
	defer cancel()
	if ui.WaitRedraw(short, ui.View{}, changes) {
		t.Error("no pass, no change: the wait ended before ctx")
	}

	changes <- struct{}{}
	start = time.Now()
	if !ui.WaitRedraw(ctx, ui.View{}, changes) || time.Since(start) > ui.ProgressTick/2 {
		t.Error("a change did not end the wait at once")
	}
}
