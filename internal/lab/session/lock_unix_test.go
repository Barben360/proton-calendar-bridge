//go:build unix

package session_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/labtest"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

func TestLoginRefusedWhileLockHeld(t *testing.T) {
	f := labtest.NewFront(t, &labtest.Front{})
	o := opts(t, f)
	if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(filepath.Join(o.StateDir, "login.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test file
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	_, err = session.Login(t.Context(), o)
	e := exitErr(t, err)
	if e.Code != exitcode.Blocked || !strings.Contains(e.Error(), "another pcb-lab login is running") {
		t.Fatalf("got %v", err)
	}
	if f.Requests != 0 {
		t.Fatal("Proton reached while the lock was held")
	}
}
