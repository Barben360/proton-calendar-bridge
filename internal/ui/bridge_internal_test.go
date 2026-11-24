package ui

import (
	"log/slog"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The daemon options Start runs with wake the window on each loop event: a
// pass's start at once, not at the next 30 s tick, as well as a state
// change and a pass's end.
func TestLoopCallbacksWakeTheWindow(t *testing.T) {
	b := NewBridge(BridgeOptions{})
	var counters daemon.RenderLog
	o := b.loopCallbacks(slog.New(slog.DiscardHandler), &counters)
	for _, tc := range []struct {
		name string
		fire func()
	}{
		{"pass start", func() { o.OnPassStart("a1") }},
		{"state change", func() { o.OnState("a1", service.ServeServing) }},
		{"pass end", func() { o.OnPass("a1", service.WatchPass{}) }},
	} {
		select {
		case <-b.Changes():
			t.Fatalf("%s: Changes fired before the event", tc.name)
		default:
		}
		if o.OnPassStart == nil || o.OnState == nil || o.OnPass == nil {
			t.Fatal("a loop callback is missing from the daemon options")
		}
		tc.fire()
		select {
		case <-b.Changes():
		case <-time.After(time.Second):
			t.Errorf("%s did not fire Changes", tc.name)
		}
	}
}
