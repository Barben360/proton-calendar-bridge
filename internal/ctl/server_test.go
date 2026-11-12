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
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

const (
	idA   = "aaaa1111"
	idB   = "bbbb2222"
	addrA = "alice@example.com"
	addrB = "bob@example.com"
)

// stubHub is a ctl.Hub of the accounts in infos: it answers each one's
// Status and Health, and records the account-changing calls it gets. When
// entered is set, Login closes it and waits for release. Any other call
// of an account's API panics.
type stubHub struct {
	infos     []service.AccountInfo
	status    map[string]service.Status
	health    map[string]service.Health
	statusErr error
	loginFn   func(service.LoginRequest) (service.LoginResult, error)

	entered, release chan struct{}

	mu        sync.Mutex
	logins    []service.LoginRequest
	codes     []string
	codeFor   []string // the selector of each code
	logouts   int
	discard   []bool // each logout's Discard
	logoutFor []string
	removed   []string
	restarted []string
	caldav    []string // "selector=on|off"
}

// newHub is a hub of one account, idA, whose login is st.
func newHub(st service.Status) *stubHub {
	return &stubHub{
		infos:  []service.AccountInfo{{ID: idA, Address: addrA, Phase: st.Phase, CalDAV: true}},
		status: map[string]service.Status{idA: st},
		health: map[string]service.Health{},
	}
}

func (h *stubHub) find(selector string) (service.AccountInfo, error) {
	for _, i := range h.infos {
		if i.ID == selector || strings.EqualFold(i.Address, selector) {
			return i, nil
		}
	}
	return service.AccountInfo{}, &exitcode.Error{Code: exitcode.Usage, Err: errors.New("no account " + selector), Hint: "see pcb account list"}
}

func (h *stubHub) Accounts() []service.AccountInfo { return h.infos }

func (h *stubHub) Account(selector string) (service.API, service.AccountInfo, error) {
	i, err := h.find(selector)
	if err != nil {
		return nil, i, err
	}
	return statusAPI{h: h, id: i.ID}, i, nil
}

func (h *stubHub) Health(_ context.Context, selector string) (service.Health, error) {
	i, err := h.find(selector)
	return h.health[i.ID], err
}

func (h *stubHub) Login(_ context.Context, r service.LoginRequest) (service.LoginResult, error) {
	h.mu.Lock()
	h.logins = append(h.logins, r)
	fn := h.loginFn
	h.mu.Unlock()
	if h.entered != nil {
		close(h.entered)
		<-h.release
	}
	if fn != nil {
		return fn(r)
	}
	exp := now.Add(5 * time.Minute)
	return service.LoginResult{State: service.StateNeedsTOTP, Username: r.Username, AccountID: "id1", Account: idA, LoginExpires: &exp}, nil
}

func (h *stubHub) SubmitTOTP(_ context.Context, selector, code string) (service.LoginResult, error) {
	h.mu.Lock()
	h.codes = append(h.codes, code)
	h.codeFor = append(h.codeFor, selector)
	h.mu.Unlock()
	return service.LoginResult{State: service.StateReady, Username: "alice", AccountID: "id1", Account: idA}, nil
}

func (h *stubHub) SignOut(_ context.Context, selector string, r service.LogoutRequest) (service.LogoutResult, error) {
	if _, err := h.find(selector); err != nil {
		return service.LogoutResult{}, err
	}
	h.mu.Lock()
	h.logouts++
	h.discard = append(h.discard, r.Discard)
	h.logoutFor = append(h.logoutFor, selector)
	h.mu.Unlock()
	return service.LogoutResult{WasLoggedIn: true, Revoked: true}, nil
}

func (h *stubHub) Remove(_ context.Context, selector string, r service.LogoutRequest) (service.AccountInfo, error) {
	i, err := h.find(selector)
	if err == nil {
		h.mu.Lock()
		h.removed = append(h.removed, selector)
		h.discard = append(h.discard, r.Discard)
		h.mu.Unlock()
	}
	return i, err
}

func (h *stubHub) SetCalDAV(_ context.Context, selector string, on bool) (service.AccountInfo, error) {
	i, err := h.find(selector)
	if err == nil {
		h.mu.Lock()
		h.caldav = append(h.caldav, selector+"="+map[bool]string{true: "on", false: "off"}[on])
		h.mu.Unlock()
		i.CalDAV = on
	}
	return i, err
}

func (h *stubHub) Restart(_ context.Context, selector string) (service.AccountInfo, error) {
	i, err := h.find(selector)
	if err == nil {
		h.mu.Lock()
		h.restarted = append(h.restarted, selector)
		h.mu.Unlock()
	}
	return i, err
}

func (h *stubHub) calls() (logins, codes int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.logins), len(h.codes)
}

