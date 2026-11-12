package ctl_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// fakeAPI answers Status and Health; any other call panics.
type fakeAPI struct {
	service.API
	status    service.Status
	statusErr error
	health    service.Health
}

func (f *fakeAPI) Status(context.Context) (service.Status, error) { return f.status, f.statusErr }
func (f *fakeAPI) Health(context.Context) (service.Health, error) { return f.health, nil }

// lockedBuffer collects the server's logs.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type served struct {
	path string
	logs *lockedBuffer
}

// serve runs srv on a fresh socket until the test ends.
func serve(t *testing.T, srv *ctl.Server) served {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	dir, err := os.MkdirTemp("", "pcb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "pcb.sock")
	ln, err := ctl.Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	if srv.Logger == nil {
		srv.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve returned %v", err)
		}
	})
	return served{path: path, logs: logs}
}

// raw sends one request over the socket and returns the status and the
// decoded envelope.
func raw(t *testing.T, path, method, target string, body io.Reader) (*http.Response, map[string]any) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(t.Context(), method, "http://pcb"+target, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var env map[string]any
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("%s %s: HTTP %d, not an envelope: %v", method, target, resp.StatusCode, err)
	}
	if dec.More() {
		t.Fatalf("%s %s: more than one JSON value", method, target)
	}
	return resp, env
}

func daemon() ctl.Daemon {
	return ctl.Daemon{PID: 4242, Version: "v1.2.3", StartedAt: now.Add(-time.Hour), LogLevel: "info", Socket: "/s/pcb.sock"}
}

