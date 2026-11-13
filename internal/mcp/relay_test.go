package mcp_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// A daemon as the relay sees it: the control socket's /mcp (HTTPHandler
// behind RelayAuth) on a unix socket, recording the requests it gets.

type seenRequest struct {
	method  string // the JSON-RPC method, "response" for an answer to the server
	version string // Mcp-Protocol-Version
	grant   string // X-Pcb-Mcp-Grant
}

type fakeDaemon struct {
	srv  *http.Server
	mu   sync.Mutex
	seen []seenRequest
}

// socketPath is a socket path short enough for sun_path.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcbr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func startDaemon(t *testing.T, srv *mcp.Server, socket string) *fakeDaemon {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDaemon{}
	h := srv.HTTPHandler(mcp.RelayAuth)
	d.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(body))
			method := "response"
			if m, err := jsonrpc.DecodeMessage(body); err == nil {
				if req, ok := m.(*jsonrpc.Request); ok {
					method = req.Method
				}
			}
			d.mu.Lock()
			d.seen = append(d.seen, seenRequest{method: method, version: r.Header.Get("Mcp-Protocol-Version"), grant: r.Header.Get(mcp.RelayGrantHeader)})
			d.mu.Unlock()
		}
		h.ServeHTTP(w, r)
	})}
	go func() { _ = d.srv.Serve(ln) }()
	t.Cleanup(func() { _ = d.srv.Close() })
	return d
}

func (d *fakeDaemon) requests() []seenRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.seen)
}

// kill ends the daemon at once: its socket closes, its streams break.
func (d *fakeDaemon) kill() { _ = d.srv.Close() }

// agentPipes are pcb mcp's stdin and stdout, and the agent's ends of them.
type agentPipes struct {
	stdin  io.Reader // pcb mcp reads
	stdout io.Writer // pcb mcp writes
	t      sdkmcp.Transport
}

func newPipes() agentPipes {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	return agentPipes{stdin: inR, stdout: outW, t: &sdkmcp.IOTransport{Reader: outR, Writer: inW}}
}

// connectAgent connects an agent declaring form elicitation (answering
// answer, none: no handler) on protocol version.
func connectAgent(t *testing.T, p agentPipes, version, answer string) *sdkmcp.ClientSession {
	t.Helper()
	var opts *sdkmcp.ClientOptions
	if answer != "" {
		opts = &sdkmcp.ClientOptions{ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			return &sdkmcp.ElicitResult{Action: answer, Content: map[string]any{}}, nil
		}}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, opts).
		Connect(ctx, p.t, &sdkmcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// ended waits for a pcb mcp run's result.
func ended(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("still running 15 s after the agent left")
	}
	return nil
}

var fullGrant = func() mcp.Grant {
	g, err := mcp.Profile("full")
	if err != nil {
		panic(err)
	}
	return g
}()

// The relay carries a whole session to the daemon's /mcp, for an agent
// on the newest protocol (renegotiated to what a stateful /mcp serves)
// and for one on 2025-11-25: tools, results, the grant on every request
// and, once a version is negotiated, the Mcp-Protocol-Version header the
// spec requires on every later request (the SDK's connection does not
// send it for a session it did not initialise itself, E19). The end of
// stdin ends Relay with nil.
func TestRelayServesBothProtocolVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, newHub())
			socket := socketPath(t)
			d := startDaemon(t, r.srv, socket)
			p := newPipes()
			done := make(chan error, 1)
			go func() { done <- mcp.Relay(t.Context(), socket, fullGrant, p.stdin, p.stdout) }()
			cs := connectAgent(t, p, version, "")
			negotiated := cs.InitializeResult().ProtocolVersion
			if negotiated != "2025-11-25" {
				t.Errorf("negotiated %q through the relay, want 2025-11-25 (a stateful /mcp)", negotiated)
			}
			tools, err := cs.ListTools(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(tools.Tools, func(tl *sdkmcp.Tool) bool { return tl.Name == "event_delete" }) {
				t.Errorf("tools of the full profile lack event_delete: %d tools", len(tools.Tools))
			}
			if got := accountsOf(mustCall(t, cs, "status", nil)); !slices.Equal(got, []string{"alice@example.com", "bob@example.com"}) {
				t.Errorf("status accounts = %v", got)
			}
			if n := r.srv.LiveSessions()[mcp.EntryRelay]; n != 1 {
				t.Errorf("relay sessions = %d, want 1", n)
			}
			_ = cs.Close()
			if err := ended(t, done); err != nil {
				t.Errorf("Relay after the end of stdin = %v, want nil", err)
			}

			reqs := d.requests()
			initAt := slices.IndexFunc(reqs, func(s seenRequest) bool { return s.method == "initialize" })
			if initAt < 0 || initAt == len(reqs)-1 {
				t.Fatalf("requests %+v: no initialize, or nothing after it", reqs)
			}
			for i, s := range reqs {
				if !strings.Contains(s.grant, `"Caps"`) {
					t.Errorf("request %d (%s) without the grant header: %q", i, s.method, s.grant)
				}
				if i > initAt && s.version != negotiated {
					t.Errorf("request %d (%s) after initialize: Mcp-Protocol-Version %q, want %q", i, s.method, s.version, negotiated)
				}
			}
		})
	}
}