// statusAPI is the per-account API a Hub hands out: it answers Status only.
type statusAPI struct {
	service.API
	h  *stubHub
	id string
}

func (a statusAPI) Status(context.Context) (service.Status, error) {
	return a.h.status[a.id], a.h.statusErr
}

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
	hub := newHub(service.Status{Phase: "ready", Username: "alice", AccountID: "id1", Secrets: "sealed", KeyFile: "/k"})
	hub.health[idA] = *serving(cal(func(c *service.CalendarHealth) {
		c.Gate, c.GateReason = "undecided", "the keys were not loaded yet"
	}))
	ln := &ctl.Listener{Addr: "127.0.0.1:1080", URL: "http://localhost:1080"}
	d := daemon()
	d.Kind = "serve"
	s := serve(t, &ctl.Server{Hub: hub, Daemon: d, Listener: func() *ctl.Listener { return ln }, Now: func() time.Time { return now }})

	resp, env := raw(t, s.path, http.MethodGet, "/v2/status", nil)
	if resp.StatusCode != http.StatusOK || env["ok"] != true || env["command"] != "status" || env["schema_version"] != float64(1) {
		t.Fatalf("HTTP %d, envelope %v", resp.StatusCode, env)
	}
	data := env["data"].(map[string]any)
	dm := data["daemon"].(map[string]any)
	if dm["pid"] != float64(4242) || dm["version"] != "v1.2.3" || dm["log_level"] != "info" || dm["socket"] != "/s/pcb.sock" ||
		dm["started_at"] != "2026-11-12T11:00:00Z" || dm["kind"] != "serve" {
		t.Errorf("daemon = %v", dm)
	}
	if l := data["listener"].(map[string]any); l["url"] != "http://localhost:1080" || l["addr"] != "127.0.0.1:1080" || l["tls"] != false {
		t.Errorf("listener = %v", l)
	}
	accounts := data["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}
	a := accounts[0].(map[string]any)
	// The AccountInfo is flat; the login, health, URL and alerts are the account's own.
	if a["id"] != idA || a["address"] != addrA || a["caldav"] != true || a["url"] != "http://localhost:1080/dav/"+idA+"/" {
		t.Errorf("account = %v", a)
	}
	if l := a["login"].(map[string]any); l["phase"] != "ready" || l["username"] != "alice" || l["secrets"] != "sealed" {
		t.Errorf("login = %v", l)
	}
	if h := a["health"].(map[string]any); h["serve"] != "serving" {
		t.Errorf("health = %v", h)
	}

	c := dial(t, s.path)
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Daemon == nil || st.Daemon.PID != 4242 || st.Listener == nil || st.Listener.URL != ln.URL || len(st.Accounts) != 1 {
		t.Fatalf("Status = %+v", st)
	}
	as := st.Accounts[0]
	if as.ID != idA || as.Login.Phase != "ready" || as.URL != "http://localhost:1080/dav/"+idA+"/" ||
		as.Health == nil || len(as.Health.Calendars) != 1 || as.Health.Calendars[0].GateReason != "the keys were not loaded yet" {
		t.Fatalf("account = %+v", as)
	}
	if len(as.Alerts) != 1 || as.Alerts[0].Code != "write-gate-undecided" || as.Alerts[0].Calendar != "cal-1" || as.Alerts[0].Account != idA {
		t.Errorf("alerts = %+v; want the undecided gate's, of the account", as.Alerts)
	}
	if len(st.Alerts) != 0 {
		t.Errorf("process alerts = %+v; want none", st.Alerts)
	}
	if logs := s.logs.String(); strings.Contains(logs, "alice") {
		t.Errorf("logs carry the response:\n%s", logs)
	}
}

// Each account is its own entry, with its own alerts and its own CalDAV URL.
func TestStatusListsEveryAccount(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready", Username: "alice"})
	hub.infos = append(hub.infos, service.AccountInfo{ID: idB, Address: addrB, Phase: "needs-login"})
	hub.status[idB] = service.Status{Phase: "needs-login", Reason: "Proton ended the session"}
	// A trailing slash on the listener's base does not double up.
	ln := &ctl.Listener{Addr: "0.0.0.0:8443", URL: "https://pcb.lan:8443/", TLS: true}
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon(), Listener: func() *ctl.Listener { return ln }, Now: func() time.Time { return now }})

	st, err := dial(t, s.path).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Accounts) != 2 {
		t.Fatalf("accounts = %+v; want two", st.Accounts)
	}
	a, b := st.Accounts[0], st.Accounts[1]
	if a.ID != idA || a.URL != "https://pcb.lan:8443/dav/"+idA+"/" || len(a.Alerts) != 0 {
		t.Errorf("first account = %+v", a)
	}
	if b.ID != idB || b.URL != "https://pcb.lan:8443/dav/"+idB+"/" || b.Login.Phase != "needs-login" {
		t.Errorf("second account = %+v", b)
	}
	if len(b.Alerts) != 1 || b.Alerts[0].Code != "needs-login" || b.Alerts[0].Account != idB || b.Alerts[0].Message != "Proton ended the session" {
		t.Errorf("second account's alerts = %+v; want its own needs-login", b.Alerts)
	}
}

