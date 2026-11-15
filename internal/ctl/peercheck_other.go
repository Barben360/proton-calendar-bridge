//go:build !windows

package ctl

import (
	"log/slog"
	"net"
	"net/http"
	"os"
)

// refusedPeerHint is the hint a refused peer gets.
const refusedPeerHint = "run pcb as the user that runs pcb serve"

// channel is what messages call the control channel: a Unix socket here.
const channel = "control socket"

// unreachableHint is the hint for a daemon that cannot be reached.
const unreachableHint = "check that you run pcb as the user that runs pcb serve"

// endpoint names the control channel of the socket path in a message: the
// path itself.
func endpoint(path string) string { return path }

// refusedPeer reports whether r's peer is of another UID than this
// process's effective one (SO_PEERCRED reports the peer's effective UID);
// a request whose connection is unknown is refused. The attribute names
// the peer's UID for the warning.
func (s *Server) refusedPeer(r *http.Request) (slog.Attr, bool) {
	c, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok {
		return slog.Int("peer_uid", -1), true
	}
	peer := s.PeerUID
	if peer == nil {
		peer = defaultPeerUID
	}
	uid, checked := peer(c)
	return slog.Int("peer_uid", uid), checked && uid != os.Geteuid()
}
