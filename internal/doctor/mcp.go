package doctor

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// MCPTokensCheck reports the MCP tokens of <StateRoot>/mcp-tokens.json
// (design §5.8): an error when the file cannot be read, a warning per
// expired token, and a warning when [mcp] remote serves /mcp and no token
// can use it. With no token and remote off, MCP is not in use and it
// reports nothing.
type MCPTokensCheck struct {
	StateRoot string
	Remote    bool             // [mcp] remote
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
	if c.Remote && usable == 0 {
		rs = append(rs, Result{Name: "mcp remote", Status: StatusWarn,
			Message: "[mcp] remote serves /mcp, but no token can use it: every request gets 401",
			Fix:     "pcb mcp token add NAME --allow read, or set [mcp] remote = false"})
	}
	return rs
}

// SocketPathCheck reports a control socket path longer than a Unix socket
// address holds: pcb serve cannot create it (ctl.ErrPathTooLong), and pcb
// status falls back to its offline view. Unix only: Windows' control
// channel is a named pipe.
type SocketPathCheck struct{ Path string }

// Name implements Check.
func (SocketPathCheck) Name() string { return "control socket path" }

// Run implements Check.
func (c SocketPathCheck) Run(context.Context) Result {
	r := Result{Name: c.Name(), Status: StatusOK, Message: c.Path}
	if n := len(c.Path); n > ctl.MaxSocketPath {
		r.Status = StatusError
		r.Message = fmt.Sprintf("%s is %d bytes: a Unix socket path holds %d at most, so pcb serve cannot create it", c.Path, n, ctl.MaxSocketPath)
		r.Fix = "set PCB_STATE_DIR to a shorter absolute path"
	}
	return r
}
