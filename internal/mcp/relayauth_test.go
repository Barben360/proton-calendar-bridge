package mcp_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
)

// relayRequest is a request of the control socket's /mcp carrying header
// as X-Pcb-Mcp-Grant (none: no header).
func relayRequest(header string, set bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initialize))
	if set {
		r.Header.Set(mcp.RelayGrantHeader, header)
	}
	return r
}

func grantHeader(t *testing.T, g mcp.Grant) string {
	t.Helper()
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The relay's grant comes from its header, re-validated: a missing or
// unreadable header, or capabilities ParseCaps refuses, is 400; a valid
// one is the grant, entry relay, named after its profile.
func TestRelayAuthNeedsTheGrantHeader(t *testing.T) {
	t.Parallel()
	full, err := mcp.Profile("full")
	if err != nil {
		t.Fatal(err)
	}
	full.Accounts = []string{"alice@example.com"}
	readOnly, _ := mcp.Profile("read-only")

	for _, tc := range []struct {
		name, header string
		set          bool
	}{
		{"no header", "", false},
		{"empty header", "", true},
		{"not JSON", "read,login", true},
		{"two values", grantHeader(t, readOnly) + " {}", true},
		{"unknown field", `{"Name":"full","Caps":["read"],"Admin":true}`, true},
		{"no capability", `{"Name":"custom","Caps":[]}`, true},
		{"unknown capability", `{"Name":"custom","Caps":["read","root"]}`, true},
		{"delete without write", `{"Name":"custom","Caps":["read","delete"]}`, true},
	} {
		_, _, _, err := mcp.RelayAuth(relayRequest(tc.header, tc.set))
		var ae *mcp.AuthError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Errorf("%s: RelayAuth = %v, want an AuthError 400", tc.name, err)
		}
	}

	for _, tc := range []struct {
		name     string
		sent     mcp.Grant
		wantName string
		wantCaps []mcp.Capability
	}{
		{"full profile", full, "stdio:full", full.Caps},
		{"read-only profile", readOnly, "stdio:read-only", readOnly.Caps},
		// --allow: the caps are re-parsed (write implies read and freebusy).
		{"custom", mcp.Grant{Name: "custom", Caps: []mcp.Capability{mcp.CapWrite}}, "stdio:custom",
			[]mcp.Capability{mcp.CapFreeBusy, mcp.CapRead, mcp.CapWrite}},
		// A profile's name on other capabilities is not that profile.
		{"profile name on other caps", mcp.Grant{Name: "full", Caps: []mcp.Capability{mcp.CapRead}}, "stdio:custom",
			[]mcp.Capability{mcp.CapFreeBusy, mcp.CapRead}},
	} {
		g, entry, name, err := mcp.RelayAuth(relayRequest(grantHeader(t, tc.sent), true))
		if err != nil {
			t.Errorf("%s: RelayAuth = %v", tc.name, err)
			continue
		}
		if entry != mcp.EntryRelay || name != tc.wantName || g.Name != tc.wantName {
			t.Errorf("%s: entry %q, name %q, grant name %q; want relay, %q", tc.name, entry, name, g.Name, tc.wantName)
		}
		if !slices.Equal(g.Caps, tc.wantCaps) || !slices.Equal(g.Accounts, tc.sent.Accounts) {
			t.Errorf("%s: grant %+v, want caps %v accounts %v", tc.name, g, tc.wantCaps, tc.sent.Accounts)
		}
	}
}

// Behind HTTPHandler, a relay without its header is answered 400 and
// never reaches the SDK; with it, the session has the header's tools.
func TestRelayAuthBehindTheHandler(t *testing.T) {
	t.Parallel()
	srv := mcp.NewServer(mcp.Options{Hub: newHub(), Now: func() time.Time { return fixedNow }, Zone: paris})
	ts := httptest.NewServer(srv.HTTPHandler(mcp.RelayAuth))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp", strings.NewReader(initialize))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || res.Header.Get("Mcp-Session-Id") != "" {
		t.Errorf("relay without a grant: %d, session %q; want 400 and no session", res.StatusCode, res.Header.Get("Mcp-Session-Id"))
	}

	readOnly, _ := mcp.Profile("read-only")
	hc := &http.Client{Transport: headerRT{key: mcp.RelayGrantHeader, value: grantHeader(t, readOnly)}}
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "relay", Version: "0"}, nil).
		Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	// read-only: the read tools, no write tool.
	if !slices.Contains(names, "events_list") || slices.Contains(names, "event_create") {
		t.Errorf("relay read-only tools = %v", names)
	}
	if got := srv.LiveSessions(); got[mcp.EntryRelay] != 1 {
		t.Errorf("live sessions = %v, want one relay", got)
	}
}