// GET /v2/accounts lists them all, pending ones too.
func TestAccountsRoute(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready"})
	hub.infos = append(hub.infos, service.AccountInfo{ID: idB, Address: addrB, Phase: "needs-captcha", Pending: true, CalDAV: true})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	got, err := dial(t, s.path).Accounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != idA || got[1].ID != idB || !got[1].Pending || got[1].Phase != "needs-captcha" {
		t.Errorf("Accounts = %+v", got)
	}
}

// A parked account is an error alert carrying the stop's message and hint.
func TestAccountStoppedAlert(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready"})
	hub.infos[0].Stopped = &service.StopInfo{Code: 20, Message: "the event cache is damaged", Hint: "remove pcb.db", At: now}
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon(), Now: func() time.Time { return now }})
	st, err := dial(t, s.path).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := ctl.Alert{Account: idA, Code: "account-stopped", Severity: "error", Message: "the event cache is damaged", Fix: "remove pcb.db"}
	if len(st.Accounts) != 1 || !slices.Equal(st.Accounts[0].Alerts, []ctl.Alert{want}) {
		t.Fatalf("alerts = %+v; want %+v", st.Accounts, want)
	}
	if got := st.Accounts[0].Stopped; got == nil || got.Code != 20 || got.Hint != "remove pcb.db" {
		t.Errorf("stopped = %+v", got)
	}
}

// A listener not published yet (CalDAV not bound) leaves out its URLs.
func TestStatusBeforeTheListener(t *testing.T) {
	t.Parallel()
	s := serve(t, &ctl.Server{Hub: newHub(service.Status{Phase: "ready"}), Daemon: daemon(),
		Listener: func() *ctl.Listener { return nil }})
	_, env := raw(t, s.path, http.MethodGet, "/v2/status", nil)
	data := env["data"].(map[string]any)
	if _, has := data["listener"]; has {
		t.Errorf("listener = %v before it was published", data["listener"])
	}
	if a, ok := data["alerts"].([]any); !ok || len(a) != 0 {
		t.Errorf("alerts = %#v, want []", data["alerts"])
	}
	a := data["accounts"].([]any)[0].(map[string]any)
	if _, has := a["url"]; has {
		t.Errorf("url = %v with no listener", a["url"])
	}
	if al, ok := a["alerts"].([]any); !ok || len(al) != 0 {
		t.Errorf("account alerts = %#v, want []", a["alerts"])
	}
}

// The certificate's alert is the process's: top level, no account.
func TestCertificateAlertIsTheProcesss(t *testing.T) {
	t.Parallel()
	ln := tlsListener(now.Add(time.Hour))
	s := serve(t, &ctl.Server{Hub: newHub(service.Status{Phase: "ready"}), Daemon: daemon(),
		Listener: func() *ctl.Listener { return ln }, Now: func() time.Time { return now }})
	st, err := dial(t, s.path).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alerts) != 1 || st.Alerts[0].Code != "tls-certificate-expiring" || st.Alerts[0].Account != "" {
		t.Errorf("process alerts = %+v", st.Alerts)
	}
	if len(st.Accounts[0].Alerts) != 0 {
		t.Errorf("the account's alerts = %+v; want none", st.Accounts[0].Alerts)
	}
}

// The daemon says which process it is.
func TestDaemonKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"serve", "ui"} {
		d := ctl.NewDaemon(kind, "/s/pcb.sock", "info", now)
		if d.Kind != kind || d.Socket != "/s/pcb.sock" || d.LogLevel != "info" || !d.StartedAt.Equal(now) || d.PID != os.Getpid() {
			t.Errorf("NewDaemon(%q) = %+v", kind, d)
		}
		s := serve(t, &ctl.Server{Hub: newHub(service.Status{Phase: "ready"}), Daemon: d})
		st, err := dial(t, s.path).Status(t.Context())
		if err != nil || st.Daemon == nil || st.Daemon.Kind != kind {
			t.Errorf("Status over the socket of a %q daemon: %+v, %v", kind, st.Daemon, err)
		}
	}
}

