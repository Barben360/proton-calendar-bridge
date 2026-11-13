package mcp_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/access"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The remote /mcp (design §5.2) over a loopback test listener: TokenAuth in
// front of the streamable HTTP handler, as the daemon mounts it.

type remote struct {
	srv    *mcp.Server
	store  *access.TokenStore
	url    string // .../mcp
	host   string // the listener's host:port
	tokens map[string]string
	skew   atomic.Int64 // added to the limiter's clock, in ns
}

// newRemote serves /mcp for the token store of a fresh state directory,
// with pcb.example as the configured hostname. mod changes the test
// server before it starts (timeouts).
func newRemote(t *testing.T, hub mcp.Hub, mod func(*http.Server)) *remote {
	t.Helper()
	r := &remote{store: access.NewTokenStore(t.TempDir(), func() time.Time { return fixedNow }), tokens: map[string]string{}}
	lim := access.NewLimiter(func() time.Time { return time.Now().Add(time.Duration(r.skew.Load())) })
	r.srv = mcp.NewServer(mcp.Options{Hub: hub, Now: func() time.Time { return fixedNow }, Zone: paris})
	ts := httptest.NewUnstartedServer(r.srv.HTTPHandler(mcp.TokenAuth(r.store, lim, []string{"pcb.example"})))
	if mod != nil {
		mod(ts.Config)
	}
	ts.Start()
	t.Cleanup(ts.Close)
	r.url = ts.URL + "/mcp"
	r.host = strings.TrimPrefix(ts.URL, "http://")
	return r
}

// add stores a token and keeps its secret under its name.
func (r *remote) add(t *testing.T, tok access.Token) string {
	t.Helper()
	secret, err := r.store.Add(t.Context(), tok)
	if err != nil {
		t.Fatal(err)
	}
	r.tokens[tok.Name] = secret
	return secret
}