// A confirmation asked by the daemon (form elicitation, server to agent)
// crosses the relay both ways: the agent's answer reaches the tool.
func TestRelayCarriesAConfirmation(t *testing.T) {
	t.Parallel()
	hub := newHub()
	r := newRig(t, hub)
	socket := socketPath(t)
	startDaemon(t, r.srv, socket)
	p := newPipes()
	go func() { _ = mcp.Relay(t.Context(), socket, fullGrant, p.stdin, p.stdout) }()
	cs := connectAgent(t, p, "2025-11-25", "accept")
	out := mustCall(t, cs, "event_delete", map[string]any{"event_id": standupID, "etag": `"standup@x-1"`, "scope": "series"})
	if out["deleted"] != true {
		t.Errorf("delete = %v", out)
	}
	if w := hub.agendas["a1"].writes(); len(w.deletes) != 1 || w.written != 1 {
		t.Errorf("deletes %+v, want the confirmed one", w)
	}
}

// RunStdio serves the session in process: the newest protocol is served
// as it is (no stateful HTTP between), 2025-11-25 too; the session counts
// as entry stdio; the end of stdin ends it with nil.
func TestRunStdioServesBothProtocolVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, newHub())
			p := newPipes()
			done := make(chan error, 1)
			go func() { done <- r.srv.RunStdio(t.Context(), fullGrant, p.stdin, p.stdout) }()
			cs := connectAgent(t, p, version, "")
			if got := cs.InitializeResult().ProtocolVersion; got != version {
				t.Errorf("negotiated %q, want %q", got, version)
			}
			if got := accountsOf(mustCall(t, cs, "status", nil)); len(got) != 2 {
				t.Errorf("status accounts = %v", got)
			}
			if n := r.srv.LiveSessions()[mcp.EntryStdio]; n != 1 {
				t.Errorf("stdio sessions = %d, want 1", n)
			}
			_ = cs.Close()
			if err := ended(t, done); err != nil {
				t.Errorf("RunStdio after the end of stdin = %v, want nil", err)
			}
		})
	}
}

// blockingHub's Health waits until release closes, after telling entered:
// a status call stays in flight.
type blockingHub struct {
	*stubHub
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockingHub) Health(ctx context.Context, sel string) (service.Health, error) {
	h.once.Do(func() { close(h.entered) })
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return h.stubHub.Health(ctx, sel)
}

