package cli

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// closed says whether c closes within d.
func closed(c <-chan struct{}, d time.Duration) bool {
	select {
	case <-c:
		return true
	case <-time.After(d):
		return false
	}
}

// A first signal equal to the session-end one (SIGTERM on Windows: the
// console close, logoff and shutdown events) closes ended as well as stop;
// any other signal, or no session-end signal, closes stop only.
func TestStopSignalsMarksASessionEnd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		sessionEnd os.Signal
		send       os.Signal
		ended      bool
	}{
		{"SIGTERM is the session end", syscall.SIGTERM, syscall.SIGTERM, true},
		{"Ctrl-C is no session end", syscall.SIGTERM, os.Interrupt, false},
		{"no session-end signal", nil, syscall.SIGTERM, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sigs := make(chan os.Signal, 2)
			stop, cut, ended, end := stopSignals(t.Context(), sigs, tc.sessionEnd)
			defer end()
			sigs <- tc.send
			if !closed(stop, 5*time.Second) {
				t.Fatal("stop did not close on the first signal")
			}
			if tc.ended && !closed(ended, 5*time.Second) {
				t.Error("ended did not close on the session-end signal")
			}
			if !tc.ended && closed(ended, 100*time.Millisecond) {
				t.Error("ended closed, want it open")
			}
			if closed(cut, 50*time.Millisecond) {
				t.Error("cut closed on one signal")
			}
		})
	}
}

// pcb's root context ends on the same SIGTERM that reaches stopSignals
// (signal.NotifyContext in Execute): whichever stopSignals sees first,
// ended closes (ruling F4), and a single SIGTERM never cuts.
func TestStopSignalsMarksASessionEndWhenTheContextEndsFirst(t *testing.T) {
	t.Parallel()
	for i := range 50 {
		ctx, cancel := context.WithCancel(t.Context())
		sigs := make(chan os.Signal, 2)
		stop, cut, ended, end := stopSignals(ctx, sigs, syscall.SIGTERM)
		if i%2 == 0 {
			cancel()
			sigs <- syscall.SIGTERM
		} else {
			sigs <- syscall.SIGTERM
			cancel()
		}
		if !closed(stop, 5*time.Second) || !closed(ended, 5*time.Second) {
			t.Fatalf("run %d: stop or ended still open after the context ended with a SIGTERM", i)
		}
		if closed(cut, 10*time.Millisecond) {
			t.Fatalf("run %d: one SIGTERM cut the stop", i)
		}
		end()
		cancel()
	}
	// The context ended long before the signal: still a session end.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sigs := make(chan os.Signal, 2)
	stop, _, ended, end := stopSignals(ctx, sigs, syscall.SIGTERM)
	defer end()
	if !closed(stop, 5*time.Second) {
		t.Fatal("stop did not close at the end of the context")
	}
	if closed(ended, 100*time.Millisecond) {
		t.Fatal("ended closed with no signal")
	}
	sigs <- syscall.SIGTERM
	if !closed(ended, 5*time.Second) {
		t.Error("a SIGTERM after the end of the context did not close ended")
	}
}

// A second signal cuts, as before.
func TestStopSignalsCutsOnTheSecondSignal(t *testing.T) {
	t.Parallel()
	sigs := make(chan os.Signal, 2)
	_, cut, _, end := stopSignals(t.Context(), sigs, syscall.SIGTERM)
	defer end()
	sigs <- os.Interrupt
	sigs <- os.Interrupt
	if !closed(cut, 5*time.Second) {
		t.Error("cut did not close on the second signal")
	}
}
