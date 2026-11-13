package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
)

// RelayGrantHeader carries the grant of a pcb mcp relay to the control
// socket's /mcp: the JSON of its Grant.
const RelayGrantHeader = "X-Pcb-Mcp-Grant"

// RelayAuth admits a pcb mcp relay on the control socket's /mcp (design
// §5.3) as entry relay: its grant is the JSON Grant of the
// X-Pcb-Mcp-Grant header, its capabilities re-read through ParseCaps
// (implications applied). A missing or unreadable header, or capabilities
// ParseCaps refuses, is 400. The session's identity, and the grant's name
// in the audit, is "stdio:" and the profile whose capabilities it holds
// (read-only, full), else "stdio:custom".
//
// The grant is a guard rail, not a boundary (decision 0016): any process
// of the socket's UID may send any grant, or run pcb itself. The control
// socket's peer-UID check, in front of this route, is the only boundary.
func RelayAuth(r *http.Request) (Grant, Entry, string, error) {
	bad := func(msg string) (Grant, Entry, string, error) {
		return Grant{}, "", "", &AuthError{Status: http.StatusBadRequest, Message: msg}
	}
	raw := strings.TrimSpace(r.Header.Get(RelayGrantHeader))
	if raw == "" {
		return bad("a relay must send its grant in " + RelayGrantHeader + ": use the pcb mcp of the same version as the daemon")
	}
	var g Grant
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return bad(RelayGrantHeader + " is not a grant: use the pcb mcp of the same version as the daemon")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad(RelayGrantHeader + " holds more than one grant")
	}
	names := make([]string, len(g.Caps))
	for i, c := range g.Caps {
		names[i] = string(c)
	}
	caps, err := ParseCaps(strings.Join(names, ","))
	if err != nil {
		return bad("the relay's capabilities cannot be served: " + err.Error())
	}
	g.Caps = caps
	g.Name = "stdio:" + profileOf(g)
	return g, EntryRelay, g.Name, nil
}

// profileOf is the name of the profile whose capabilities g holds, else
// "custom". The name g was sent with does not count.
func profileOf(g Grant) string {
	for _, name := range []string{"read-only", "full"} {
		if p, err := Profile(name); err == nil && slices.Equal(p.Caps, g.Caps) {
			return name
		}
	}
	return "custom"
}
