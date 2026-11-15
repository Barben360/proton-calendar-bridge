package cli_test

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// On Windows a descriptor number is not a handle: --password-fd 0 must read
// the standard input (Get-Content pw | pcb login --password-fd 0), which
// failed with "cannot read the password from descriptor 0" (live check,
// 2026-11-15). Not parallel: it swaps the process's standard input.
func TestPasswordFDZeroIsStandardInput(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wr.WriteString("pa ss\r\n"); err != nil {
		t.Fatal(err)
	}
	if err := wr.Close(); err != nil {
		t.Fatal(err)
	}
	old, err := windows.GetStdHandle(windows.STD_INPUT_HANDLE)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetStdHandle(windows.STD_INPUT_HANDLE, windows.Handle(rd.Fd())); err != nil {
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