// M5-P3: on Linux a peer of another UID is refused, root included.
func TestOtherUIDIsRefused(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready", Username: "alice"})
	uid := os.Geteuid()
	refused := []int{uid + 1}
	if uid != 0 {
		refused = append(refused, 0) // root too (M5-P3)
	}
	for _, peer := range refused {
		s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon(), PeerUID: func(net.Conn) (int, bool) { return peer, true }})
		resp, env := raw(t, s.path, http.MethodGet, "/v2/status", nil)
		if resp.StatusCode != http.StatusForbidden || env["ok"] != false {
			t.Fatalf("peer %d: HTTP %d, envelope %v; want 403", peer, resp.StatusCode, env)
		}
		if _, has := env["data"]; has {
			t.Errorf("peer %d got data: %v", peer, env)
		}
		if logs := s.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "peer_uid="+strconv.Itoa(peer)) {
			t.Errorf("logs lack the warning naming the peer UID %d:\n%s", peer, logs)
		}
		// Nothing of the routes is learnt: an unknown path is refused the same.
		if resp, _ := raw(t, s.path, http.MethodGet, "/v2/nope", nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("peer %d: an unknown path answered HTTP %d, want 403", peer, resp.StatusCode)
		}
	}

	// Handler mounted without ConnContext: no connection to check, refused.
	rec := httptest.NewRecorder()
	(&ctl.Server{Hub: hub, Daemon: daemon()}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/status", nil))
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || rec.Code != http.StatusForbidden || env["ok"] != false {
		t.Errorf("no recorded connection: HTTP %d, %s (%v); want 403", rec.Code, rec.Body.Bytes(), err)
	}
	if _, has := env["data"]; has || strings.Contains(rec.Body.String(), "alice") {
		t.Errorf("no recorded connection got data: %s", rec.Body.Bytes())
	}

	// The same UID, or an unchecked peer, is served.
	for _, peer := range []func(net.Conn) (int, bool){
		func(net.Conn) (int, bool) { return uid, true },
		func(net.Conn) (int, bool) { return 0, false },
	} {
		s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon(), PeerUID: peer})
		if resp, env := raw(t, s.path, http.MethodGet, "/v2/status", nil); resp.StatusCode != http.StatusOK {
			t.Errorf("HTTP %d, envelope %v; want 200", resp.StatusCode, env)
		}
	}
}

func TestUnknownPathAndMethod(t *testing.T) {
	t.Parallel()
	s := serve(t, &ctl.Server{Hub: newHub(service.Status{}), Daemon: daemon()})
	for _, tc := range []struct {
		method, target, allow string
		want                  int
	}{
		{http.MethodGet, "/v2/nope", "", http.StatusNotFound},
		{http.MethodGet, "/status", "", http.StatusNotFound},
		{http.MethodGet, "/v2/accounts/" + idA + "/nope", "", http.StatusNotFound},
		{http.MethodPost, "/v2/status", http.MethodGet, http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v2/status", http.MethodGet, http.StatusMethodNotAllowed},
		{http.MethodPost, "/v2/accounts", http.MethodGet, http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/login", http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/accounts/" + idA + "/logout", http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/accounts/" + idA + "/remove", http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/accounts/" + idA + "/caldav", http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/accounts/" + idA + "/restart", http.MethodPost, http.StatusMethodNotAllowed},
		{http.MethodGet, "/v2/accounts/" + idA + "/login/totp", http.MethodPost, http.StatusMethodNotAllowed},
	} {
		resp, env := raw(t, s.path, tc.method, tc.target, nil)
		if resp.StatusCode != tc.want || env["ok"] != false || env["error"] == nil {
			t.Errorf("%s %s: HTTP %d, envelope %v; want %d with an error", tc.method, tc.target, resp.StatusCode, env, tc.want)
		}
		if resp.Header.Get("Allow") != tc.allow {
			t.Errorf("%s %s: Allow = %q, want %q", tc.method, tc.target, resp.Header.Get("Allow"), tc.allow)
		}
	}
}

// The v1 routes are gone: an old pcb is told so, not served.
func TestV1RoutesAreGone(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready"})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/v1/status"},
		{http.MethodPost, "/v1/login"},
		{http.MethodPost, "/v1/login/totp"},
		{http.MethodPost, "/v1/logout"},
	} {
		resp, env := raw(t, s.path, tc.method, tc.target, strings.NewReader("{}"))
		if resp.StatusCode != http.StatusNotFound || env["ok"] != false {
			t.Errorf("%s %s: HTTP %d, envelope %v; want 404", tc.method, tc.target, resp.StatusCode, env)
		}
	}
	if l, c := hub.calls(); l != 0 || c != 0 || hub.logouts != 0 {
		t.Error("a v1 route reached the hub")
	}
}

