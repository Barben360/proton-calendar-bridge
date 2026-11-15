//go:build windows

package ctl

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

// clientSIDOf reads the SID of a pipe connection's client, from its
// process token (the server's check); serverSIDOf the SID owning the pipe
// (the client's check, checkPipeOwner). Variables: a test replaces
// serverSIDOf to play a pipe of another user.
var clientSIDOf, serverSIDOf = pipeClientSID, pipeOwnerSID

// pipeClientSID is the user SID of the process at the client end of c: its
// token's user. A connection that is not a named pipe, or a process or
// token this process cannot open, is an error: the server refuses it,
// whatever SID it would have read (another user's process; possibly an
// elevated pcb's, whose token a daemon that is not elevated may not read).
//
// The PID names the process that opened the client end, which may since
// have exited, its handle living on in a child or a duplicate, and its PID
// been reused. What makes reading it safe is the pipe's DACL (Listen): only
// this user's processes can open the pipe, so every client end was opened
// by this user. A reused PID can only make the check read another process's
// token: another user's is refused, this user's is accepted, as the
// connection's opener was.
func pipeClientSID(c net.Conn) (string, error) {
	h, err := pipeHandle(c)
	if err != nil {
		return "", err
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(h, &pid); err != nil {
		return "", fmt.Errorf("reading the pipe client's process ID: %w", err)
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", fmt.Errorf("opening the pipe client's process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(p) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return "", fmt.Errorf("opening the token of the pipe client's process %d: %w", pid, err)
	}
	defer func() { _ = tok.Close() }()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("reading the user of the pipe client's process %d: %w", pid, err)
	}
	return u.User.Sid.String(), nil
}

// pipeOwnerSID is the owner SID of the pipe c is connected to, from its
// security descriptor. A connection that is not a named pipe, or a
// descriptor that cannot be read, is an error: the client refuses it.
func pipeOwnerSID(c net.Conn) (string, error) {
	h, err := pipeHandle(c)
	if err != nil {
		return "", err
	}
	sd, err := windows.GetSecurityInfo(h, windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("reading the pipe's owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", fmt.Errorf("reading the pipe's owner: %w", err)
	}
	if owner == nil {
		return "", errors.New("the pipe has no owner")
	}
	return owner.String(), nil
}

// pipeHandle is the Win32 handle of a named pipe connection.
func pipeHandle(c net.Conn) (windows.Handle, error) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return 0, errors.New("not a named pipe connection")
	}
	return windows.Handle(f.Fd()), nil
}
