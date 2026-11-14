package doctor

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// MCPTokensCheck reports the MCP tokens of <StateRoot>/mcp-tokens.json
// (design §5.8): an error when the file cannot be read, a warning per
// expired token, and a warning when [mcp] remote serves /mcp on a
// non-loopback bind and no token can use it. With no token and remote
// off, MCP is not in use and it reports nothing.
type MCPTokensCheck struct {
	StateRoot string
	Remote    bool             // [mcp] remote
	Listen    string           // [caldav] listen, when Bind is nil
	Bind      *Bind            // shared with the CalDAV checks; nil: its own
	Live      *Live            // the running daemon's listener (nil: none)
	Now       func() time.Time // nil: time.Now
}

// Name implements Check.
func (MCPTokensCheck) Name() string { return "mcp tokens" }

// Run implements Check: the first of RunAll's results.
func (c MCPTokensCheck) Run(ctx context.Context) Result {
	if rs := c.RunAll(ctx); len(rs) > 0 {
		return rs[0]
	}
	return Result{Name: c.Name(), Status: StatusSkip, Message: "MCP is not in use"}
}

// RunAll implements Multi.
func (c MCPTokensCheck) RunAll(ctx context.Context) []Result {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	tokens, err := access.NewTokenStore(c.StateRoot, now).List()
	if err != nil {
		return []Result{{Name: c.Name(), Status: StatusError, Message: err.Error(),
			Fix: "fix or restore " + shellquote.Quote(filepath.Join(c.StateRoot, "mcp-tokens.json")) + " (pcb mcp token list shows the error)"}}
	}
	if len(tokens) == 0 && !c.Remote {
		return nil
	}
	var rs []Result
	usable := 0
	for _, t := range tokens {
		if t.Expires != nil && !now().Before(*t.Expires) {
			rs = append(rs, Result{Name: "mcp token " + t.Name, Status: StatusWarn,
				Message: "expired on " + t.Expires.UTC().Format(time.RFC3339) + ": agents using it get 401",
				Fix:     "pcb mcp token remove " + shellquote.Quote(t.Name)})
			continue
		}
		usable++
	}
	count := fmt.Sprintf("%d tokens", len(tokens))
	switch len(tokens) {
	case 0:
		count = "no token"
	case 1:
		count = "1 token"
	}
	rs = append([]Result{{Name: c.Name(), Status: StatusOK, Message: count}}, rs...)
	if c.Remote && usable == 0 && !c.loopback(ctx) {
		rs = append(rs, Result{Name: "mcp remote", Status: StatusWarn,
			Message: "[mcp] remote serves /mcp on a non-loopback bind, but no token can use it",
			Fix:     "pcb mcp token add NAME --allow read, or set [mcp] remote = false"})
	}
	return rs
}

// loopback says whether the listener binds a loopback address: the running
// daemon's, else what pcb serve would bind (one that cannot be resolved
// counts as non-loopback, as for TLS).
func (c MCPTokensCheck) loopback(ctx context.Context) bool {
	var ip net.IP
	if c.Live != nil {
		host, _, err := net.SplitHostPort(c.Live.Addr)
		if err != nil {
			return false
		}
		ip = net.ParseIP(host)
	} else {
		var err error
		if ip, err = bindOf(c.Bind, c.Listen).IP(ctx); err != nil {
			return false
		}
	}
	return ip != nil && ip.IsLoopback()
}
