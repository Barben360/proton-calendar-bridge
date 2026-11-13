package ui_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// The desktop app's daemon serves /mcp like pcb serve's when [mcp] remote
// is on.
func TestBridgeServesRemoteMCP(t *testing.T) {
	l := newLab(t)
	body := fmt.Sprintf("[proton]\nbase_url = %q\n[caldav]\nlisten = \"127.0.0.1:0\"\n[mcp]\nremote = true\n", l.f.URL)
	if err := os.WriteFile(config.Path(l.dirs), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(l.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	secret, err := access.NewTokenStore(l.dirs.State, nil).Add(t.Context(), access.Token{Name: "agent", Caps: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	b := l.start(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+b.Info().Addr+"/mcp", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("initialize on /mcp: %d, session %q, want 200 and a session", res.StatusCode, res.Header.Get("Mcp-Session-Id"))
	}
}

// grantHeader sends a relay's grant on every request to the control socket.
type grantHeader struct {
	base  http.RoundTripper
	grant string
}

func (g grantHeader) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(mcp.RelayGrantHeader, g.grant)
	return g.base.RoundTrip(req)
}

// An agent's login reaching pcb-ui (a pcb mcp relaying to it) raises the
// window's add-account wizard with the username: no page, no URL.
func TestBridgeRaisesTheWizardForAnAgentLogin(t *testing.T) {
	l := newLab(t)
	var (
		mu    sync.Mutex
		names []string
		got   = make(chan struct{}, 1)
	)
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, ShowLogin: func(u string) {
		mu.Lock()
		names = append(names, u)
		mu.Unlock()
		got <- struct{}{}
	}})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	socket := l.dirs.Socket()
	hc := &http.Client{Transport: grantHeader{grant: `{"Caps":["freebusy","login"]}`, base: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: "http://pcb/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "login", Arguments: map[string]any{"username": "carol@example.org"}})
	if err != nil || res.IsError {
		t.Fatalf("login: %v %+v", err, res)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("ShowLogin was not called")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(names) != 1 || names[0] != "carol@example.org" {
		t.Errorf("ShowLogin got %q", names)
	}
	if out, _ := res.StructuredContent.(map[string]any); out["where"] != "pcb-ui" || out["url"] != nil {
		t.Errorf("login = %v, want the wizard and no URL", out)
	}
}
