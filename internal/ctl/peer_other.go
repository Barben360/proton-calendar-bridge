//go:build !linux

package ctl

import "net"

// defaultPeerUID leaves the peer unchecked: other Unix rely on the socket's
// mode, 0600 in a 0700 directory (ruling M5-P3).
func defaultPeerUID(net.Conn) (int, bool) { return 0, false }
