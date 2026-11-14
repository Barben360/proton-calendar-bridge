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

// ClientSIDOf and ServerSIDOf read the SID of the process at the other
// end of a pipe connection: its client (server side) and its server
// (client side).
var ClientSIDOf, ServerSIDOf = pipeClientSID, pipeServerSID

// SetServerSIDOf makes Dial read a pipe server's SID with f until the test
// ends. A test calling it must not run in parallel.
func SetServerSIDOf(t testing.TB, f func(net.Conn) (string, error)) {
	t.Helper()
	old := serverSIDOf
	serverSIDOf = f
	t.Cleanup(func() { serverSIDOf = old })
}
