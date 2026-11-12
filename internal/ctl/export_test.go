package ctl

import (
	"context"
	"net"
	"time"
)

// DialCheckingPeer is Dial with peer reading the server's UID in place of
// SO_PEERCRED (ok false: unchecked).
func DialCheckingPeer(ctx context.Context, path string, peer func(net.Conn) (uid int, ok bool)) (*Client, error) {
	return dial(ctx, path, peer)
}

// DialWithClock is Dial with now as the clock that ages the first connection.
func DialWithClock(ctx context.Context, path string, now func() time.Time) (*Client, error) {
	c, err := dial(ctx, path, defaultPeerUID)
	if c != nil {
		c.now = now
		c.firstAt = now()
	}
	return c, err
}
