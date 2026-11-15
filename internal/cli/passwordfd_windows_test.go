package cli_test

import (
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// On Windows a descriptor number is not a handle: --password-fd 0 must read
// the standard input (cmd /c "pcb login --password-fd 0 < pw"), which
// failed with "cannot read the password from descriptor 0" (live check,
// 2026-11-15). Not parallel: it swaps the process's standard input.
func TestPasswordFDZeroIsStandardInput(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	if _, err := wr.WriteString("pa ss\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}
	// pcb closes the handle it reads: give it a duplicate, so rd's own
	// handle is closed once, by rd, and never a second time by value.
	self := windows.CurrentProcess()
	var dup windows.Handle
	if err := windows.DuplicateHandle(self, windows.Handle(rd.Fd()), self, &dup, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		t.Fatal(err)
	}
	old, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetStdHandle(windows.STD_INPUT_HANDLE, dup); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = windows.SetStdHandle(windows.STD_INPUT_HANDLE, old) })

	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	r.want(exitcode.OK, "login", "--non-interactive", "--password-fd", "0")
	if len(s.logins) != 1 || s.logins[0].Password != "pa ss" {
		t.Errorf("logins %+v, want one with the password from standard input", s.logins)
	}
}

// Any other number would be read, then closed, as a raw handle value: one
// the process holds for something else. It is refused before any read.
func TestPasswordFDOnWindowsTakesOnlyTheStandardHandles(t *testing.T) {
	t.Parallel()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	if _, err := wr.WriteString("secret\n"); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}
	for _, fd := range []string{"3", strconv.FormatUint(uint64(rd.Fd()), 10)} {
		s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		r := stubRig(t, s)
		r.want(exitcode.Usage, "login", "--non-interactive", "--password-fd", fd)
		if len(s.logins) != 0 {
			t.Errorf("--password-fd %s reached the service", fd)
		}
		if e := r.errb.String(); !strings.Contains(e, "on Windows only 0, 1 or 2") || !strings.Contains(e, "--password-fd 0") {
			t.Errorf("--password-fd %s: stderr %q, want the Windows rule and --password-fd 0", fd, e)
		}
	}
	b, err := io.ReadAll(rd)
	if err != nil || string(b) != "secret\n" {
		t.Errorf("the pipe's handle was read or closed: %q, %v", b, err)
	}
}
