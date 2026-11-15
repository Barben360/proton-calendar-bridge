//go:build unix

package ctl

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// Ruling M5-R2: Dial checks with lstat that the path is a socket owned by
// this process's effective UID before it dials. Missing is no daemon;
// anything else is refused, blocked (20).
func TestDialChecksTheSocketBeforeDialling(t *testing.T) {
	t.Parallel()
	dir := sockDir(t)
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Dial(t.Context(), file); errors.Is(err, ErrNoDaemon) || exitcode.From(err) != exitcode.Blocked || exitcode.HintOf(err) == "" {
		t.Errorf("Dial on a file: %v (exit %d), want a blocked refusal with a hint", err, exitcode.From(err))
	}
	sock := filepath.Join(dir, "pcb.sock")
	listen(t, sock)
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSocket(sock, fi, os.Geteuid()); err != nil {
		t.Errorf("checkSocket(own socket) = %v", err)
	}
	err = checkSocket(sock, fi, os.Geteuid()+1)
	if exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "belongs to another user (uid "+strconv.Itoa(os.Geteuid())+")") {
		t.Errorf("checkSocket(another user's socket) = %v, want a blocked refusal naming the owner", err)
	}
}

// A daemon that cannot be reached is named by its socket's path, with the
// hint to run pcb as pcb serve's user (Windows' twin names the pipe:
// TestUnreachableNamesThePipe).
func TestUnreachableNamesTheSocket(t *testing.T) {
	cause := errors.New("boom")
	err := unreachable("/run/pcb/pcb.sock", cause)
	if err.Error() != "cannot reach the pcb daemon on /run/pcb/pcb.sock: boom" || exitcode.HintOf(err) != "check that you run pcb as the user that runs pcb serve" ||
		exitcode.From(err) != exitcode.Blocked || !errors.Is(err, cause) {
		t.Errorf("unreachable = %q, hint %q, exit %d", err, exitcode.HintOf(err), exitcode.From(err))
	}
}

// The control channel's names in messages and logs, on Unix: the socket
// (Windows' twin: TestWindowsNamesThePipeInMessages).
func TestUnixNamesTheSocketInMessages(t *testing.T) {
	if got := ErrRefused.Error(); got != "pcb refuses this control socket" {
		t.Errorf("ErrRefused = %q", got)
	}
	if Channel != "control socket" {
		t.Errorf("Channel = %q", Channel)
	}
	if a := AddrAttr("/run/pcb/pcb.sock"); a.Key != "socket" || a.Value.String() != "/run/pcb/pcb.sock" {
		t.Errorf("AddrAttr = %v, want socket=/run/pcb/pcb.sock", a)
	}
}