// Request bodies are at most 64 KiB.
func TestOversizedBodyIs413(t *testing.T) {
	t.Parallel()
	s := serve(t, &ctl.Server{Hub: newHub(service.Status{Phase: "ready"}), Daemon: daemon()})
	resp, env := raw(t, s.path, http.MethodGet, "/v2/status", strings.NewReader(strings.Repeat("x", 64<<10+1)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || env["ok"] != false {
		t.Errorf("64 KiB + 1: HTTP %d, envelope %v; want 413", resp.StatusCode, env)
	}
	// No length announced: the limit holds while reading.
	resp, env = raw(t, s.path, http.MethodGet, "/v2/status", io.MultiReader(strings.NewReader(strings.Repeat("x", 64<<10)), strings.NewReader("x")))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || env["ok"] != false {
		t.Errorf("64 KiB + 1, chunked: HTTP %d, envelope %v; want 413", resp.StatusCode, env)
	}
	resp, env = raw(t, s.path, http.MethodGet, "/v2/status", strings.NewReader(strings.Repeat("x", 64<<10)))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("64 KiB: HTTP %d, envelope %v; want 200", resp.StatusCode, env)
	}
}

// A command error is a 422 that the client turns back into the same
// *exitcode.Error.
func TestErrorsCrossTheSocket(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{})
	hub.statusErr = &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the key file is missing"), Hint: "restore /k/secrets.key"}
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	resp, env := raw(t, s.path, http.MethodGet, "/v2/status", nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("HTTP %d, envelope %v; want 422", resp.StatusCode, env)
	}
	c := dial(t, s.path)
	_, err := c.Status(t.Context())
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.MustAct || ee.Error() != "the key file is missing" || ee.Hint != "restore /k/secrets.key" {
		t.Fatalf("Status error = %#v; want code 10 with the message and hint", err)
	}

	// A transport error is blocked (20), never mistaken for the command's.
	s = serve(t, &ctl.Server{Hub: hub, Daemon: daemon(), PeerUID: func(net.Conn) (int, bool) { return os.Geteuid() + 1, true }})
	c = dial(t, s.path)
	if _, err := c.Status(t.Context()); exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "403") {
		t.Errorf("Status on a refusal = %v (exit %d); want blocked naming HTTP 403", err, exitcode.From(err))
	}
}

// A selector that names no account is exit 2 on every account route, and
// nothing reaches the Hub's state.
func TestUnknownAccountRoute(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready"})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	for _, action := range []string{"logout", "remove", "caldav", "restart"} {
		target := "/v2/accounts/zzzzzzzz/" + action
		resp, env := raw(t, s.path, http.MethodPost, target, strings.NewReader(`{"on":true}`))
		if resp.StatusCode != http.StatusUnprocessableEntity || env["ok"] != false {
			t.Errorf("POST %s: HTTP %d, envelope %v; want 422", target, resp.StatusCode, env)
			continue
		}
		e := env["error"].(map[string]any)
		if e["code"] != float64(exitcode.Usage) || !strings.Contains(e["message"].(string), "no account") {
			t.Errorf("POST %s: error = %v; want exit 2, no account", target, e)
		}
	}
	c := dial(t, s.path)
	_, err := c.Logout(t.Context(), "zzzzzzzz", service.LogoutRequest{})
	if exitcode.From(err) != exitcode.Usage || !strings.Contains(err.Error(), "no account") {
		t.Errorf("Logout of an unknown account = %v (exit %d); want exit 2", err, exitcode.From(err))
	}
	if len(hub.removed)+len(hub.restarted)+len(hub.caldav)+hub.logouts != 0 {
		t.Errorf("the hub changed something: %+v", hub)
	}
}

// Each account route reaches its Hub method, for the account the path names
// (its address too, escaped), with the body's fields.
func TestAccountRoutesReachTheHub(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready"})
	hub.infos = append(hub.infos, service.AccountInfo{ID: idB, Address: addrB, CalDAV: true})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	c := dial(t, s.path)
	ctx := t.Context()

	if _, err := c.Logout(ctx, idB, service.LogoutRequest{Discard: true}); err != nil {
		t.Fatal(err)
	}
	info, err := c.Remove(ctx, addrA, service.LogoutRequest{}) // by address: "@" is escaped in the path
	if err != nil || info.ID != idA {
		t.Fatalf("Remove = %+v, %v", info, err)
	}
	info, err = c.SetCalDAV(ctx, idB, false)
	if err != nil || info.ID != idB || info.CalDAV {
		t.Fatalf("SetCalDAV off = %+v, %v", info, err)
	}
	if info, err = c.SetCalDAV(ctx, idB, true); err != nil || !info.CalDAV {
		t.Fatalf("SetCalDAV on = %+v, %v", info, err)
	}
	if info, err = c.Restart(ctx, idA); err != nil || info.ID != idA {
		t.Fatalf("Restart = %+v, %v", info, err)
	}

	hub.mu.Lock()
	defer hub.mu.Unlock()
	if !slices.Equal(hub.logoutFor, []string{idB}) || !slices.Equal(hub.removed, []string{addrA}) ||
		!slices.Equal(hub.caldav, []string{idB + "=off", idB + "=on"}) || !slices.Equal(hub.restarted, []string{idA}) {
		t.Errorf("hub calls: logout %v, remove %v, caldav %v, restart %v", hub.logoutFor, hub.removed, hub.caldav, hub.restarted)
	}
	if !slices.Equal(hub.discard, []bool{true, false}) {
		t.Errorf("Discard of the logout and the remove = %v, want [true false]", hub.discard)
	}
}

