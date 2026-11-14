//go:build windows

package ctl

import (
	"log/slog"
	"net"
	"net/http"
)

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
