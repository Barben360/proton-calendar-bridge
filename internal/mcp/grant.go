package mcp

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Capability is a set of tools a grant allows (design §5.4).
type Capability string

// The capabilities, in the order they are shown. login and manage are
// stdio only: a token never holds them (access.TokenCaps).
const (
	CapFreeBusy Capability = "freebusy" // status, calendars_list, free_busy
	CapRead     Capability = "read"     // events_list, event_get, events_search; implies freebusy
	CapWrite    Capability = "write"    // event_create, event_update; implies read
	CapDelete   Capability = "delete"   // event_delete; needs write
	CapLogin    Capability = "login"    // login (stdio only)
	CapManage   Capability = "manage"   // logout (stdio only)
)

var capOrder = []Capability{CapFreeBusy, CapRead, CapWrite, CapDelete, CapLogin, CapManage}

// Grant is what one MCP session may do: its capabilities, the accounts it
// sees (an address or a local ID each; none: every account, those added
// later included) and the calendars (IDs; none: every calendar). Name is
// the token's name or the profile's, for the audit line.
type Grant struct {
	Name      string
	Caps      []Capability
	Accounts  []string
	Calendars []string
}

// knownCaps are the names ParseCaps accepts: a token's (access.TokenCaps)
// and the two stdio-only ones.
func knownCaps() []Capability {
	out := make([]Capability, 0, len(access.TokenCaps)+2)
	for _, c := range access.TokenCaps {
		out = append(out, Capability(c))
	}
	return append(out, CapLogin, CapManage)
}

// ParseCaps reads a comma-separated list of capabilities and applies their
// implications: read ⇒ freebusy, write ⇒ read. delete without write is an
// error, as is an unknown, empty or blank name. The result is in capOrder,
// without repeats.
func ParseCaps(s string) ([]Capability, error) {
	known := knownCaps()
	set := map[Capability]bool{}
	for part := range strings.SplitSeq(s, ",") {
		c := Capability(strings.TrimSpace(part))
		if !slices.Contains(known, c) {
			names := make([]string, len(known))
			for i, k := range known {
				names[i] = string(k)
			}
			return nil, fmt.Errorf("%q is not a capability: use %s", strings.TrimSpace(part), strings.Join(names, ", "))
		}
		set[c] = true
	}
	if set[CapDelete] && !set[CapWrite] {
		return nil, errors.New("delete needs write: a grant that may delete may also edit")
	}
	if set[CapWrite] {
		set[CapRead] = true
	}
	if set[CapRead] {
		set[CapFreeBusy] = true
	}
	var out []Capability
	for _, c := range capOrder {
		if set[c] {
			out = append(out, c)
		}
	}
	return out, nil
}

// Profile is a stdio profile's grant (design §5.3): read-only is read and
// login, full every capability. It names no account nor calendar.
func Profile(name string) (Grant, error) {
	var caps string
	switch name {
	case "read-only":
		caps = "read,login"
	case "full":
		caps = "write,delete,login,manage"
	default:
		return Grant{}, fmt.Errorf("%q is not a profile: use read-only or full", name)
	}
	c, err := ParseCaps(caps)
	return Grant{Name: name, Caps: c}, err
}

// has reports whether the grant holds c.
func (g Grant) has(c Capability) bool { return slices.Contains(g.Caps, c) }

// key identifies the grant: equal grants share one SDK server.
func (g Grant) key() string {
	caps := make([]string, len(g.Caps))
	for i, c := range g.Caps {
		caps[i] = string(c)
	}
	sorted := func(s []string) string {
		s = slices.Clone(s)
		slices.Sort(s)
		return strings.Join(s, "\x01")
	}
	return strings.Join([]string{g.Name, sorted(caps), sorted(g.Accounts), sorted(g.Calendars)}, "\x00")
}

// seesAccount: the account is in the grant (or the grant names none), not
// pending, not signed out, and its MCP switch is on (design §5.5b, Review
// Focus 5). Anything else answers as an account that does not exist.
func (g Grant) seesAccount(a service.AccountInfo) bool {
	if a.Pending || !a.MCP || a.Phase == phaseLoggedOut {
		return false
	}
	if len(g.Accounts) == 0 {
		return true
	}
	return slices.ContainsFunc(g.Accounts, func(name string) bool { return namesAccount(name, a) })
}

// namesAccount: name is a's local ID or, case-insensitively, its address.
func namesAccount(name string, a service.AccountInfo) bool {
	return name != "" && (name == a.ID || strings.EqualFold(name, a.Address))
}

// seesCalendar: the calendar is in the grant, or the grant names none.
func (g Grant) seesCalendar(id string) bool {
	return len(g.Calendars) == 0 || slices.Contains(g.Calendars, id)
}
