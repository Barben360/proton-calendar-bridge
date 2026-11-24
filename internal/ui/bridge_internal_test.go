package ui

import (
	"testing"
	"time"
)

// A pass's start wakes the window at once, not at the next 30 s tick.
func TestPassStartWakesTheWindow(t *testing.T) {
	b := NewBridge(BridgeOptions{})
	select {
	case <-b.Changes():
		t.Fatal("Changes fired before anything happened")
	default:
	}
	b.passStarted("a1")
	select {
	case <-b.Changes():
	case <-time.After(time.Second):
		t.Fatal("a pass start did not fire Changes")
	}
}
