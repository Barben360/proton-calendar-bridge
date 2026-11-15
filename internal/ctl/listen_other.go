//go:build !unix && !windows

package ctl

import (
	"context"
	"net"
)

// Listen has no control socket to open off Unix and Windows (ruling M5-P3).
func Listen(string) (net.Listener, error) { return nil, ErrUnsupported }

// dialSocket finds no daemon off Unix and Windows: the CLI is always
// offline there.
func dialSocket(context.Context, string, func(net.Conn) (int, bool)) (net.Conn, error) {
	return nil, ErrNoDaemon
}
