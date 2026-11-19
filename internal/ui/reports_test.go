package ui_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

var rctx = context.Background()

var rmeta = report.Meta{Binary: "pcb-ui", Command: "pcb-ui", Install: "other", Version: "v0.1.0"}

// rig is a Reports over a fresh state directory and a fake Sentry that
// counts the requests it gets.
type rig struct {
	*ui.Reports
	requests *atomic.Int32
}

func newRig(t *testing.T, status int) rig {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	dsn := "http://key@" + host + "/1"
	store := report.Store{StateDir: t.TempDir(), Env: func(string) string { return "" }}
	return rig{&ui.Reports{Store: store, Sender: report.Sender{DSN: dsn, Store: store}}, &n}
}

func (r rig) save(t *testing.T, fn string, crash bool) {
	t.Helper()
	frames := []report.Frame{{Function: fn}}
	e := report.ErrorEvent(errors.New("x"), frames, rmeta, time.Now())
	if crash {
		e = report.CrashEvent(report.Crash{Kind: "panic", Frames: frames}, rmeta, time.Now())
	}
	if err := r.Store.Save(rctx, e); err != nil {
		t.Fatal(err)
	}
}

func (r rig) pending(t *testing.T) int {
	t.Helper()
	n, _ := r.Pending()
	return n
}

func TestReportsPendingTellsACrashFromAnError(t *testing.T) {
	r := newRig(t, 200)
	if n, crashed := r.Pending(); n != 0 || crashed {
		t.Fatalf("empty store: %d, %v", n, crashed)
	}
	r.save(t, "a", false)
	if n, crashed := r.Pending(); n != 1 || crashed {
		t.Errorf("one error: %d, crashed %v", n, crashed)
	}
	r.save(t, "b", true)
	if n, crashed := r.Pending(); n != 2 || !crashed {
		t.Errorf("an error and a crash: %d, crashed %v", n, crashed)
	}
}

func TestReportsTextIsThePendingJSON(t *testing.T) {
	r := newRig(t, 200)
	r.save(t, "a", true)
	pend, _ := r.Store.Pending()
	got := r.Text()
	if !strings.Contains(got, "\n  \"event_id\": \""+pend[0].ID+"\"") {
		t.Errorf("Text is not the indented event:\n%s", got)
	}
}

func TestReportsSendPostsAndClears(t *testing.T) {
	r := newRig(t, 200)
	r.save(t, "a", true)
	if err := r.Send(rctx); err != nil {
		t.Fatal(err)
	}
	if n := r.pending(t); n != 0 || r.requests.Load() != 1 {
		t.Errorf("%d pending, %d requests; want 0 and 1", n, r.requests.Load())
	}
	if r.Store.Mode() != report.Ask {
		t.Errorf("Send changed the mode to %s", r.Store.Mode())
	}
}

func TestReportsSendFailureKeepsTheReport(t *testing.T) {
	r := newRig(t, 500)
	r.save(t, "a", true)
	if err := r.Send(rctx); err == nil {
		t.Error("a refused report is a nil error")
	}
	if n := r.pending(t); n != 1 {
		t.Errorf("%d pending after a failed send, want 1", n)
	}
}

func TestReportsAlwaysSendSetsTheModeAndSends(t *testing.T) {
	r := newRig(t, 200)
	r.save(t, "a", true)
	if err := r.AlwaysSend(rctx); err != nil {
		t.Fatal(err)
	}
	if r.Store.Mode() != report.Always || r.pending(t) != 0 || r.requests.Load() != 1 {
		t.Errorf("mode %s, %d pending, %d requests", r.Store.Mode(), r.pending(t), r.requests.Load())
	}
}

func TestReportsDontSendDeletesWithoutSending(t *testing.T) {
	r := newRig(t, 200)
	r.save(t, "a", true)
	if err := r.DontSend(rctx); err != nil {
		t.Fatal(err)
	}
	if r.pending(t) != 0 || r.requests.Load() != 0 || r.Store.Mode() != report.Ask {
		t.Errorf("%d pending, %d requests, mode %s", r.pending(t), r.requests.Load(), r.Store.Mode())
	}
}

func TestReportsEnabledRoundTrip(t *testing.T) {
	r := newRig(t, 200)
	if r.Enabled() {
		t.Fatal("enabled by default")
	}
	if err := r.SetEnabled(rctx, true); err != nil {
		t.Fatal(err)
	}
	if !r.Enabled() || r.Store.Mode() != report.Always {
		t.Errorf("on: enabled %v, mode %s", r.Enabled(), r.Store.Mode())
	}
	if err := r.SetEnabled(rctx, false); err != nil {
		t.Fatal(err)
	}
	if r.Enabled() || r.Store.Mode() != report.Ask {
		t.Errorf("off: enabled %v, mode %s", r.Enabled(), r.Store.Mode())
	}
}

// startWatch runs Watch under an installed recorder; shown counts its calls.
func startWatch(t *testing.T, r rig) (shown chan struct{}) {
	t.Helper()
	report.Install(&report.Process{Store: r.Store, Meta: rmeta, DSN: r.Sender.DSN})
	t.Cleanup(report.Uninstall)
	shown = make(chan struct{}, 10)
	ctx, cancel := context.WithCancel(rctx)
	done := make(chan struct{})
	subscribed := make(chan struct{})
	go func() {
		defer close(done)
		close(subscribed)
		r.Watch(ctx, func() { shown <- struct{}{} })
	}()
	t.Cleanup(func() { cancel(); <-done })
	<-subscribed
	time.Sleep(50 * time.Millisecond) // Watch's Subscribe follows the goroutine's start
	return shown
}

func TestReportsWatchFiresOnASavedReportInAsk(t *testing.T) {
	r := newRig(t, 200)
	shown := startWatch(t, r)
	report.RecordError(errors.New("boom"))
	select {
	case <-shown:
	case <-time.After(3 * time.Second):
		t.Fatal("no show after a report was saved in mode ask")
	}
}

func TestReportsWatchStaysQuietOutsideAsk(t *testing.T) {
	r := newRig(t, 200)
	if err := r.Store.SetMode(rctx, report.Always); err != nil {
		t.Fatal(err)
	}
	shown := startWatch(t, r)
	report.RecordError(errors.New("boom"))
	select {
	case <-shown:
		t.Error("show called in mode always")
	case <-time.After(300 * time.Millisecond):
	}
}

func TestStartReportsWithoutAddressKeepsNothing(t *testing.T) {
	state := t.TempDir()
	r, stop := ui.StartReports(state, "", rmeta, func(string) string { return "" })
	defer stop()
	if r != nil || report.Current() != nil {
		t.Errorf("reports %v, recorder %v for a build without an address", r, report.Current())
	}
}

func TestStartReportsInstallsTheRecorder(t *testing.T) {
	r, stop := ui.StartReports(t.TempDir(), "http://key@127.0.0.1:1/1", rmeta, func(string) string { return "" })
	if r == nil || report.Current() == nil {
		t.Fatalf("reports %v, recorder %v", r, report.Current())
	}
	stop()
	if report.Current() != nil {
		t.Error("the recorder outlives stop")
	}
}
