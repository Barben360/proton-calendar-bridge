//go:build windows

package ctl

import (
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
)

// refusedPeerHint is the hint a refused peer gets: another user's process,
// or one whose token the daemon cannot read (pipeClientSID). An elevated
// pcb talking to a daemon that is not elevated (an elevated ssh session
// against the console's pcb-ui) may be such a client: Windows may deny a
// normal process the token of an elevated one; not checked live.
const refusedPeerHint = "run pcb as the user that runs pcb-ui or pcb serve; an elevated (administrator) pcb may be refused by one that is not elevated"

// Channel is what messages call the control channel: a named pipe here.
const Channel = "control pipe"

// unreachableHint is the hint for a daemon that cannot be reached: pcb-ui
// is the usual server on Windows.
const unreachableHint = "check that you run pcb as the user that runs pcb-ui or pcb serve"

// endpoint names the control channel of the socket path in a message: the
// pipe of its directory (Windows has no pcb.sock file), or that directory
// when it cannot be opened (another user's, say).
func endpoint(path string) string {
	if name, ok := pipeOf(path); ok {
		return "the control pipe " + name
	}
	return "the control pipe of " + filepath.Dir(path)
}

// AddrAttr names the control channel of the socket path in a log line:
// pipe=<its name>, or pipe=the control pipe of <dir> when that directory
// cannot be opened.
func AddrAttr(path string) slog.Attr {
	if name, ok := pipeOf(path); ok {
		return slog.String("pipe", name)
	}
	return slog.String("pipe", "the control pipe of "+filepath.Dir(path))
}

// pipeOf is the pipe of the socket path's directory; ok is false when that
// directory cannot be opened.
func pipeOf(path string) (string, bool) {
	name, err := pipeName(filepath.Dir(path))
	return name, err == nil
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
