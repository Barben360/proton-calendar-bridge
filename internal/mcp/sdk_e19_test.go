package mcp_test

// The SDK behaviours experiment E19 (spec/experiments/E19-mcp-go-sdk.md)
// records and the M8 design relies on. Each test goes red when an SDK bump
// changes one of them, before the code built on it breaks quietly.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

type e19ConfirmOut struct {
	Action string `json:"action"`
	Calls  int    `json:"calls"`
}

type e19CapsOut struct {
	HasCaps bool `json:"has_caps"`
}

// e19Server has "confirm", which asks a form elicitation through an
// InputRequests result (the form that works on every protocol version), and
// "caps", which reports whether the client's capabilities reached the handler.
func e19Server(calls *int) *sdkmcp.Server {
	s := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "e19", Version: "0"}, nil)
	sdkmcp.AddTool(s, &sdkmcp.Tool{Name: "confirm"}, func(_ context.Context, req *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, e19ConfirmOut, error) {
		*calls++
		if r, ok := req.Params.InputResponses["ok"].(*sdkmcp.ElicitResult); ok {
			return nil, e19ConfirmOut{Action: r.Action, Calls: *calls}, nil
		}
		return &sdkmcp.CallToolResult{InputRequests: sdkmcp.InputRequestMap{"ok": &sdkmcp.ElicitParams{
			Mode:            "form",
			Message:         "Delete?",
			RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		}}}, e19ConfirmOut{}, nil
	})
	sdkmcp.AddTool(s, &sdkmcp.Tool{Name: "caps"}, func(_ context.Context, req *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, e19CapsOut, error) {
		c := req.ClientCapabilities()
		return nil, e19CapsOut{HasCaps: c != nil && c.Elicitation != nil}, nil
	})
	return s
}

func e19Client(t *testing.T) *sdkmcp.Client {
	t.Helper()
	return sdkmcp.NewClient(&sdkmcp.Implementation{Name: "e19-client", Version: "0"}, &sdkmcp.ClientOptions{
		ElicitationHandler: func(context.Context, *sdkmcp.ElicitRequest) (*sdkmcp.ElicitResult, error) {
			return &sdkmcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
		},
	})
}

func e19Call(t *testing.T, cs *sdkmcp.ClientSession, name string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error %v", name, res.Content)
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("%s: structured content %T", name, res.StructuredContent)
	}
	return out
}

// An InputRequests result reaches the client as a form elicitation on the
// newest protocol (multi round-trip) and on 2025-11-25 (the SDK's shim sends
// elicitation/create), and the handler runs twice: everything before the
// question must be safe to repeat.
func TestE19InputRequestsConfirmOnEveryProtocol(t *testing.T) {
	for _, version := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			calls := 0
			st, ct := sdkmcp.NewInMemoryTransports()
			ss, err := e19Server(&calls).Connect(context.Background(), st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ss.Close() }()
			cs, err := e19Client(t).Connect(context.Background(), ct, &sdkmcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cs.Close() }()
			if got := cs.InitializeResult().ProtocolVersion; got != version {
				t.Fatalf("negotiated %q, want %q", got, version)
			}
			out := e19Call(t, cs, "confirm")
			if out["action"] != "accept" || out["calls"] != float64(2) {
				t.Fatalf("confirm = %v, want action accept after 2 handler calls", out)
			}
		})
	}
}

// A stateful streamable handler answers a newest-protocol client with
// 2025-11-25 and a session ID, and the client's capabilities reach the tool.
// A stateless one loses a legacy client's capabilities: no elicitation, which
// is why /mcp is stateful (design §5.2).
func TestE19StreamableStatefulKeepsCapabilities(t *testing.T) {
	for _, tc := range []struct {
		stateless   bool
		version     string
		wantVersion string
		wantSession bool
		wantHasCaps bool
	}{
		{stateless: false, version: "", wantVersion: "2025-11-25", wantSession: true, wantHasCaps: true},
		{stateless: true, version: "2025-11-25", wantVersion: "2025-11-25", wantSession: false, wantHasCaps: false},
	} {
		calls := 0
		s := e19Server(&calls)
		h := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return s }, &sdkmcp.StreamableHTTPOptions{Stateless: tc.stateless})
		ts := httptest.NewServer(h)
		cs, err := e19Client(t).Connect(context.Background(),
			&sdkmcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: ts.Client()},
			&sdkmcp.ClientSessionOptions{ProtocolVersion: tc.version})
		if err != nil {
			ts.Close()
			t.Fatal(err)
		}
		if got := cs.InitializeResult().ProtocolVersion; got != tc.wantVersion {
			t.Errorf("stateless=%v: negotiated %q, want %q", tc.stateless, got, tc.wantVersion)
		}
		if got := cs.ID() != ""; got != tc.wantSession {
			t.Errorf("stateless=%v: session ID present = %v, want %v", tc.stateless, got, tc.wantSession)
		}
		if got := e19Call(t, cs, "caps")["has_caps"]; got != tc.wantHasCaps {
			t.Errorf("stateless=%v: client elicitation capability seen = %v, want %v", tc.stateless, got, tc.wantHasCaps)
		}
		_ = cs.Close()
		ts.Close()
	}
}

func e19Post(t *testing.T, url, host, origin string) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"1"}}}`
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// On a loopback listener the SDK refuses any non-loopback Host, a configured
// hostname included, and never looks at Origin. The remote /mcp guard (Task 10)
// must therefore disable the SDK's check to admit [caldav] hostnames and do the
// Origin check itself.
func TestE19HostCheckedOriginNot(t *testing.T) {
	calls := 0
	s := e19Server(&calls)
	newH := func(disable bool) http.Handler {
		return sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return s },
			&sdkmcp.StreamableHTTPOptions{DisableLocalhostProtection: disable})
	}
	ts := httptest.NewServer(newH(false))
	defer ts.Close()
	off := httptest.NewServer(newH(true))
	defer off.Close()
	for _, tc := range []struct {
		name, url, host, origin string
		want                    int
	}{
		{"loopback Host", ts.URL, "", "", http.StatusOK},
		{"foreign Host", ts.URL, "evil.example", "", http.StatusForbidden},
		{"configured hostname", ts.URL, "pcb.example:8443", "", http.StatusForbidden},
		{"foreign Origin", ts.URL, "", "http://evil.example", http.StatusOK},
		{"configured hostname, protection off", off.URL, "pcb.example:8443", "", http.StatusOK},
	} {
		if got := e19Post(t, tc.url, tc.host, tc.origin); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}
