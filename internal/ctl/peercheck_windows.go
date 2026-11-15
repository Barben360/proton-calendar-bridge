//go:build windows

package ctl

import (
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
)

// refusedPeerHint is the hint a refused peer gets: another user's process,
// or one whose token the daemon cannot read (pipeClientSID), such as an
// elevated pcb talking to a daemon that is not elevated (an elevated ssh
// session against the console's pcb-ui), whatever its login session.
const refusedPeerHint = "run pcb as the user that runs pcb-ui or pcb serve, and not elevated (as administrator) when they are not"

// channel is what messages call the control channel: a named pipe here.
const channel = "control pipe"

// unreachableHint is the hint for a daemon that cannot be reached: pcb-ui
// is the usual server on Windows.
const unreachableHint = "check that you run pcb as the user that runs pcb-ui or pcb serve"

// endpoint names the control channel of the socket path in a message: the
// pipe of its directory (Windows has no pcb.sock file), or that directory
// when it cannot be opened (another user's, say).
func endpoint(path string) string {
	dir := filepath.Dir(path)
	if name, err := pipeName(dir); err == nil {
		return "the control pipe " + name
	}
	return "the control pipe of " + dir
}

// refusedPeer reports whether r's pipe client runs as another user than
// this process (s.PeerSID, or the client process's token): a client whose
// SID cannot be read is refused, and so is a request whose connection is
// unknown. The attribute names the client's SID for the warning.
func (s *Server) refusedPeer(r *http.Request) (slog.Attr, bool) {
	c, ok := r.Context().Value(connKey{}).(net.Conn)
	if !ok {
		return slog.String("peer_sid", "unknown: no connection"), true
	}
	peer := s.PeerSID
	if peer == nil {
		peer = clientSIDOf
	}
	sid, err := peer(c)
	if err != nil {
		return slog.String("peer_sid", "unreadable: "+err.Error()), true
	}
	own, err := currentSID()
	return slog.String("peer_sid", sid), err != nil || sid != own
}
