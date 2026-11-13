package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const mcpInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// addToken stores an MCP token in the state root and returns its secret.
func (e *env) addToken(t *testing.T, tok access.Token) string {
	t.Helper()
	secret, err := access.NewTokenStore(e.dirs.State, nil).Add(t.Context(), tok)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// postMCP sends an initialize to /mcp of the listener with the bearer
// secret (none: no header) and host (none: the listener's address).
func postMCP(t *testing.T, r *running, secret, host string) *http.Response {
	t.Helper()
	_, port, _ := net.SplitHostPort(r.info.Addr)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:"+port+"/mcp", strings.NewReader(mcpInitialize))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if host != "" {
		req.Host = host
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res
}

func daemonStatus(t *testing.T, e *env) ctl.Status {
	t.Helper()
	c, err := ctl.Dial(t.Context(), e.dirs.Socket())
	if err != nil {
		t.Fatalf("the control socket does not answer: %v", err)
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Ruling R4: with [mcp] remote off (the default), /mcp is one more path of
// the CalDAV handler: no MCP answer, even for a valid token.
func TestRemoteMCPOffByDefault(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login(t)
	secret := e.addToken(t, access.Token{Name: "agent", Caps: []string{"read"}})
	r := e.start(t, e.hub, func(o *daemon.Options) { o.MCP = mcp.NewServer(mcp.Options{Hub: e.hub}) })
	res := postMCP(t, r, secret, "")
	if res.Header.Get("Mcp-Session-Id") != "" || res.StatusCode/100 == 2 {
		t.Errorf("/mcp with remote off: %d, session %q, want an answer that is not MCP", res.StatusCode, res.Header.Get("Mcp-Session-Id"))
	}
	if st := daemonStatus(t, e); st.MCP == nil || st.MCP.Remote {
		t.Errorf("status mcp = %+v, want remote off", st.MCP)
	}
}

// With [mcp] remote on, /mcp on the CalDAV listener serves token holders
// their grant's tools, behind the Host guard; CalDAV still answers beside
// it, and the status counts the session.
func TestRemoteMCPServesTools(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, password := e.login(t)
	e.cfg.MCP.Remote = true
	e.cfg.CalDAV.Hostnames = []string{"pcb.example"}
	secret := e.addToken(t, access.Token{Name: "agent", Caps: []string{"read"}})
	r := e.start(t, e.hub, func(o *daemon.Options) { o.MCP = mcp.NewServer(mcp.Options{Hub: e.hub}) })
	_, port, _ := net.SplitHostPort(r.info.Addr)

	hc := &http.Client{Transport: bearerRT{secret: secret}}
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: "http://127.0.0.1:" + port + "/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "events_list") || slices.Contains(names, "login") || slices.Contains(names, "event_create") {
		t.Errorf("tools = %v, want the read tools only", names)
	}
	if st := daemonStatus(t, e); st.MCP == nil || !st.MCP.Remote || st.MCP.Sessions["remote"] != 1 {
		t.Errorf("status mcp = %+v, want remote on with one remote session", st.MCP)
	}

	// The Host guard knows [caldav] hostnames: a configured name gets to
	// the token check, a foreign one does not.
	if got := postMCP(t, r, "", "pcb.example:"+port).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("Host pcb.example without a token: %d, want 401", got)
	}
	if got := postMCP(t, r, secret, "evil.example:"+port).StatusCode; got != http.StatusForbidden {
		t.Errorf("Host evil.example: %d, want 403", got)
	}
	// CalDAV is untouched beside /mcp.
	resp, err := r.propfind(t.Context(), "http://127.0.0.1:"+port+service.AccountPath(id), password)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("PROPFIND beside /mcp = %d, want 207", resp.StatusCode)
	}
}

// bearerRT sends every request with the token.
type bearerRT struct{ secret string }

func (b bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.secret)
	return http.DefaultTransport.RoundTrip(req)
}

// relayRT sends every request over the control socket with the relay's
// grant header, as pcb mcp does.
type relayRT struct {
	socket, grant string
}

func (r relayRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(mcp.RelayGrantHeader, r.grant)
	return (&http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", r.socket)
	}}).RoundTrip(req)
}

// relayClient is an MCP session over the control socket's /mcp with grant g.
func relayClient(t *testing.T, socket string, g mcp.Grant) (*sdkmcp.ClientSession, error) {
	t.Helper()
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: relayRT{socket: socket, grant: string(b)}}
	return sdkmcp.NewClient(&sdkmcp.Implementation{Name: "relay", Version: "0"}, nil).
		Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: "http://pcb/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
}

func toolNames(t *testing.T, cs *sdkmcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// The session daemon of pcb mcp (NoListener): no port is opened, not even
// for [mcp] remote, Ready gets a zero Info, and the control socket still
// answers, /mcp included, to a relay with its grant.
func TestRunWithoutListenerOpensNoPort(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login(t)
	e.cfg.MCP.Remote = true
	var listens atomic.Int32
	r := e.start(t, e.hub, func(o *daemon.Options) {
		o.Kind, o.NoListener = "mcp", true
		o.MCP = mcp.NewServer(mcp.Options{Hub: e.hub})
		o.Listen = func(string, string) (net.Listener, error) {
			listens.Add(1)
			return nil, errors.New("no listener for a session daemon")
		}
	})
	if r.info != (daemon.Info{}) {
		t.Errorf("Ready got %+v, want a zero Info", r.info)
	}
	if n := listens.Load(); n != 0 {
		t.Errorf("Listen was called %d times", n)
	}
	st := daemonStatus(t, e)
	if st.Daemon == nil || st.Daemon.Kind != "mcp" || st.Listener != nil {
		t.Errorf("status daemon %+v, listener %+v; want kind mcp and no listener", st.Daemon, st.Listener)
	}
	if st.MCP == nil || st.MCP.Remote {
		t.Errorf("status mcp = %+v, want remote off without a listener", st.MCP)
	}

	g, err := mcp.Profile("read-only")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := relayClient(t, e.dirs.Socket(), g)
	if err != nil {
		t.Fatalf("a relay over the control socket: %v", err)
	}
	defer func() { _ = cs.Close() }()
	if names := toolNames(t, cs); !slices.Contains(names, "events_list") || slices.Contains(names, "event_create") {
		t.Errorf("relay read-only tools = %v", names)
	}
	if st := daemonStatus(t, e); st.MCP == nil || st.MCP.Sessions["relay"] != 1 {
		t.Errorf("status mcp = %+v, want one relay session", st.MCP)
	}
	select {
	case <-r.fin:
		t.Fatalf("Run ended on its own: %v", r.err)
	default:
	}
}

// Every daemon kind serves /mcp on its control socket (pcb serve here,
// with its CalDAV listener): a relay's session gets its grant's tools.
func TestControlSocketMCPInServe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login(t)
	e.start(t, e.hub, func(o *daemon.Options) {
		o.Kind = "serve"
		o.MCP = mcp.NewServer(mcp.Options{Hub: e.hub})
	})
	g, _ := mcp.Profile("full")
	cs, err := relayClient(t, e.dirs.Socket(), g)
	if err != nil {
		t.Fatalf("a relay over the control socket: %v", err)
	}
	defer func() { _ = cs.Close() }()
	if names := toolNames(t, cs); !slices.Contains(names, "event_create") || !slices.Contains(names, "event_delete") {
		t.Errorf("relay full tools = %v", names)
	}
	// Without the grant header the relay is refused.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", e.dirs.Socket())
	}}}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://pcb/mcp", strings.NewReader(mcpInitialize))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("/mcp without a grant: %d, want 400", res.StatusCode)
	}
}
