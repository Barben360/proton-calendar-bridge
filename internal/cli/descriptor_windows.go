package cli

import (
	"fmt"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// fdHint is how to hand pcb the secret s on a descriptor. cmd's < gives the
// file's bytes as they are; a Windows PowerShell 5.1 pipe re-encodes them
// with $OutputEncoding (ASCII), which mangles a non-ASCII password. Only 0
// is a usable input, so the mailbox password of a login whose password is
// piped comes from PCB_MAILBOX_PASSWORD.
func fdHint(s fdSecret) string {
	if s == mailboxFDSecret {
		return `set PCB_MAILBOX_PASSWORD, or pipe the mailbox password in (cmd /c "pcb login --mailbox-password-fd 0 < file"), ` +
			"or omit --mailbox-password-fd to be prompted"
	}
	return `pipe the password in (cmd /c "pcb login --password-fd 0 < pwfile"), or omit --password-fd to be prompted`
}

// stdHandlesHint is the hint of a descriptor other than 0, 1 or 2.
func stdHandlesHint(s fdSecret) string {
	if s == mailboxFDSecret {
		return "set PCB_MAILBOX_PASSWORD, or pipe the mailbox password into the standard input and use --mailbox-password-fd 0"
	}
	return `pipe the password into the standard input and use --password-fd 0 (cmd /c "pcb login --password-fd 0 < pwfile")`
}

// descriptor is the handle the descriptor flag of s (--password-fd N,
// --mailbox-password-fd N) reads on Windows, where a descriptor number is
// not a handle: 0, 1 and 2 are the standard handles (handle 0 is never
// valid, live 2026-11-15). Any other number is refused: as a raw handle
// value it could be one the process holds for something else, which pcb
// would read and close.
func descriptor(fd int, s fdSecret) (uintptr, error) {
	std := map[int]uint32{0: windows.STD_INPUT_HANDLE, 1: windows.STD_OUTPUT_HANDLE, 2: windows.STD_ERROR_HANDLE}
	n, ok := std[fd]
	if !ok {
		return 0, &exitcode.Error{Code: exitcode.Usage,
			Err:  fmt.Errorf("%s %d: on Windows only 0, 1 or 2 (the standard handles)", s.flag, fd),
			Hint: stdHandlesHint(s)}
	}
	h, err := windows.GetStdHandle(n)
	if err != nil || h == windows.InvalidHandle {
		return 0, UsageError(fmt.Errorf("%s %d is not a usable descriptor", s.flag, fd))
	}
	return uintptr(h), nil
}