// The caldav switch must be given: a body without "on" is no request.
func TestCalDAVNeedsItsSwitch(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{Phase: "ready"})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	for _, body := range []string{``, `{}`, `{"on":`, `{"on":"yes"}`} {
		resp, env := raw(t, s.path, http.MethodPost, "/v2/accounts/"+idA+"/caldav", strings.NewReader(body))
		if resp.StatusCode != http.StatusBadRequest || env["ok"] != false {
			t.Errorf("body %q: HTTP %d, envelope %v; want 400", body, resp.StatusCode, env)
		}
	}
	if len(hub.caldav) != 0 {
		t.Errorf("a refused body switched CalDAV: %v", hub.caldav)
	}
}

// The 2FA code goes to the account whose path carries it, not another.
func TestTOTPGoesToItsAccount(t *testing.T) {
	t.Parallel()
	hub := newHub(service.Status{})
	hub.infos = append(hub.infos, service.AccountInfo{ID: idB, Address: addrB})
	s := serve(t, &ctl.Server{Hub: hub, Daemon: daemon()})
	c := dial(t, s.path)
	if _, err := c.SubmitTOTP(t.Context(), idB, "654321"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubmitTOTP(t.Context(), addrA, "111111"); err != nil { // an address is a selector too
		t.Fatal(err)
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if !slices.Equal(hub.codeFor, []string{idB, addrA}) || !slices.Equal(hub.codes, []string{"654321", "111111"}) {
		t.Errorf("the hub got codes %v for %v", hub.codes, hub.codeFor)
	}
}

// The login's result names the account it is for.
func TestLoginResultNamesItsAccount(t *testing.T) {
	t.Parallel()
	s := serve(t, &ctl.Server{Hub: newHub(service.Status{}), Daemon: daemon()})
	res, err := dial(t, s.path).Login(t.Context(), service.LoginRequest{Username: addrA, Password: "pw"})
	if err != nil || res.Account != idA {
		t.Errorf("Login = %+v, %v; want Account %s", res, err, idA)
	}
}

func dial(t *testing.T, path string) *ctl.Client {
	t.Helper()
	c, err := ctl.Dial(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// M5-Rpre4: the secrets travel in the request bodies only; the daemon's
// logs, at debug level, carry each request's path and exit code, never a
// password or a code.
func TestLoginOverTheSocketNeverLogsSecrets(t *testing.T) {
	t.Parallel()
	const password, totp = "pw-S3CRET-hunter2", "246810"
	api := newHub(service.Status{})
	s := serve(t, &ctl.Server{Hub: api, Daemon: daemon()})
	c := dial(t, s.path)

	req := service.LoginRequest{Username: "alice", Password: password, CaptchaToken: "cap-1", UnattendedRelogin: true}
	res, err := c.Login(t.Context(), req)
	if err != nil || res.State != service.StateNeedsTOTP || res.LoginExpires == nil || !res.LoginExpires.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("Login = %+v, %v", res, err)
	}
	res, err = c.SubmitTOTP(t.Context(), idA, totp)
	if err != nil || res.State != service.StateReady || res.AccountID != "id1" {
		t.Fatalf("SubmitTOTP = %+v, %v", res, err)
	}
	out, err := c.Logout(t.Context(), idA, service.LogoutRequest{})
	if err != nil || !out.WasLoggedIn || !out.Revoked {
		t.Fatalf("Logout = %+v, %v", out, err)
	}
	// The discard of a store this pcb cannot read crosses (state-I1).
	if _, err := c.Logout(t.Context(), idA, service.LogoutRequest{Discard: true}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	if want := []bool{false, true}; !slices.Equal(api.discard, want) {
		t.Errorf("the daemon's logouts had Discard %v, want %v", api.discard, want)
	}
	api.logouts--
	if len(api.logins) != 1 || api.logins[0] != req {
		t.Errorf("the daemon got %d logins, want the request as sent", len(api.logins))
	}
	if len(api.codes) != 1 || api.codes[0] != totp || api.logouts != 1 {
		t.Errorf("the daemon got %d codes and %d logouts, want the code as sent and 1", len(api.codes), api.logouts)
	}
	// A command error crosses with its code and hint, and is logged by code.
	api.loginFn = func(service.LoginRequest) (service.LoginResult, error) {
		return service.LoginResult{}, &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("Proton refused the password"), Hint: "check the password"}
	}
	api.mu.Unlock()
	_, err = c.Login(t.Context(), req)
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.MustAct || ee.Error() != "Proton refused the password" || ee.Hint != "check the password" {
		t.Fatalf("Login error = %#v", err)
	}

	logs := s.logs.String()
	for _, secret := range []string{password, totp, "cap-1"} {
		if strings.Contains(logs, secret) {
			t.Errorf("the logs carry a secret:\n%s", logs)
		}
	}
	for _, want := range []string{"path=/v2/login ", "path=/v2/accounts/" + idA + "/login/totp ", "path=/v2/accounts/" + idA + "/logout ", "exit=0", "exit=10"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs lack %q:\n%s", want, logs)
		}
	}
}

// Request bodies are at most 64 KiB on the login endpoints too, and an
// oversized or malformed one never reaches the service.
func TestLoginBodyIsBounded(t *testing.T) {
	t.Parallel()
	api := newHub(service.Status{})
	s := serve(t, &ctl.Server{Hub: api, Daemon: daemon()})
	big := `{"username":"alice","password":"` + strings.Repeat("x", 64<<10) + `"}`
	for _, target := range []string{"/v2/login", "/v2/accounts/" + idA + "/login/totp"} {
		resp, env := raw(t, s.path, http.MethodPost, target, strings.NewReader(big))
		if resp.StatusCode != http.StatusRequestEntityTooLarge || env["ok"] != false {
			t.Errorf("POST %s, 64 KiB + : HTTP %d, envelope %v; want 413", target, resp.StatusCode, env)
		}
		resp, env = raw(t, s.path, http.MethodPost, target, strings.NewReader(`{"username":`))
		if resp.StatusCode != http.StatusBadRequest || env["ok"] != false {
			t.Errorf("POST %s, malformed: HTTP %d, envelope %v; want 400", target, resp.StatusCode, env)
		}
		if resp, env := raw(t, s.path, http.MethodGet, target, nil); resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: HTTP %d, envelope %v; want 405", target, resp.StatusCode, env)
		}
	}
	if l, c := api.calls(); l != 0 || c != 0 {
		t.Errorf("the service got %d logins and %d codes from refused bodies", l, c)
	}
	if strings.Contains(s.logs.String(), "xxxx") {
		t.Error("the logs carry the body")
	}
}

// A second login while one runs is refused at once (exit 10), not queued.
func TestSecondLoginWhileOneRunsIsRefused(t *testing.T) {
	t.Parallel()
	api := newHub(service.Status{})
	api.entered, api.release = make(chan struct{}), make(chan struct{})
	s := serve(t, &ctl.Server{Hub: api, Daemon: daemon()})
	first := make(chan error, 1)
	go func() {
		c, err := ctl.Dial(context.Background(), s.path)
		if err != nil {
			first <- err
			return
		}
		defer func() { _ = c.Close() }()
		_, err = c.Login(context.Background(), service.LoginRequest{Username: "alice", Password: "pw"})
		first <- err
	}()
	<-api.entered
	c := dial(t, s.path)
	// Bounded: a login queued behind the first would wait for ever.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for name, call := range map[string]func() error{
		"Login": func() error {
			_, err := c.Login(ctx, service.LoginRequest{Username: "bob", Password: "pw"})
			return err
		},
		"SubmitTOTP": func() error { _, err := c.SubmitTOTP(ctx, idA, "123456"); return err },
	} {
		err := call()
		var ee *exitcode.Error
		if !errors.As(err, &ee) || ee.Code != exitcode.MustAct || ee.Error() != "a login is already in progress" || ee.Hint == "" {
			t.Errorf("%s during a login = %#v; want exit 10, a login is already in progress", name, err)
		}
	}
	close(api.release)
	if err := <-first; err != nil {
		t.Fatalf("the first login: %v", err)
	}
	if l, c := api.calls(); l != 1 || c != 0 {
		t.Errorf("the service got %d logins and %d codes, want only the first login", l, c)
	}
	// Once it is done, the next one goes through.
	if _, err := c.SubmitTOTP(t.Context(), idA, "123456"); err != nil {
		t.Errorf("SubmitTOTP after the login: %v", err)
	}
}

