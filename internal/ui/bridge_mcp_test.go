package ui_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
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
