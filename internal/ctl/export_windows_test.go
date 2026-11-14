//go:build windows

package ctl

import (
	"net"
	"testing"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
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

// SetDeniedAndGone makes Listen's ListenPipe always fail access denied and
// its probe always find the name gone, until the test ends, and counts
// the tries. A test calling it must not run in parallel.
func SetDeniedAndGone(t testing.TB) (tries *int) {
	t.Helper()
	oldListen, oldProbe := listenPipe, probe
	n := 0
	listenPipe = func(string, *winio.PipeConfig) (net.Listener, error) {
		n++
		return nil, windows.ERROR_ACCESS_DENIED
	}
	probe = func(string) pipeProbe { return pipeGone }
	t.Cleanup(func() { listenPipe, probe = oldListen, oldProbe })
	return &n
}