// A daemon that ends during a call: the call in flight returns a retry
// tool error, and Relay returns ErrDaemonGone once the agent sends its
// next message (design §5.3). Relayed again to another daemon, the
// session goes on without a new handshake from the agent (the relay
// replays it): the next call is answered there.
func TestRelayAnswersRetryWhenItsDaemonEnds(t *testing.T) {
	t.Parallel()
	slow := &blockingHub{stubHub: newHub(), entered: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(slow.release) })
	first := mcp.NewServer(mcp.Options{Hub: slow, Now: func() time.Time { return fixedNow }, Zone: paris})
	socket := socketPath(t)
	d := startDaemon(t, first, socket)

	p := newPipes()
	st := mcp.NewStdio(p.stdin, p.stdout)
	relayed := make(chan error, 1)
	go func() { relayed <- st.Relay(t.Context(), socket, fullGrant) }()
	cs := connectAgent(t, p, "2025-11-25", "")

	type result struct {
		res *sdkmcp.CallToolResult
		err error
	}
	inFlight := make(chan result, 1)
	go func() {
		res, err := cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "status", Arguments: map[string]any{}})
		inFlight <- result{res, err}
	}()
	select {
	case <-slow.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the status call never reached the daemon")
	}
	d.kill()
	var got result
	select {
	case got = <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("the call in flight never returned after its daemon ended")
	}
	if got.err != nil || got.res == nil || !got.res.IsError || len(got.res.Content) != 1 ||
		!strings.Contains(got.res.Content[0].(*sdkmcp.TextContent).Text, `"class":"retry"`) {
		t.Fatalf("call in flight = %+v, %v; want a retry tool error", got.res, got.err)
	}
	select {
	case err := <-relayed:
		t.Fatalf("Relay returned %v before the agent's next message", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The next call: Relay returns ErrDaemonGone, keeping the message;
	// relayed to a new daemon, it is answered there.
	second := newRig(t, newHub())
	startDaemon(t, second.srv, socket)
	next := make(chan map[string]any, 1)
	go func() { next <- mustCall(t, cs, "status", nil) }()
	if err := ended(t, relayed); !errors.Is(err, mcp.ErrDaemonGone) {
		t.Fatalf("Relay = %v, want ErrDaemonGone", err)
	}
	go func() { relayed <- st.Relay(t.Context(), socket, fullGrant) }()
	select {
	case out := <-next:
		if got := accountsOf(out); len(got) != 2 {
			t.Errorf("status through the new daemon = %v", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the next call was not answered by the new daemon")
	}
	if n := second.srv.LiveSessions()[mcp.EntryRelay]; n != 1 {
		t.Errorf("the new daemon has %d relay sessions, want 1", n)
	}
}

// A daemon that answers the relay's requests without serving them (here a
// 400 for every one, as a daemon of another version refusing the grant
// header) is not a daemon gone: Relay says it was refused, so pcb mcp
// waits before it probes again instead of looping, and the agent's
// message stays queued for the next attempt.
func TestRelayRefusedIsNotADaemonGone(t *testing.T) {
	t.Parallel()
	socket := socketPath(t)
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	refusing := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "a relay must send its grant", http.StatusBadRequest)
	})}
	go func() { _ = refusing.Serve(ln) }()

	p := newPipes()
	st := mcp.NewStdio(p.stdin, p.stdout)
	relayed := make(chan error, 1)
	go func() { relayed <- st.Relay(t.Context(), socket, fullGrant) }()
	connected := make(chan *sdkmcp.ClientSession, 1)
	go func() {
		cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, nil).Connect(t.Context(), p.t, nil)
		if err == nil {
			t.Cleanup(func() { _ = cs.Close() })
		}
		connected <- cs
	}()
	if err := ended(t, relayed); !errors.Is(err, mcp.ErrRelayRefused) || errors.Is(err, mcp.ErrDaemonGone) {
		t.Fatalf("Relay = %v, want ErrRelayRefused", err)
	}
	_ = refusing.Close()

	// The agent's initialize waited in the queue: a daemon that serves
	// answers it.
	startDaemon(t, newRig(t, newHub()).srv, socket)
	go func() { relayed <- st.Relay(t.Context(), socket, fullGrant) }()
	select {
	case cs := <-connected:
		if cs == nil {
			t.Fatal("the agent's handshake failed")
		}
		mustCall(t, cs, "status", nil)
	case <-time.After(10 * time.Second):
		t.Fatal("the queued handshake was not answered")
	}
}

// A relay whose daemon went away between calls hands the session to the
// process itself (it became the daemon): the waiting message is answered
// in process, after the handshake is replayed there.
func TestStdioMovesFromARelayToTheProcess(t *testing.T) {
	t.Parallel()
	r := newRig(t, newHub())
	socket := socketPath(t)
	d := startDaemon(t, r.srv, socket)
	p := newPipes()
	st := mcp.NewStdio(p.stdin, p.stdout)
	relayed := make(chan error, 1)
	go func() { relayed <- st.Relay(t.Context(), socket, fullGrant) }()
	cs := connectAgent(t, p, "2025-11-25", "")
	mustCall(t, cs, "status", nil)
	d.kill()

	next := make(chan map[string]any, 1)
	go func() { next <- mustCall(t, cs, "calendars_list", nil) }()
	if err := ended(t, relayed); !errors.Is(err, mcp.ErrDaemonGone) {
		t.Fatalf("Relay = %v, want ErrDaemonGone", err)
	}
	own := newRig(t, newHub())
	served := make(chan error, 1)
	go func() { served <- own.srv.ServeStdio(t.Context(), st, fullGrant) }()
	select {
	case out := <-next:
		if out["calendars"] == nil {
			t.Errorf("calendars_list in process = %v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting call was not answered in process")
	}
	if n := own.srv.LiveSessions()[mcp.EntryStdio]; n != 1 {
		t.Errorf("stdio sessions in process = %d, want 1", n)
	}
	_ = cs.Close()
	if err := ended(t, served); err != nil {
		t.Errorf("ServeStdio after the end of stdin = %v", err)
	}
}
