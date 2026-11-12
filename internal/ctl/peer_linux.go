//go:build linux

package ctl

import (
	"net"
	"syscall"
)

// defaultPeerUID reads the peer's UID with SO_PEERCRED: the server checks
// its client with it, and the client its server. A connection whose
// credentials cannot be read reports -1, checked: it is refused, never let
// through unchecked.
func defaultPeerUID(c net.Conn) (int, bool) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return -1, true
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return -1, true
	}
	var cred *syscall.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil || cerr != nil {
		return -1, true
	}
	return int(cred.Uid), true
}
