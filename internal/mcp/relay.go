package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrDaemonGone is Relay's end when the daemon went away: the agent's
// next message is queued, for the next backend (another daemon, or this
// process become the session daemon).
var ErrDaemonGone = errors.New("the pcb daemon went away")

// ErrRelayRefused is Relay's end when the daemon answered without serving
// the session (a refused request, before any answer): the agent's
// messages stay queued, and a new attempt should wait first.
var ErrRelayRefused = errors.New("the pcb daemon did not serve the relay")

// protocolVersionHeader is the header MCP over HTTP requires on every
// request after initialize.
const protocolVersionHeader = "Mcp-Protocol-Version"

// Relay bridges stdio to the daemon's /mcp on the control socket, message
// by message. See (*Stdio).Relay.
func Relay(ctx context.Context, socket string, g Grant, in io.Reader, out io.Writer) error {
	return NewStdio(in, out).Relay(ctx, socket, g)
}

// Relay serves st through the daemon's /mcp on the control socket at
// socket, sending g as the grant (RelayAuth) on every request, until
// stdin ends (nil, after up to 5 s for the answers still due), ctx ends
// (its error) or the daemon goes away: then the tool calls in flight are
// answered retry and, once the agent sent its next message, Relay returns
// ErrDaemonGone (stdin ending meanwhile: nil). A daemon that answers
// without serving the session (a refused request before any answer) ends
// it with ErrRelayRefused, the agent's messages kept.
//
// The SDK's connection sends Mcp-Protocol-Version only for a session it
// initialised itself (E19): Relay adds the version the agent's initialize
// settled to every request that lacks one.
func (st *Stdio) Relay(ctx context.Context, socket string, g Grant) error {
	grant, err := json.Marshal(g)
	if err != nil {
		return err
	}
	base := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
	defer base.CloseIdleConnections()
	hc := &http.Client{Transport: &relayTransport{base: base, grant: string(grant), st: st}}
	b, err := (&sdkmcp.StreamableClientTransport{
		Endpoint:   "http://pcb/mcp",
		HTTPClient: hc,
		MaxRetries: -1, // a broken stream is a daemon gone: no reconnect
		// Only the answers to the agent's requests travel: the daemon
		// sends nothing outside them (E19: no standalone GET stream).
		DisableStandaloneSSE: true,
	}).Connect(ctx)
	if err != nil {
		return err
	}
	err = st.pump(ctx, b)
	var gone *goneError
	if !errors.As(err, &gone) {
		return err
	}
	if !gone.answered && gone.cause != nil && !undelivered(gone.cause) {
		return fmt.Errorf("%w: %w", ErrRelayRefused, gone.cause)
	}
	switch err := st.await(ctx); {
	case err == nil:
		return ErrDaemonGone
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return nil // stdin ended
	}
}

// relayTransport sends the grant, and the negotiated protocol version, on
// every request to the daemon.
type relayTransport struct {
	base  http.RoundTripper
	grant string
	st    *Stdio
}

func (t *relayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(RelayGrantHeader, t.grant)
	if req.Header.Get(protocolVersionHeader) == "" {
		if v := t.st.negotiated(); v != "" {
			req.Header.Set(protocolVersionHeader, v)
		}
	}
	return t.base.RoundTrip(req)
}

// undelivered reports whether a failed write never reached the daemon, so
// the message may go to the next backend: the socket could not be dialled,
// or the daemon no longer knows the session (it restarted, or the session
// idled out).
func undelivered(err error) bool {
	var op *net.OpError
	return errors.Is(err, sdkmcp.ErrSessionMissing) || (errors.As(err, &op) && op.Op == "dial")
}
