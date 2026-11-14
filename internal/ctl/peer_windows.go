//go:build windows

package ctl

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

// clientSIDOf reads the SID of a pipe connection's client (the server's
// check), serverSIDOf that of its server (the client's). Variables: a test
// replaces serverSIDOf to play a server of another user.
var clientSIDOf, serverSIDOf = pipeClientSID, pipeServerSID

func pipeClientSID(c net.Conn) (string, error) {
	return pipePeerSID(c, windows.GetNamedPipeClientProcessId)
}

func pipeServerSID(c net.Conn) (string, error) {
	return pipePeerSID(c, windows.GetNamedPipeServerProcessId)
}

// pipePeerSID is the user SID of the process pidOf names at the other end
// of c: its token's user. A connection that is not a named pipe, or a
// process or token that cannot be read, is an error: the caller refuses it.
func pipePeerSID(c net.Conn, pidOf func(windows.Handle, *uint32) error) (string, error) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return "", errors.New("not a named pipe connection")
	}
	var pid uint32
	if err := pidOf(windows.Handle(f.Fd()), &pid); err != nil {
		return "", fmt.Errorf("reading the pipe peer's process ID: %w", err)
	}
	p, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", fmt.Errorf("opening the pipe peer's process %d: %w", pid, err)
	}
	defer func() { _ = windows.CloseHandle(p) }()
	var tok windows.Token
	if err := windows.OpenProcessToken(p, windows.TOKEN_QUERY, &tok); err != nil {
		return "", fmt.Errorf("opening the token of the pipe peer's process %d: %w", pid, err)
	}
	defer func() { _ = tok.Close() }()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("reading the user of the pipe peer's process %d: %w", pid, err)
	}
	return u.User.Sid.String(), nil
}
