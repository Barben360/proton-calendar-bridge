package cli_test

import (
	"net/http"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// End to end: pcb serve with [mcp] remote on serves a token from pcb mcp
// token add on /mcp, its tools reading times in [mcp] timezone.
func TestServeServesRemoteMCP(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	writeConfig(t, r, "[mcp]\nremote = true\ntimezone = \"Asia/Tokyo\"\n")
	r.want(exitcode.OK, "mcp", "token", "add", "agent", "--allow", "read", "--json")
	var added struct {
		Token string `json:"token"`
	}
	mustUnmarshal(t, r.envelope().Data, &added)
	if added.Token == "" {
		t.Fatal("token add printed no token")
	}
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	addr := s.listening()

	hc := &http.Client{Transport: bearerTransport{secret: added.Token}}
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: "http://" + addr + "/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "status", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("status: %v %+v", err, res)
	}
	out, _ := res.StructuredContent.(map[string]any)
	if out["zone"] != "Asia/Tokyo" {
		t.Errorf("status zone = %v, want [mcp] timezone Asia/Tokyo", out["zone"])
	}
	_ = cs.Close()
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
}

// bearerTransport sends every request with the token.
type bearerTransport struct{ secret string }

func (b bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.secret)
	return http.DefaultTransport.RoundTrip(req)
}