// Re-review N1: the client checks the UID of the process that accepted its
// connection (SO_PEERCRED on Linux) before it sends anything: a socket
// swapped between the lstat and the dial never gets a login.
func TestClientRefusesAServerOfAnotherUID(t *testing.T) {
	t.Parallel()
	api := newHub(service.Status{})
	s := serve(t, &ctl.Server{Hub: api, Daemon: daemon()})
	other := func(net.Conn) (int, bool) { return os.Geteuid() + 1, true }
	c, err := ctl.DialCheckingPeer(t.Context(), s.path, other)
	if err == nil {
		_, err = c.Login(t.Context(), service.LoginRequest{Username: "alice", Password: "pw"})
		_ = c.Close()
	}
	if exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "uid "+strconv.Itoa(os.Geteuid()+1)) {
		t.Fatalf("a server of another UID: %v (exit %d); want blocked naming its UID", err, exitcode.From(err))
	}
	if l, _ := api.calls(); l != 0 {
		t.Errorf("the service got %d logins", l)
	}
	if logs := s.logs.String(); strings.Contains(logs, "control request") {
		t.Errorf("a request reached the server:\n%s", logs)
	}

	// The same UID, or an unchecked peer (other Unix), is talked to.
	for _, peer := range []func(net.Conn) (int, bool){
		func(net.Conn) (int, bool) { return os.Geteuid(), true },
		func(net.Conn) (int, bool) { return 0, false },
	} {
		c, err := ctl.DialCheckingPeer(t.Context(), s.path, peer)
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Login(t.Context(), service.LoginRequest{Username: "alice", Password: "pw"})
		_ = c.Close()
		if err != nil {
			t.Errorf("Login: %v", err)
		}
	}
	// The real check passes against this test's own server.
	if _, err := dial(t, s.path).Logout(t.Context(), idA, service.LogoutRequest{}); err != nil {
		t.Errorf("Logout over the real peer check: %v", err)
	}
}

