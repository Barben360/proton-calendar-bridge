//go:build windows

package ctl

import (
	"net"
	"testing"
)

// PipeName is the pipe that serves the state directory dir.
var PipeName = pipeName

// CurrentSID is the SID of this process's user.
var CurrentSID = currentSID

// ClientSIDOf reads the SID of a pipe connection's client process (the
// server's check); ServerSIDOf the SID owning the pipe (the client's).
var ClientSIDOf, ServerSIDOf = pipeClientSID, pipeOwnerSID

// SetServerSIDOf makes Dial read the SID owning a pipe with f until the
// test ends. A test calling it must not run in parallel.
func SetServerSIDOf(t testing.TB, f func(net.Conn) (string, error)) {
	t.Helper()
	old := serverSIDOf
	serverSIDOf = f
	t.Cleanup(func() { serverSIDOf = old })
}

// SetCurrentSID makes this process's user SID read with f until the test
// ends. A test calling it must not run in parallel.
func SetCurrentSID(t testing.TB, f func() (string, error)) {
	t.Helper()
	old := currentSID
	currentSID = f
	t.Cleanup(func() { currentSID = old })
}
