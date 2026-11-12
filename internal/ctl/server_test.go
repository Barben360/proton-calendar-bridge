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
	uid := os.Geteuid()
	refused := []int{uid + 1}
	if uid != 0 {
		refused = append(refused, 0) // root too (M5-P3)
	}
	for _, peer := range refused {
		s := serve(t, &ctl.Server{API: api, Daemon: daemon(), PeerUID: func(net.Conn) (int, bool) { return peer, true }})
		resp, env := raw(t, s.path, http.MethodGet, "/v1/status", nil)
		if resp.StatusCode != http.StatusForbidden || env["ok"] != false {
			t.Fatalf("peer %d: HTTP %d, envelope %v; want 403", peer, resp.StatusCode, env)
		}
		if _, has := env["data"]; has {
			t.Errorf("peer %d got data: %v", peer, env)
		}
		if logs := s.logs.String(); !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "peer_uid="+strconv.Itoa(peer)) {
			t.Errorf("logs lack the warning naming the peer UID %d:\n%s", peer, logs)
		}
	}

	// Handler mounted without ConnContext: no connection to check, refused.
	rec := httptest.NewRecorder()
	(&ctl.Server{API: api, Daemon: daemon()}).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
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
	s = serve(t, &ctl.Server{API: api, Daemon: daemon(), PeerUID: func(net.Conn) (int, bool) { return os.Geteuid() + 1, true }})
	c, err = ctl.Dial(t.Context(), s.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Status(t.Context()); exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "403") {
		t.Errorf("Status on a refusal = %v (exit %d); want blocked naming HTTP 403", err, exitcode.From(err))
	}
}

// loginAPI records the login calls it answers. When entered is set, Login
// closes it and waits for release.
type loginAPI struct {
	fakeAPI
	mu      sync.Mutex
	logins  []service.LoginRequest
	codes   []string
	logouts int
	loginFn func(service.LoginRequest) (service.LoginResult, error)

	entered, release chan struct{}
}

func (a *loginAPI) Login(_ context.Context, r service.LoginRequest) (service.LoginResult, error) {
	a.mu.Lock()
	a.logins = append(a.logins, r)
	fn := a.loginFn
	a.mu.Unlock()
	if a.entered != nil {
		close(a.entered)
		<-a.release
	}
	if fn != nil {
		return fn(r)
	}
	exp := now.Add(5 * time.Minute)
	return service.LoginResult{State: service.StateNeedsTOTP, Username: r.Username, AccountID: "id1", LoginExpires: &exp}, nil
}

func (a *loginAPI) SubmitTOTP(_ context.Context, code string) (service.LoginResult, error) {
	a.mu.Lock()
	a.codes = append(a.codes, code)
	a.mu.Unlock()
	return service.LoginResult{State: service.StateReady, Username: "alice", AccountID: "id1"}, nil
}

func (a *loginAPI) Logout(context.Context) (service.LogoutResult, error) {
	a.mu.Lock()
	a.logouts++
	a.mu.Unlock()
	return service.LogoutResult{WasLoggedIn: true, Revoked: true}, nil
}

func (a *loginAPI) calls() (logins, codes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.logins), len(a.codes)
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
	api := &loginAPI{}
	s := serve(t, &ctl.Server{API: api, Daemon: daemon()})
	c := dial(t, s.path)

	req := service.LoginRequest{Username: "alice", Password: password, CaptchaToken: "cap-1", UnattendedRelogin: true}
	res, err := c.Login(t.Context(), req)
	if err != nil || res.State != service.StateNeedsTOTP || res.LoginExpires == nil || !res.LoginExpires.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("Login = %+v, %v", res, err)
	}
	res, err = c.SubmitTOTP(t.Context(), totp)
	if err != nil || res.State != service.StateReady || res.AccountID != "id1" {
		t.Fatalf("SubmitTOTP = %+v, %v", res, err)
	}
	out, err := c.Logout(t.Context())
	if err != nil || !out.WasLoggedIn || !out.Revoked {
		t.Fatalf("Logout = %+v, %v", out, err)
	}
	api.mu.Lock()
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
	for _, want := range []string{"path=/v1/login ", "path=/v1/login/totp ", "path=/v1/logout ", "exit=0", "exit=10"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the logs lack %q:\n%s", want, logs)
		}
	}
}

// Request bodies are at most 64 KiB on the login endpoints too, and an
// oversized or malformed one never reaches the service.
func TestLoginBodyIsBounded(t *testing.T) {
	t.Parallel()
	api := &loginAPI{}
	s := serve(t, &ctl.Server{API: api, Daemon: daemon()})
	big := `{"username":"alice","password":"` + strings.Repeat("x", 64<<10) + `"}`
	for _, target := range []string{"/v1/login", "/v1/login/totp"} {
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
	api := &loginAPI{entered: make(chan struct{}), release: make(chan struct{})}
	s := serve(t, &ctl.Server{API: api, Daemon: daemon()})
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
		"SubmitTOTP": func() error { _, err := c.SubmitTOTP(ctx, "123456"); return err },
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
	if _, err := c.SubmitTOTP(t.Context(), "123456"); err != nil {
		t.Errorf("SubmitTOTP after the login: %v", err)
	}
}

// Re-review N1: the client checks the UID of the process that accepted its
// connection (SO_PEERCRED on Linux) before it sends anything: a socket
// swapped between the lstat and the dial never gets a login.
func TestClientRefusesAServerOfAnotherUID(t *testing.T) {
	t.Parallel()
	api := &loginAPI{}
	s := serve(t, &ctl.Server{API: api, Daemon: daemon()})
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
	if _, err := dial(t, s.path).Logout(t.Context()); err != nil {
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
	srv := &ctl.Server{API: &loginAPI{}, Daemon: daemon()}
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
