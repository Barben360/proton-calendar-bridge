//go:build !unix

package ctl

import (
	"context"
	"net"
)

// Listen has no control socket to open off Unix (ruling M5-P3).
func Listen(string) (net.Listener, error) { return nil, ErrUnsupported }

// dialSocket finds no daemon off Unix: the CLI is always offline there.
func dialSocket(context.Context, string) (net.Conn, error) { return nil, ErrNoDaemon }
