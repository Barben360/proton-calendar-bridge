package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// The commands' logger (ruling A2): Warn and above only, one line in pcb's
// style, its fields in parentheses and a hint on its own line.
func TestNoteLoggerPrintsWarningsInPcbsStyle(t *testing.T) {
	var b strings.Builder
	log := newNoteLogger(&b).With("file", "/s/clients.json")
	log.Info(`created the CalDAV client "default": see pcb client show`)
	log.Warn(`the CalDAV client "default" could not be created: run pcb client add NAME`, "at", time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC))
	log.Error("clients.json cannot be loaded", "cause", "not JSON", "hint", "run pcb doctor")
	want := "Warning: the CalDAV client \"default\" could not be created: run pcb client add NAME (file: /s/clients.json, at: 2026-11-12T10:00:00Z)\n" +
		"Error: clients.json cannot be loaded (file: /s/clients.json, cause: not JSON)\nHint: run pcb doctor\n"
	if got := b.String(); got != want {
		t.Errorf("logged %q, want %q", got, want)
	}
}

// Ruling A11: an error attribute never prints its Go chain: a pcb error
// gives its message, a raw chain only its innermost cause.
func TestNoteLoggerPrintsNoGoChain(t *testing.T) {
	var b strings.Builder
	raw := fmt.Errorf("updating state: %w", fmt.Errorf("open /s/state.lock: %w", &fs.PathError{Op: "open", Path: "/s/state.lock", Err: syscall.EACCES}))
	pcb := fmt.Errorf("wrapped: %w", &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the login state file cannot be read")})
	newNoteLogger(&b).Warn("the secrets generation could not be recorded", "error", raw, "other", pcb)
	want := "Warning: the secrets generation could not be recorded (error: permission denied, other: the login state file cannot be read)\n"
	if got := b.String(); got != want || strings.Contains(got, "/s/state.lock") {
		t.Errorf("logged %q, want %q", got, want)
	}
}