// countingListener counts the connections it accepts.
type countingListener struct {
	net.Listener
	mu sync.Mutex
	n  int
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.n++
		l.mu.Unlock()
	}
	return c, err
}

func (l *countingListener) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// The connection Dial made is used only while fresh: pcb login dials, then
// prompts, and the server drops a connection that sent no request within
// its 5 s header timeout. A stale one is replaced, not sent on.
func TestStaleFirstConnectionIsRedialled(t *testing.T) {
	t.Parallel()
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
	cl := &countingListener{Listener: ln}
	srv := &ctl.Server{Hub: newHub(service.Status{}), Daemon: daemon()}
	go func() { _ = srv.Serve(cl) }()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	for _, tc := range []struct {
		wait time.Duration
		want int // connections accepted in all
	}{{time.Second, 1}, {3 * time.Second, 3}} {
		clock := time.Now()
		c, err := ctl.DialWithClock(t.Context(), path, func() time.Time { return clock })
		if err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(tc.wait) // the person types the password
		if _, err := c.Login(t.Context(), service.LoginRequest{Username: "alice", Password: "pw"}); err != nil {
			t.Fatalf("Login after %v: %v", tc.wait, err)
		}
		_ = c.Close()
		if n := cl.count(); n != tc.want {
			t.Errorf("after a wait of %v: %d connections accepted in all, want %d", tc.wait, n, tc.want)
		}
	}
}

// Ruling M5-R12 (5): the redial that replaces a stale first connection
// checks its server's UID too, before anything is sent.
func TestRedialChecksTheServersUID(t *testing.T) {
	t.Parallel()
	api := newHub(service.Status{})
	s := serve(t, &ctl.Server{Hub: api, Daemon: daemon()})
	var mu sync.Mutex
	dials := 0
	peer := func(net.Conn) (int, bool) { // the first server is ours, the redial reaches another UID's
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return os.Geteuid(), true
		}
		return os.Geteuid() + 1, true
	}
	clock := time.Now()
	c, err := ctl.DialForTest(t.Context(), s.path, func() time.Time { return clock }, peer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	clock = clock.Add(3 * time.Second) // the first connection is stale: the login redials
	_, err = c.Login(t.Context(), service.LoginRequest{Username: "alice", Password: "pw"})
	if exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "uid "+strconv.Itoa(os.Geteuid()+1)) {
		t.Fatalf("Login over a redial to another UID: %v (exit %d); want blocked naming its UID", err, exitcode.From(err))
	}
	mu.Lock()
	n := dials
	mu.Unlock()
	if n != 2 {
		t.Errorf("%d peer checks, want 2 (the dial and the redial)", n)
	}
	if l, _ := api.calls(); l != 0 {
		t.Errorf("the service got %d logins", l)
	}
	if logs := s.logs.String(); strings.Contains(logs, "control request") {
		t.Errorf("a request reached the server:\n%s", logs)
	}
}

// The real Hub is what the socket serves.
var _ ctl.Hub = (*service.Hub)(nil)
