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
