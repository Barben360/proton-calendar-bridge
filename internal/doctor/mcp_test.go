package doctor_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
)

// mcpResults runs the mcp tokens check of state for [mcp] remote and a
// [caldav] listen address, at day0.
func mcpResults(t *testing.T, state string, remote bool, listen string) []doctor.Result {
	t.Helper()
	c := doctor.MCPTokensCheck{StateRoot: state, Remote: remote, Bind: &doctor.Bind{Listen: listen}, Now: func() time.Time { return day0 }}
	return doctor.Run(context.Background(), []doctor.Check{c}, nil)
}

func addMCPToken(t *testing.T, state, name string, expires *time.Time) {
	t.Helper()
	if _, err := access.NewTokenStore(state, func() time.Time { return day0.Add(-48 * time.Hour) }).
		Add(context.Background(), access.Token{Name: name, Caps: []string{"read"}, Expires: expires}); err != nil {
		t.Fatal(err)
	}
}

// find is the result named name, nil when there is none.
func find(rs []doctor.Result, name string) *doctor.Result {
	for i := range rs {
		if rs[i].Name == name {
			return &rs[i]
		}
	}
	return nil
}

// Design §5.8: remote MCP on a non-loopback bind with no token to use it
// is a warning; expired tokens are listed as warnings; a token file that
// cannot be read is an error.
func TestDoctorWarnsRemoteWithoutToken(t *testing.T) {
	empty := t.TempDir()
	rs := mcpResults(t, empty, true, "0.0.0.0:8443")
	if w := find(rs, "mcp remote"); w == nil || w.Status != doctor.StatusWarn || !strings.Contains(w.Message, "no token") ||
		!strings.Contains(w.Fix, "pcb mcp token add") {
		t.Errorf("remote on 0.0.0.0, no token: %+v, want a warning with the fix", rs)
	}
	if r := find(rs, "mcp tokens"); r == nil || r.Status != doctor.StatusOK {
		t.Errorf("mcp tokens = %+v, want ok", r)
	}
	for _, c := range []struct {
		remote bool
		listen string
	}{{true, "127.0.0.1:1080"}, {true, "[::1]:1080"}, {false, "0.0.0.0:8443"}} {
		if w := find(mcpResults(t, empty, c.remote, c.listen), "mcp remote"); w != nil {
			t.Errorf("remote %v on %s: %+v, want no warning", c.remote, c.listen, *w)
		}
	}
	// Nothing to say while MCP is not used: no token, remote off.
	if rs := mcpResults(t, empty, false, "127.0.0.1:1080"); len(rs) != 0 {
		t.Errorf("no token, remote off: %+v, want no result", rs)
	}

	// One usable token: no warning.
	withToken := t.TempDir()
	addMCPToken(t, withToken, "agent", nil)
	rs = mcpResults(t, withToken, true, "0.0.0.0:8443")
	if w := find(rs, "mcp remote"); w != nil {
		t.Errorf("with a token: %+v, want no warning", *w)
	}
	if r := find(rs, "mcp tokens"); r == nil || r.Status != doctor.StatusOK || r.Message != "1 token" {
		t.Errorf("mcp tokens = %+v, want ok, 1 token", r)
	}

	// Only an expired token: it is listed, and remote still has none.
	expired := t.TempDir()
	past := day0.Add(-time.Hour)
	addMCPToken(t, expired, "old", &past)
	rs = mcpResults(t, expired, true, "0.0.0.0:8443")
	if w := find(rs, "mcp remote"); w == nil || w.Status != doctor.StatusWarn {
		t.Errorf("only an expired token: %+v, want the remote warning", rs)
	}
	if w := find(rs, "mcp token old"); w == nil || w.Status != doctor.StatusWarn || !strings.Contains(w.Message, "expired") ||
		w.Fix != "pcb mcp token remove old" {
		t.Errorf("expired token: %+v, want a warning with its removal", rs)
	}
	// Remote off, the expired token is still worth a warning.
	if w := find(mcpResults(t, expired, false, "127.0.0.1:1080"), "mcp token old"); w == nil || w.Status != doctor.StatusWarn {
		t.Errorf("expired token with remote off: %+v, want a warning", w)
	}

	// An unreadable file is an error, whatever remote says.
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "mcp-tokens.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := find(mcpResults(t, broken, false, "127.0.0.1:1080"), "mcp tokens"); r == nil || r.Status != doctor.StatusError || r.Fix == "" {
		t.Errorf("a broken token file: %+v, want an error with a fix", r)
	}
}