func TestStatusOverTheSocket(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{
		status: service.Status{Phase: "ready", Username: "alice", AccountID: "id1", Secrets: "sealed", KeyFile: "/k"},
		health: *serving(cal(func(c *service.CalendarHealth) {
			c.Gate, c.GateReason = "undecided", "the keys were not loaded yet"
		})),
	}
	ln := &ctl.Listener{Addr: "127.0.0.1:1080", URL: "http://localhost:1080/"}
	s := serve(t, &ctl.Server{API: api, Daemon: daemon(), Listener: func() *ctl.Listener { return ln }, Now: func() time.Time { return now }})

	resp, env := raw(t, s.path, http.MethodGet, "/v1/status", nil)
	if resp.StatusCode != http.StatusOK || env["ok"] != true || env["command"] != "status" || env["schema_version"] != float64(1) {
		t.Fatalf("HTTP %d, envelope %v", resp.StatusCode, env)
	}
	data := env["data"].(map[string]any)
	// M5-P4: service.Status's fields stay flat at the top level.
	if data["phase"] != "ready" || data["username"] != "alice" || data["secrets"] != "sealed" {
		t.Errorf("flat status fields = %v", data)
	}
	d := data["daemon"].(map[string]any)
	if d["pid"] != float64(4242) || d["version"] != "v1.2.3" || d["log_level"] != "info" || d["socket"] != "/s/pcb.sock" || d["started_at"] != "2026-11-12T11:00:00Z" {
		t.Errorf("daemon = %v", d)
	}
	if l := data["listener"].(map[string]any); l["url"] != "http://localhost:1080/" || l["addr"] != "127.0.0.1:1080" || l["tls"] != false {
		t.Errorf("listener = %v", l)
	}
	if h := data["health"].(map[string]any); h["serve"] != "serving" {
		t.Errorf("health = %v", h)
	}

	c, err := ctl.Dial(t.Context(), s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "ready" || st.Daemon == nil || st.Daemon.PID != 4242 || st.Listener == nil || st.Listener.URL != ln.URL ||
		st.Health == nil || len(st.Health.Calendars) != 1 || st.Health.Calendars[0].GateReason != "the keys were not loaded yet" {
		t.Fatalf("Status = %+v", st)
	}
	if len(st.Alerts) != 1 || st.Alerts[0].Code != "write-gate-undecided" || st.Alerts[0].Calendar != "cal-1" {
		t.Errorf("alerts = %+v; want the undecided gate's", st.Alerts)
	}
	if logs := s.logs.String(); strings.Contains(logs, "alice") {
		t.Errorf("logs carry the response:\n%s", logs)
	}
}

// A listener not published yet (CalDAV not bound) is left out.
func TestStatusBeforeTheListener(t *testing.T) {
	t.Parallel()
	s := serve(t, &ctl.Server{API: &fakeAPI{status: service.Status{Phase: "ready"}}, Daemon: daemon(),
		Listener: func() *ctl.Listener { return nil }})
	_, env := raw(t, s.path, http.MethodGet, "/v1/status", nil)
	data := env["data"].(map[string]any)
	if _, has := data["listener"]; has {
		t.Errorf("listener = %v before it was published", data["listener"])
	}
	if a, ok := data["alerts"].([]any); !ok || len(a) != 0 {
		t.Errorf("alerts = %#v, want []", data["alerts"])
	}
}

// M5-P3: on Linux a peer of another UID is refused, root included.
func TestOtherUIDIsRefused(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{status: service.Status{Phase: "ready", Username: "alice"}}
	uid := os.Getuid()
	s := serve(t, &ctl.Server{API: api, Daemon: daemon(), PeerUID: func(net.Conn) (int, bool) { return uid + 1, true }})
	resp, env := raw(t, s.path, http.MethodGet, "/v1/status", nil)
	if resp.StatusCode != http.StatusForbidden || env["ok"] != false {
		t.Fatalf("HTTP %d, envelope %v; want 403", resp.StatusCode, env)
	}
	if _, has := env["data"]; has {
		t.Errorf("a refused peer got data: %v", env)
	}
	if logs := s.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "peer_uid="+strconv.Itoa(uid+1)) {
		t.Errorf("logs lack the warning naming the peer UID:\n%s", logs)
	}

	// The same UID, or an unchecked peer, is served.
	for _, peer := range []func(net.Conn) (int, bool){
		func(net.Conn) (int, bool) { return uid, true },
		func(net.Conn) (int, bool) { return 0, false },
	} {
		s := serve(t, &ctl.Server{API: api, Daemon: daemon(), PeerUID: peer})
		if resp, env := raw(t, s.path, http.MethodGet, "/v1/status", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("HTTP %d, envelope %v; want 200", resp.StatusCode, env)
		}
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	t.Parallel()
	s := serve(t, &ctl.Server{API: &fakeAPI{}, Daemon: daemon()})
	for _, tc := range []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/v1/nope", http.StatusNotFound},
		{http.MethodGet, "/status", http.StatusNotFound},
		{http.MethodPost, "/v1/status", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/status", http.StatusMethodNotAllowed},
	} {
		resp, env := raw(t, s.path, tc.method, tc.target, nil)
		if resp.StatusCode != tc.want || env["ok"] != false || env["error"] == nil {
			t.Errorf("%s %s: HTTP %d, envelope %v; want %d with an error", tc.method, tc.target, resp.StatusCode, env, tc.want)
		}
		if tc.want == http.StatusMethodNotAllowed && resp.Header.Get("Allow") != http.MethodGet {
			t.Errorf("%s %s: Allow = %q", tc.method, tc.target, resp.Header.Get("Allow"))
		}
	}
}

// Request bodies are at most 64 KiB.
func TestOversizedBodyIs413(t *testing.T) {
	t.Parallel()
	s := serve(t, &ctl.Server{API: &fakeAPI{status: service.Status{Phase: "ready"}}, Daemon: daemon()})
	resp, env := raw(t, s.path, http.MethodGet, "/v1/status", strings.NewReader(strings.Repeat("x", 64<<10+1)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || env["ok"] != false {
		t.Errorf("64 KiB + 1: HTTP %d, envelope %v; want 413", resp.StatusCode, env)
	}
	// No length announced: the limit holds while reading.
	resp, env = raw(t, s.path, http.MethodGet, "/v1/status", io.MultiReader(strings.NewReader(strings.Repeat("x", 64<<10)), strings.NewReader("x")))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || env["ok"] != false {
		t.Errorf("64 KiB + 1, chunked: HTTP %d, envelope %v; want 413", resp.StatusCode, env)
	}
	resp, env = raw(t, s.path, http.MethodGet, "/v1/status", strings.NewReader(strings.Repeat("x", 64<<10)))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("64 KiB: HTTP %d, envelope %v; want 200", resp.StatusCode, env)
	}
}

// A command error is a 422 that the client turns back into the same
// *exitcode.Error.
func TestErrorsCrossTheSocket(t *testing.T) {
	t.Parallel()
	api := &fakeAPI{statusErr: &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the key file is missing"), Hint: "restore /k/secrets.key"}}
	s := serve(t, &ctl.Server{API: api, Daemon: daemon()})
	resp, env := raw(t, s.path, http.MethodGet, "/v1/status", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("HTTP %d, envelope %v; want 422", resp.StatusCode, env)
	}
	c, err := ctl.Dial(t.Context(), s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, err = c.Status(t.Context())
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.MustAct || ee.Error() != "the key file is missing" || ee.Hint != "restore /k/secrets.key" {
		t.Fatalf("Status error = %#v; want code 10 with the message and hint", err)
	}

	// A transport error is blocked (20), never mistaken for the command's.
	s = serve(t, &ctl.Server{API: api, Daemon: daemon(), PeerUID: func(net.Conn) (int, bool) { return os.Getuid() + 1, true }})
	c, err = ctl.Dial(t.Context(), s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Status(t.Context()); exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "403") {
		t.Errorf("Status on a refusal = %v (exit %d); want blocked naming HTTP 403", err, exitcode.From(err))
	}
}