const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`

// post sends one JSON-RPC message; hdr sets headers ("Host" sets the
// request's host, an empty value removes the header).
func (r *remote) post(t *testing.T, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, r.url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range hdr {
		switch {
		case k == "Host":
			req.Host = v
		case v == "":
			req.Header.Del(k)
		default:
			req.Header.Set(k, v)
		}
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

func bearer(secret string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + secret}
}

// initStatus is the status of an initialize sent with hdr.
func (r *remote) initStatus(t *testing.T, hdr map[string]string) int {
	t.Helper()
	res := r.post(t, initialize, hdr)
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// client is an SDK client session over /mcp with the token secret.
func (r *remote) client(t *testing.T, secret string) *sdkmcp.ClientSession {
	t.Helper()
	hc := &http.Client{Transport: headerRT{key: "Authorization", value: "Bearer " + secret}}
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: r.url, HTTPClient: hc, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// headerRT adds a header to every request.
type headerRT struct{ key, value string }

func (h headerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(h.key, h.value)
	return http.DefaultTransport.RoundTrip(req)
}

func TestRemoteMCPNeedsAToken(t *testing.T) {
	t.Parallel()
	r := newRemote(t, newHub(), nil)
	good := r.add(t, access.Token{Name: "agent", Caps: []string{"read"}})
	past := fixedNow.Add(-time.Hour)
	expired := r.add(t, access.Token{Name: "old", Caps: []string{"read"}, Expires: &past})
	other := strings.Replace(good, good[len(good)-4:], "AAAA", 1)
	if other == good {
		other = strings.Replace(good, good[len(good)-4:], "BBBB", 1)
	}

	for name, hdr := range map[string]map[string]string{
		"no header":         nil,
		"basic":             {"Authorization": "Basic bWU6cGFzcw=="},
		"empty bearer":      {"Authorization": "Bearer "},
		"unknown token":     bearer(other),
		"no prefix":         bearer(strings.TrimPrefix(good, access.TokenPrefix)),
		"a CalDAV password": bearer("correct-horse-battery-staple"),
		"expired":           bearer(expired),
	} {
		res := r.post(t, initialize, hdr)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: %d, WWW-Authenticate %q, want 401 asking for a bearer token", name, res.StatusCode, res.Header.Get("WWW-Authenticate"))
		}
		if res.Header.Get("Mcp-Session-Id") != "" {
			t.Errorf("%s: a session was opened", name)
		}
		if strings.Contains(string(body), good) || strings.Contains(string(body), expired) {
			t.Errorf("%s: the answer quotes a token: %q", name, body)
		}
		r.skew.Add(int64(2 * access.Window)) // each failure in a window of its own: no 503 here
	}

	res := r.post(t, initialize, bearer(good))
	if res.StatusCode != http.StatusOK || res.Header.Get("Mcp-Session-Id") == "" {
		t.Fatalf("a valid token: %d, session %q, want 200 and a session", res.StatusCode, res.Header.Get("Mcp-Session-Id"))
	}
	// The use is recorded (pcb mcp token list's last used).
	list, err := r.store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range list {
		if used := !tok.LastUsed.IsZero(); used != (tok.Name == "agent") {
			t.Errorf("token %s: last used %v", tok.Name, tok.LastUsed)
		}
	}

	// A stored grant the tools cannot serve (delete without write) is
	// refused, not widened nor narrowed.
	bad := r.add(t, access.Token{Name: "deleter", Caps: []string{"delete"}})
	if got := r.initStatus(t, bearer(bad)); got != http.StatusForbidden {
		t.Errorf("a token with delete alone: %d, want 403", got)
	}
}

func TestRemoteMCPLimiter503AfterFiveFailures(t *testing.T) {
	t.Parallel()
	r := newRemote(t, newHub(), nil)
	good := r.add(t, access.Token{Name: "agent", Caps: []string{"read"}})
	for i := range access.MaxFailures {
		if got := r.initStatus(t, bearer("pcbm_wrong")); got != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d, want 401", i+1, got)
		}
	}
	// Past five failures, even the right token waits: the limiter answers
	// before any token is checked.
	res := r.post(t, initialize, bearer(good))
	retry, err := strconv.Atoi(res.Header.Get("Retry-After"))
	if res.StatusCode != http.StatusServiceUnavailable || err != nil || retry < 1 || retry > 60 {
		t.Fatalf("after five failures: %d, Retry-After %q, want 503 and 1..60 s", res.StatusCode, res.Header.Get("Retry-After"))
	}
	if res.Header.Get("Mcp-Session-Id") != "" {
		t.Error("a limited request opened a session")
	}
}

func TestRemoteMCPRefusesForeignHost(t *testing.T) {
	t.Parallel()
	r := newRemote(t, newHub(), nil)
	good := r.add(t, access.Token{Name: "agent", Caps: []string{"read"}})
	_, port, _ := strings.Cut(r.host, ":")
	with := func(host string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + good, "Host": host}
	}
	for _, host := range []string{"evil.example", "evil.example:" + port, "pcb.example.evil.example", "localhost.evil.example", "192.168.1.5:" + port} {
		if got := r.initStatus(t, with(host)); got != http.StatusForbidden {
			t.Errorf("Host %s: %d, want 403", host, got)
		}
	}
	// Refusals for the host are not password guesses: the limiter is untouched.
	for _, host := range []string{r.host, "localhost:" + port, "LOCALHOST:" + port, "[::1]:" + port, "127.0.0.1", "pcb.example:8443", "PCB.example"} {
		if got := r.initStatus(t, with(host)); got != http.StatusOK {
			t.Errorf("Host %s: %d, want 200", host, got)
		}
	}
}

func TestRemoteMCPRefusesForeignOrigin(t *testing.T) {
	t.Parallel()
	r := newRemote(t, newHub(), nil)
	good := r.add(t, access.Token{Name: "agent", Caps: []string{"read"}})
	with := func(host, origin string) map[string]string {
		h := map[string]string{"Authorization": "Bearer " + good, "Origin": origin}
		if host != "" {
			h["Host"] = host
		}
		return h
	}
	for _, c := range []struct{ host, origin string }{
		{"", "http://evil.example"},
		{"", "null"},
		{"", "http://" + r.host + ".evil.example"},
		{"pcb.example:8443", "https://evil.example:8443"},
		{"pcb.example:8443", "https://localhost:8443"},
	} {
		if got := r.initStatus(t, with(c.host, c.origin)); got != http.StatusForbidden {
			t.Errorf("Host %q, Origin %s: %d, want 403", c.host, c.origin, got)
		}
	}
	for _, c := range []struct{ host, origin string }{
		{"", "http://" + r.host},
		{"pcb.example:8443", "https://pcb.example:8443"},
	} {
		if got := r.initStatus(t, with(c.host, c.origin)); got != http.StatusOK {
			t.Errorf("Host %q, Origin %s: %d, want 200", c.host, c.origin, got)
		}
	}
}

// A token's session is its own: another valid token presenting its
// session ID is refused (E19: else a stolen ID works under any token).
func TestRemoteMCPSessionBoundToItsToken(t *testing.T) {
	t.Parallel()
	r := newRemote(t, newHub(), nil)
	a := r.add(t, access.Token{Name: "a", Caps: []string{"read"}})
	b := r.add(t, access.Token{Name: "b", Caps: []string{"read"}})
	res := r.post(t, initialize, bearer(a))
	id := res.Header.Get("Mcp-Session-Id")
	if res.StatusCode != http.StatusOK || id == "" {
		t.Fatalf("initialize: %d, session %q", res.StatusCode, id)
	}
	const list = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	hdr := func(secret string) map[string]string {
		return map[string]string{"Authorization": "Bearer " + secret, "Mcp-Session-Id": id, "Mcp-Protocol-Version": "2025-06-18"}
	}
	if got := r.post(t, list, hdr(b)); got.StatusCode != http.StatusForbidden {
		t.Errorf("token b on a's session: %d, want 403", got.StatusCode)
	}
	got := r.post(t, list, hdr(a))
	body, _ := io.ReadAll(got.Body)
	if got.StatusCode != http.StatusOK || !strings.Contains(string(body), "events_list") {
		t.Errorf("token a on its session: %d %s, want 200 listing its tools", got.StatusCode, body)
	}
}

// A remote session nobody uses is forgotten after an hour (design §5.2,
// ruling R17): its ID then answers 404, and the client starts a new one.
// The hour is shortened here; not parallel, since it sets the idle time.
func TestRemoteMCPSessionEndsWhenIdle(t *testing.T) {
	if got := mcp.SessionIdle(); got != time.Hour {
		t.Fatalf("a remote session idles out after %s, want 1h (SPEC §12)", got)
	}
	defer mcp.SetSessionIdle(300 * time.Millisecond)()
	r := newRemote(t, newHub(), nil)
	a := r.add(t, access.Token{Name: "a", Caps: []string{"read"}})
	res := r.post(t, initialize, bearer(a))
	id := res.Header.Get("Mcp-Session-Id")
	if res.StatusCode != http.StatusOK || id == "" {
		t.Fatalf("initialize: %d, session %q", res.StatusCode, id)
	}
	hdr := map[string]string{"Authorization": "Bearer " + a, "Mcp-Session-Id": id, "Mcp-Protocol-Version": "2025-06-18"}
	const list = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	if got := r.post(t, list, hdr); got.StatusCode != http.StatusOK {
		t.Fatalf("the session right after initialize: %d, want 200", got.StatusCode)
	}
	// Each request restarts the idle clock, so wait well past it between
	// tries (a loaded machine may fire the timer late).
	for try := 0; try < 5; try++ {
		time.Sleep(time.Second)
		got := r.post(t, list, hdr)
		_, _ = io.Copy(io.Discard, got.Body)
		if got.StatusCode == http.StatusNotFound {
			return
		}
		if got.StatusCode != http.StatusOK {
			t.Fatalf("an idle session: %d, want 404", got.StatusCode)
		}
	}
	t.Fatal("the session still answers after 1 s idle (five tries), with a 300 ms idle time: want 404")
}

// Over HTTP a read token gets the read tools and never login nor logout;
// the live sessions are counted per entry until they close.
func TestRemoteMCPServesReadTools(t *testing.T) {
	t.Parallel()
	r := newRemote(t, newHub(), nil)
	cs := r.client(t, r.add(t, access.Token{Name: "agent", Caps: []string{"read"}}))
	want := []string{"calendars_list", "event_get", "events_list", "events_search", "free_busy", "status"}
	if got, _ := toolNames(t, cs); !slices.Equal(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	if out := mustCall(t, cs, "status", nil); len(accountsOf(out)) != 2 {
		t.Errorf("status accounts = %v, want both", accountsOf(out))
	}
	if got := r.srv.LiveSessions(); got[mcp.EntryRemote] != 1 || got[mcp.EntryRelay] != 0 || got[mcp.EntryStdio] != 0 {
		t.Errorf("live sessions = %v, want one remote", got)
	}
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for r.srv.LiveSessions()[mcp.EntryRemote] != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("live sessions after close = %v, want none", r.srv.LiveSessions())
		}
		time.Sleep(10 * time.Millisecond)
	}
	// A token for one account sees that one only.
	one := r.client(t, r.add(t, access.Token{Name: "bob-only", Caps: []string{"freebusy"}, Accounts: []string{"bob@example.com"}}))
	if out := mustCall(t, one, "status", nil); !slices.Equal(accountsOf(out), []string{"bob@example.com"}) {
		t.Errorf("status accounts = %v, want bob only", accountsOf(out))
	}
}

// slowHub answers Health after a delay: a tool call that outlives the
// listener's timeouts.
type slowHub struct {
	*stubHub
	delay time.Duration
}

func (h slowHub) Health(ctx context.Context, sel string) (service.Health, error) {
	select {
	case <-time.After(h.delay):
	case <-ctx.Done():
		return service.Health{}, ctx.Err()
	}
	return h.stubHub.Health(ctx, sel)
}

// The listener's read and write timeouts bound CalDAV requests; /mcp
// lifts them once a request is authorised, so a long tool call (or an SSE
// stream) delivers its answer.
func TestRemoteMCPStreamOutlivesWriteTimeout(t *testing.T) {
	t.Parallel()
	const timeout = 300 * time.Millisecond
	r := newRemote(t, slowHub{stubHub: newHub(), delay: 4 * timeout}, func(s *http.Server) {
		s.ReadTimeout, s.WriteTimeout = timeout, timeout
	})
	cs := r.client(t, r.add(t, access.Token{Name: "agent", Caps: []string{"read"}}))
	start := time.Now()
	if out := mustCall(t, cs, "status", nil); len(accountsOf(out)) != 2 {
		t.Errorf("status accounts = %v", accountsOf(out))
	}
	if d := time.Since(start); d < 2*timeout {
		t.Fatalf("the call took %v: it did not outlive the timeouts, the test proves nothing", d)
	}

	// The standalone SSE stream (GET) stays open past both timeouts.
	secret := r.add(t, access.Token{Name: "listener", Caps: []string{"read"}})
	res := r.post(t, initialize, bearer(secret))
	id := res.Header.Get("Mcp-Session-Id")
	if res.StatusCode != http.StatusOK || id == "" {
		t.Fatalf("initialize: %d, session %q", res.StatusCode, id)
	}
	hdr := map[string]string{"Authorization": "Bearer " + secret, "Mcp-Session-Id": id, "Mcp-Protocol-Version": "2025-06-18"}
	if got := r.post(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, hdr); got.StatusCode != http.StatusAccepted {
		t.Fatalf("initialized: %d", got.StatusCode)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, r.url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "text/event-stream")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("GET: %d", stream.StatusCode)
	}
	ended := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, stream.Body); ended <- err }()
	select {
	case err := <-ended:
		t.Fatalf("the SSE stream ended after less than %v: %v", 4*timeout, err)
	case <-time.After(4 * timeout):
	}
}

// The token stops at TokenAuth: what the SDK hands the tools (their
// request's headers) carries no secret.
func TestRemoteMCPKeepsTheTokenFromTools(t *testing.T) {
	t.Parallel()
	r := newRemote(t, newHub(), nil)
	secret := r.add(t, access.Token{Name: "agent", Caps: []string{"read"}})
	caps, err := mcp.ParseCaps("read")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	// The server the handler serves this token's sessions with (Session
	// keeps one per grant).
	r.srv.Session(mcp.Grant{Name: "agent", Caps: caps}, mcp.EntryRemote).AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
			if x := req.GetExtra(); x != nil && x.Header != nil {
				mu.Lock()
				seen = append(seen, method+": "+strings.Join(x.Header.Values("Authorization"), ","))
				mu.Unlock()
			}
			return next(ctx, method, req)
		}
	})
	cs := r.client(t, secret)
	mustCall(t, cs, "status", nil)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no request reached the server with its headers: the check would pass vacuously")
	}
	for _, s := range seen {
		if method, auth, _ := strings.Cut(s, ": "); strings.Contains(auth, secret) || strings.Contains(auth, access.TokenPrefix) {
			t.Errorf("%s: the request the server sees carries the token", method) // not printed
		}
	}
}

// An Auth that admits a request without any Authorization header (the
// control socket's, Task 12) is served.
func TestHTTPHandlerServesAnAuthWithoutBearer(t *testing.T) {
	t.Parallel()
	srv := mcp.NewServer(mcp.Options{Hub: newHub(), Now: func() time.Time { return fixedNow }, Zone: paris})
	ts := httptest.NewServer(srv.HTTPHandler(func(*http.Request) (mcp.Grant, mcp.Entry, string, error) {
		return readGrant, mcp.EntryRelay, "relay", nil
	}))
	t.Cleanup(ts.Close)
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "relay", Version: "0"}, nil).
		Connect(t.Context(), &sdkmcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", MaxRetries: -1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	if out := mustCall(t, cs, "status", nil); len(accountsOf(out)) != 2 {
		t.Errorf("status accounts = %v", accountsOf(out))
	}
	if got := srv.LiveSessions(); got[mcp.EntryRelay] != 1 || got[mcp.EntryRemote] != 0 {
		t.Errorf("live sessions = %v, want one relay", got)
	}
}
