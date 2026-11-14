package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// listenLoopback listens on addr, which must name a loopback IP literal and
// a port (127.0.0.1:0 for any free one). A host name is refused, even
// localhost: what it resolves to is not this program's to check. The
// address bound is checked again.
func listenLoopback(what, addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--%s %q: want HOST:PORT with a loopback IP, such as 127.0.0.1:0", what, addr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" {
		return nil, fmt.Errorf("--%s %q: the fake listens on a loopback IP only (127.0.0.1 or ::1), never %q", what, addr, host)
	}
	l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return nil, fmt.Errorf("--%s: %w", what, err)
	}
	bound, ok := l.Addr().(*net.TCPAddr)
	if !ok || !bound.IP.IsLoopback() {
		_ = l.Close()
		return nil, errors.New("--" + what + ": the address bound is not loopback")
	}
	return l, nil
}
