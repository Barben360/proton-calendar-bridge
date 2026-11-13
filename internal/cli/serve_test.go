package cli_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
)

// syncBuffer is a bytes.Buffer pcb serve writes while the test runs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// serveRun is a `pcb serve` running in the background. Its listener always
// binds 127.0.0.1:0 (the address asked for is recorded); wrap, when set,
// changes what the listener reports as its address, so a non-loopback bind
// is decided without binding one. stop plays SIGINT/SIGTERM: Execute turns
// them into the cancellation of Main's context.
type serveRun struct {
	t         *testing.T
	requested chan string
	networks  chan string // the network of each Listen call
	addr      chan net.Addr
	code      chan exitcode.Code
	stop      context.CancelFunc
	out, errb *syncBuffer
}

func startServe(t *testing.T, r *rig, wrap func(net.Listener) net.Listener, args ...string) *serveRun {
	t.Helper()
	return startServeWith(t, r, wrap, nil, args...)
}

// startServeWith is startServe with sigs, when set, as the signals pcb
// serve subscribes to (IO.Signals).
func startServeWith(t *testing.T, r *rig, wrap func(net.Listener) net.Listener, sigs chan os.Signal, args ...string) *serveRun {
	t.Helper()
	// The state directory pcb serve binds its control socket in (a real
	// service makes it before it is ready).
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &serveRun{t: t, requested: make(chan string, 1), networks: make(chan string, 1), addr: make(chan net.Addr, 1), code: make(chan exitcode.Code, 1),
		out: &syncBuffer{}, errb: &syncBuffer{}}
	io := cli.IO{Stdout: s.out, Stderr: s.errb, Term: r.term, Env: func(k string) string { return r.env[k] },
		Listen: func(network, address string) (net.Listener, error) {
			s.networks <- network
			s.requested <- address
			ln, err := net.Listen(network, "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			if wrap != nil {
				ln = wrap(ln)
			}
			return &acceptSignal{Listener: ln, first: func() { s.addr <- ln.Addr() }}, nil
		}}
	switch {
	case r.svc != nil:
		io.NewService = func(context.Context, func(string) string) (service.API, error) { return r.svc, nil }
	case r.mk != nil:
		io.NewService = func(context.Context, func(string) string) (service.API, error) { return r.mk() }
	}
	if sigs != nil {
		io.Signals = func() (<-chan os.Signal, func()) { return sigs, func() {} }
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	s.stop = cancel
	go func() { s.code <- cli.Main(ctx, append([]string{"serve"}, args...), io) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.code:
		case <-time.After(30 * time.Second):
			t.Error("pcb serve did not stop within 30 s of the test's end")
		}
	})
	return s
}

// acceptSignal calls first when the server first accepts: it is serving
// then, its certificate made and its first lines printed.
type acceptSignal struct {
	net.Listener
	once  sync.Once
	first func()
}

func (a *acceptSignal) Accept() (net.Conn, error) {
	a.once.Do(a.first)
	return a.Listener.Accept()
}

// listening waits until the listener serves and returns the address the test dials
// (127.0.0.1 and the real port).
func (s *serveRun) listening() string {
	s.t.Helper()
	select {
	case a := <-s.addr:
		_, port, _ := net.SplitHostPort(a.String())
		return net.JoinHostPort("127.0.0.1", port)
	case c := <-s.code:
		s.code <- c
		s.t.Fatalf("pcb serve exited %d before listening\nstdout: %s\nstderr: %s", c, s.out.String(), s.errb.String())
	}
	return ""
}

// exit waits for pcb serve to end and returns its exit code.
func (s *serveRun) exit() exitcode.Code {
	s.t.Helper()
	select {
	case c := <-s.code:
		s.code <- c // the cleanup reads it again
		return c
	case <-time.After(30 * time.Second):
		s.t.Fatalf("pcb serve still runs\nstdout: %s\nstderr: %s", s.out.String(), s.errb.String())
	}
	return 0
}

// neverListened fails the test if pcb serve asked for its listener.
func (s *serveRun) neverListened() {
	s.t.Helper()
	select {
	case a := <-s.requested:
		s.t.Errorf("pcb serve bound %s although it refused to start", a)
	default:
	}
}

// serveStub is a service whose Serve is ready at once and runs until its
// context ends or stopWith gets an error to stop on.
type serveStub struct {
	stubAPI
	clients  []service.Client
	stopWith chan error
	served   chan struct{} // closed when Serve returns
	// stopOnCancel is what Serve returns when its context ends (nil: a
	// normal stop): an error the loop stops on while pcb serve stops.
	stopOnCancel error
	// passes, when set, are the passes Serve reports after its first one.
	passes []service.WatchPass
	// hang, when set, makes Serve outlive its context until it is closed:
	// a sync loop that does not stop. hung is closed once it hangs.
	hang, hung chan struct{}

	// blockCheck makes CheckClient (the first call of every authenticated
	// request) wait, after closing checkEntered, until checkRelease is
	// closed or Serve's context ends, which it records in cancelledInFlight.
	blockCheck        bool
	checkEntered      chan struct{}
	checkRelease      chan struct{}
	cancelledInFlight atomic.Bool
	mu                sync.Mutex
	serveCtx          context.Context
}

func newServeStub() *serveStub {
	return &serveStub{stopWith: make(chan error, 1), served: make(chan struct{}),
		checkEntered: make(chan struct{}), checkRelease: make(chan struct{})}
}

func (s *serveStub) Serve(ctx context.Context, o service.ServeOptions) error {
	defer close(s.served)
	s.mu.Lock()
	s.serveCtx = ctx
	s.mu.Unlock()
	o.Ready()
	o.OnPass(service.WatchPass{Result: service.SyncResult{Calendars: []service.CalendarSync{{ID: "cal-1", Name: "Secret name", Mode: "incremental", Added: 1, Events: 2}}}, Next: 30 * time.Second})
	for _, p := range s.passes {
		o.OnPass(p)
	}
	select {
	case <-ctx.Done():
		if s.hang != nil {
			close(s.hung)
			<-s.hang
		}
		return s.stopOnCancel
	case err := <-s.stopWith:
		o.OnPass(service.WatchPass{Err: err})
		return err
	}
}

func (s *serveStub) Clients(context.Context) ([]service.Client, error) { return s.clients, nil }

func (s *serveStub) CheckClient(context.Context, string, string) (bool, time.Duration) {
	if !s.blockCheck {
		return false, 0
	}
	close(s.checkEntered)
	s.mu.Lock()
	ctx := s.serveCtx
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		s.cancelledInFlight.Store(true)
	case <-s.checkRelease:
	}
	return false, 0
}

func options(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// refusesConnections fails the test if addr still accepts a connection.
func refusesConnections(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err == nil {
		_ = c.Close()
		t.Errorf("%s still accepts connections after pcb serve exited", addr)
	}
}

func TestServeRefusesWhileSyncRuns(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.useLocal()
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := filelock.TryLock(filepath.Join(r.dirs.State, "sync.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	s := startServe(t, r, nil)
	if code := s.exit(); code != exitcode.Blocked {
		t.Fatalf("exit %d, want 20\nstderr: %s", code, s.errb.String())
	}
	if !strings.Contains(s.errb.String(), "a sync is already running") {
		t.Errorf("stderr = %q", s.errb.String())
	}
	s.neverListened()
}

func TestServeRefusesWhenLoggedOut(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.useLocal()
	s := startServe(t, r, nil)
	if code := s.exit(); code != exitcode.MustAct {
		t.Fatalf("exit %d, want 10\nstderr: %s", code, s.errb.String())
	}
	if !strings.Contains(s.errb.String(), "Hint: run pcb login") {
		t.Errorf("stderr = %q, want the pcb login hint", s.errb.String())
	}
	s.neverListened()
}

// Logged in with no client (all removed): a login would not run again, so
// the hint is client add (final review I2).
func TestServeIntroHintsClientAddWhenLoggedIn(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	stub.statusPhase = "ready"
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	s.listening()
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, s.errb.String())
	}
	if want := "No CalDAV client: run pcb client add NAME, then use its password in Thunderbird."; !strings.Contains(s.out.String(), want) {
		t.Errorf("stdout lacks %q: %q", want, s.out.String())
	}
	if strings.Contains(s.out.String(), "pcb login") {
		t.Errorf("a logged-in daemon tells to log in: %q", s.out.String())
	}
}

func TestServeRefusesJSONAndABadListen(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--json"}, {"--listen", "1080"}, {"--listen", ":1080"}, {"--listen", "127.0.0.1:99999"}} {
		stub := newServeStub()
		r := stubRig(t, &stub.stubAPI)
		r.svc = stub
		s := startServe(t, r, nil, args...)
		if code := s.exit(); code != exitcode.Usage {
			t.Errorf("serve %v: exit %d, want 2", args, code)
		}
		s.neverListened()
		select {
		case <-stub.served:
			t.Errorf("serve %v: the service ran before the usage error", args)
		default:
		}
	}
}

func TestServeStopsOnSignal(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	s := startServe(t, r, nil, "--listen", "127.0.0.1:1999")
	addr := s.listening()
	if got := <-s.requested; got != "127.0.0.1:1999" {
		t.Errorf("listened on %q, want the --listen address over the config's", got)
	}
	if got := <-s.networks; got != "tcp4" {
		t.Errorf("listened on network %q, want tcp4 for an IPv4 literal (no dual stack)", got)
	}
	resp := options(t, http.DefaultClient, "http://"+addr+"/")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("DAV"), "calendar-access") {
		t.Fatalf("OPTIONS = %d, DAV %q", resp.StatusCode, resp.Header.Get("DAV"))
	}

	s.stop() // SIGINT
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d after the signal, want 0\nstderr: %s", code, s.errb.String())
	}
	select {
	case <-stub.served:
	default:
		t.Error("pcb serve exited while the sync loop still ran")
	}
	refusesConnections(t, addr)

	_, port, _ := net.SplitHostPort(addr)
	lines := strings.Split(s.out.String(), "\n")
	if want := "Thunderbird URL: http://localhost:" + port + "/"; lines[0] != want {
		t.Errorf("first line %q, want %q", lines[0], want)
	}
	// Not logged in (decision 0013): the login creates the client default.
	if want := "No CalDAV client yet: run pcb login (it creates the client default), then pcb client show for its password."; !strings.Contains(s.out.String(), want) {
		t.Errorf("stdout lacks %q with no client and no login: %q", want, s.out.String())
	}
	if strings.Contains(s.out.String(), "fingerprint") {
		t.Errorf("plain HTTP printed a fingerprint: %q", s.out.String())
	}
	logs := s.errb.String()
	for _, want := range []string{"listener started", "sync pass", "calendars=1", "listener stopped"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "Secret name") {
		t.Errorf("logs carry a calendar name (IDs only):\n%s", logs)
	}
}

// Ruling M3-R34: pcb serve logs the render counters of each pass while one
// is non-zero, and once more when they change (back to zero); IDs only.
func TestServeLogsTheRenderCounters(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	zero := &service.RenderCounts{}
	bad := &service.RenderCounts{Undecryptable: 2, Unverified: 3, DuplicateMasters: 1, KeysErrors: []string{"cal-locked", "cal-other"}}
	stub.passes = []service.WatchPass{
		{Render: zero}, // nothing to say: no line
		{Render: &service.RenderCounts{Unverified: 1}}, // line 1
		{Render: zero},                    // back to zero, a change: line 2
		{Render: bad},                     // line 3
		{Render: bad},                     // unchanged but non-zero: line 4
		{Render: &service.RenderCounts{}}, // back to zero: line 5
		{Render: zero},                    // zero again: no line
		{},                                // no counters (WatchSync's passes): no line
		{Render: &service.RenderCounts{DuplicateMasters: 4}}, // line 6
		{Render: &service.RenderCounts{DroppedAlarms: 2}},    // line 7
	}
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	s := startServe(t, r, nil)
	s.listening()
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
	var lines []string
	for l := range strings.SplitSeq(s.errb.String(), "\n") {
		if strings.Contains(l, "render counters") {
			lines = append(lines, l)
		}
	}
	want := [][]string{
		{"level=WARN", "undecryptable=0", "unverified=1", "duplicate_masters=0"},
		{"level=INFO", "undecryptable=0", "unverified=0", "duplicate_masters=0"},
		{"level=WARN", "undecryptable=2", "unverified=3", "duplicate_masters=1", `keys_error_ids="[cal-locked cal-other]"`},
		{"level=WARN", "undecryptable=2", "unverified=3", "duplicate_masters=1", "keys_error_ids="},
		{"level=INFO", "undecryptable=0", "unverified=0", "duplicate_masters=0"},
		{"level=WARN", "undecryptable=0", "unverified=0", "duplicate_masters=4", "dropped_alarms=0"},
		{"level=WARN", "undecryptable=0", "unverified=0", "duplicate_masters=0", "dropped_alarms=2"},
	}
	if len(lines) != len(want) {
		t.Fatalf("%d render counter lines, want %d:\n%s", len(lines), len(want), strings.Join(lines, "\n"))
	}
	for i, ws := range want {
		for _, w := range ws {
			if !strings.Contains(lines[i], w) {
				t.Errorf("line %d lacks %q: %s", i+1, w, lines[i])
			}
		}
		if len(ws) < 5 && strings.Contains(lines[i], "keys_error_ids") {
			t.Errorf("line %d names keys errors although none: %s", i+1, lines[i])
		}
	}
}

func TestServeExitsWithTheErrorTheSyncStopsOn(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	stub.clients = []service.Client{{ID: "c1", Name: "Thunderbird"}}
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	s := startServe(t, r, nil)
	addr := s.listening()
	if got := <-s.requested; got != config.DefaultListen {
		t.Errorf("listened on %q, want the default %s", got, config.DefaultListen)
	}
	stub.stopWith <- &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("Proton ended the session"), Hint: "run pcb login"}
	if code := s.exit(); code != exitcode.MustAct {
		t.Fatalf("exit %d, want 10 (the stop error's)\nstderr: %s", code, s.errb.String())
	}
	refusesConnections(t, addr)
	if !strings.Contains(s.errb.String(), "Hint: run pcb login") {
		t.Errorf("stderr = %q", s.errb.String())
	}
	if strings.Contains(s.out.String(), "pcb client add") {
		t.Errorf("the client hint was printed with a client registered: %q", s.out.String())
	}
}

// caPool trusts the certificate of a PEM file.
func caPool(t *testing.T, certPath string) *x509.CertPool {
	t.Helper()
	leaf, err := tlsutil.ReadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return pool
}

// fakeAddr reports a listener's port at another IP.
type fakeAddr struct {
	net.Listener
	ip net.IP
}

func (f fakeAddr) Addr() net.Addr {
	a := f.Listener.Addr().(*net.TCPAddr)
	return &net.TCPAddr{IP: f.ip, Port: a.Port}
}

func TestNonLoopbackBindNeverServesPlainHTTP(t *testing.T) {
	t.Parallel()
	for _, bind := range []string{"0.0.0.0", "::"} {
		t.Run(bind, func(t *testing.T) {
			t.Parallel()
			stub := newServeStub()
			r := stubRig(t, &stub.stubAPI)
			r.svc = stub
			wrap := func(ln net.Listener) net.Listener { return fakeAddr{ln, net.ParseIP(bind)} }
			fingerprints := make([]string, 0, 2)
			for start := range 2 {
				r.svc = newServeStub()
				s := startServe(t, r, wrap, "--listen", net.JoinHostPort(bind, "8443"))
				addr := s.listening()
				certPath := tlsutil.CertPath(filepath.Join(r.dirs.State, "tls"))
				tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
					RootCAs: caPool(t, certPath), ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}}}
				if resp := options(t, tlsClient, "https://"+addr+"/"); resp.StatusCode != http.StatusOK || resp.TLS == nil {
					t.Fatalf("OPTIONS over TLS = %d", resp.StatusCode)
				}
				// Plain HTTP never gets a CalDAV answer.
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodOptions, "http://"+addr+"/", nil)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK || resp.Header.Get("DAV") != "" {
						t.Errorf("plain HTTP answered %d with DAV %q", resp.StatusCode, resp.Header.Get("DAV"))
					}
				}
				// TLS below 1.2 is refused.
				old := &tls.Config{RootCAs: caPool(t, certPath), ServerName: "127.0.0.1", MaxVersion: tls.VersionTLS11} //nolint:gosec // the test checks it is refused
				if c, err := tls.Dial("tcp", addr, old); err == nil {
					_ = c.Close()
					t.Error("a TLS 1.1 handshake succeeded")
				}
				s.stop()
				if code := s.exit(); code != exitcode.OK {
					t.Fatalf("exit %d", code)
				}
				leaf, err := tlsutil.ReadCertificate(certPath)
				if err != nil {
					t.Fatal(err)
				}
				fp := tlsutil.Fingerprint(leaf.Raw)
				fingerprints = append(fingerprints, fp)
				out := s.out.String()
				if !strings.HasPrefix(out, "Thunderbird URL: https://") || !strings.Contains(out, ":"+portOf(addr)+"/\n") {
					t.Errorf("start %d: stdout = %q, want an https URL with the port first", start, out)
				}
				if lines := strings.Split(out, "\n"); len(lines) < 2 || !strings.Contains(lines[1], fp) {
					t.Errorf("start %d: the second line does not carry the fingerprint %s: %q", start, fp, out)
				}
				want := map[int]string{0: "tls certificate generated", 1: "tls certificate reused"}[start]
				if !strings.Contains(s.errb.String(), want) {
					t.Errorf("start %d: logs lack %q:\n%s", start, want, s.errb.String())
				}
			}
			if fingerprints[0] != fingerprints[1] {
				t.Errorf("the second start made a new certificate: %v", fingerprints)
			}
		})
	}
	t.Run("loopback", func(t *testing.T) {
		t.Parallel()
		stub := newServeStub()
		r := stubRig(t, &stub.stubAPI)
		r.svc = stub
		s := startServe(t, r, nil)
		addr := s.listening()
		if resp := options(t, http.DefaultClient, "http://"+addr+"/"); resp.StatusCode != http.StatusOK {
			t.Fatalf("plain OPTIONS on loopback = %d", resp.StatusCode)
		}
		if c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true}); err == nil { //nolint:gosec // only whether TLS answers
			_ = c.Close()
			t.Error("loopback answered a TLS handshake")
		}
		if _, err := os.Stat(filepath.Join(r.dirs.State, "tls")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("loopback created %s/tls (err %v)", r.dirs.State, err)
		}
	})
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

func TestUserCertIsUsed(t *testing.T) {
	t.Parallel()
	certDir := t.TempDir()
	user, err := tlsutil.SelfSigned(tlsutil.Options{Dir: certDir, Hostnames: []string{"pcb.example"}, BindIP: net.ParseIP("192.0.2.1"),
		InterfaceAddrs: func() ([]net.Addr, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	stub := newServeStub()
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	toml := "[proton]\nbase_url = \"http://127.0.0.1:1\"\n[caldav]\nlisten = \"0.0.0.0:8443\"\nhostnames = [\"pcb.example\"]\n" +
		"tls_cert = \"" + tlsutil.CertPath(certDir) + "\"\ntls_key = \"" + tlsutil.KeyPath(certDir) + "\"\n"
	if err := os.WriteFile(config.Path(r.dirs), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	s := startServe(t, r, func(ln net.Listener) net.Listener { return fakeAddr{ln, net.IPv4zero} })
	addr := s.listening()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: caPool(t, tlsutil.CertPath(certDir)), ServerName: "pcb.example", MinVersion: tls.VersionTLS12}}}
	resp := options(t, client, "https://"+addr+"/")
	if resp.StatusCode != http.StatusOK || resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		t.Fatalf("OPTIONS over TLS = %d", resp.StatusCode)
	}
	if got := tlsutil.Fingerprint(resp.TLS.PeerCertificates[0].Raw); got != user.Fingerprint {
		t.Errorf("served certificate %s, want the user's %s", got, user.Fingerprint)
	}
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
	if !strings.HasPrefix(s.out.String(), "Thunderbird URL: https://pcb.example:"+portOf(addr)+"/\n") || !strings.Contains(s.out.String(), user.Fingerprint) {
		t.Errorf("stdout = %q, want the first hostname's URL and the user's fingerprint", s.out.String())
	}
	if _, err := os.Stat(filepath.Join(r.dirs.State, "tls")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a user certificate still generated one in %s/tls (err %v)", r.dirs.State, err)
	}
}

// End to end on the real listener: pcb serve against the fake Proton, one
// client, an authenticated PROPFIND of the home set.
func TestServeEndToEndPropfindsTheHomeSet(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	work := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	r.f.AddEvent(work, protontest.Event{UID: "a@x", Start: baseTime, End: baseTime + 3600})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	r.want(exitcode.OK, "sync", "--json")
	sec := addClient(t, r, "Thunderbird")
	r.mk = func() (service.API, error) { // passes wait for the next interval: no busy loop
		cfg, err := config.Load(config.Path(r.dirs), r.dirs)
		if err != nil {
			return nil, err
		}
		return service.NewLocal(service.Options{Dirs: r.dirs, Config: cfg, Now: func() time.Time { return t0 },
			Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }})
	}
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	addr := s.listening()
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/><D:displayname/></D:prop></D:propfind>`
	req, err := http.NewRequestWithContext(t.Context(), "PROPFIND", "http://"+addr+"/dav/calendars/me/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	req.SetBasicAuth("me@example.org", sec.Password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus || !strings.Contains(string(got), "<D:displayname>Work</D:displayname>") {
		t.Fatalf("PROPFIND home set = %d\n%s", resp.StatusCode, got)
	}
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
	if strings.Contains(s.out.String()+s.errb.String(), sec.Password) {
		t.Error("the client password reached pcb serve's output")
	}
}

// failingListener accepts nothing: the server's Serve returns its error.
type failingListener struct{ net.Listener }

func (failingListener) Accept() (net.Conn, error) {
	return nil, errors.New("accept: the listener broke")
}

func TestServeListenerFailureIsExit20(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	s := startServe(t, r, func(ln net.Listener) net.Listener { return failingListener{ln} })
	if code := s.exit(); code != exitcode.Blocked {
		t.Fatalf("exit %d, want 20\nstderr: %s", code, s.errb.String())
	}
	if !strings.Contains(s.errb.String(), "Error: the CalDAV listener failed") {
		t.Errorf("stderr = %q", s.errb.String())
	}
	select {
	case <-stub.served:
	default:
		t.Error("pcb serve exited with the sync loop still running")
	}
}

// The listener fails while the sync loop stops on an error of its own: the
// loop's error (a person is needed) wins over the listener's.
func TestServeSyncStopErrorWinsOverAListenerFailure(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	stub.stopOnCancel = &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("Proton ended the session"), Hint: "run pcb login"}
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	s := startServe(t, r, func(ln net.Listener) net.Listener { return failingListener{ln} })
	if code := s.exit(); code != exitcode.MustAct {
		t.Fatalf("exit %d, want 10 (the sync loop's)\nstderr: %s", code, s.errb.String())
	}
	if !strings.Contains(s.errb.String(), "Hint: run pcb login") {
		t.Errorf("stderr = %q", s.errb.String())
	}
}

// closeSignal closes closed when the server closes its listener: the first
// step of Shutdown, before it waits for the requests in flight.
type closeSignal struct {
	net.Listener
	once   sync.Once
	closed chan struct{}
}

func (c *closeSignal) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Listener.Close()
}

// On a signal the listener drains first: a request in flight finishes with
// the sync loop (and its cache handle) still running; the loop is cancelled
// after.
func TestServeDrainsBeforeCancellingTheSyncLoop(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	stub.blockCheck = true
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	closed := make(chan struct{})
	s := startServe(t, r, func(ln net.Listener) net.Listener { return &closeSignal{Listener: ln, closed: closed} })
	addr := s.listening()
	status := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), "PROPFIND", "http://"+addr+"/dav/calendars/me/", nil)
		req.SetBasicAuth("u", "p")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- 0
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	<-stub.checkEntered
	s.stop() // SIGTERM with the request in flight
	<-closed // the drain has begun
	close(stub.checkRelease)
	if got := <-status; got != http.StatusUnauthorized {
		t.Errorf("the request in flight got %d, want its own answer (401)", got)
	}
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d", code)
	}
	if stub.cancelledInFlight.Load() {
		t.Error("the sync loop was cancelled while a request was still in flight")
	}
}

func TestServeWarnsAboutCertificatesToCheck(t *testing.T) {
	t.Parallel()
	userCert := func(t *testing.T, issued time.Time) string {
		dir := t.TempDir()
		if _, err := tlsutil.SelfSigned(tlsutil.Options{Dir: dir, Hostnames: []string{"pcb.example"}, BindIP: net.ParseIP("192.0.2.1"),
			Now: func() time.Time { return issued }, InterfaceAddrs: func() ([]net.Addr, error) { return nil, nil }}); err != nil {
			t.Fatal(err)
		}
		return "[caldav]\ntls_cert = \"" + tlsutil.CertPath(dir) + "\"\ntls_key = \"" + tlsutil.KeyPath(dir) + "\"\n"
	}
	run := func(t *testing.T, r *rig, wrap func(net.Listener) net.Listener) string {
		r.svc = newServeStub()
		s := startServe(t, r, wrap)
		s.listening()
		s.stop()
		if code := s.exit(); code != exitcode.OK {
			t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
		}
		return s.errb.String()
	}
	withConfig := func(t *testing.T, extra string) *rig {
		r := stubRig(t, &newServeStub().stubAPI)
		if err := os.WriteFile(config.Path(r.dirs), []byte("[proton]\nbase_url = \"http://127.0.0.1:1\"\n"+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		return r
	}
	validity := tlsutil.Validity
	for name, tc := range map[string]struct {
		issued time.Time
		want   string
	}{
		"valid":           {time.Now(), ""},
		"under 30 days":   {time.Now().Add(-validity + 10*24*time.Hour), "Warning: the certificate of caldav.tls_cert expires on "},
		"already expired": {time.Now().Add(-validity - 24*time.Hour), "Warning: the certificate of caldav.tls_cert expired on "},
	} {
		t.Run("user certificate "+name, func(t *testing.T) {
			t.Parallel()
			errs := run(t, withConfig(t, userCert(t, tc.issued)), nil)
			if tc.want == "" && strings.Contains(errs, "Warning:") || tc.want != "" && !strings.Contains(errs, tc.want) {
				t.Errorf("stderr = %q, want %q", errs, tc.want)
			}
		})
	}
	t.Run("unusable self-signed pair", func(t *testing.T) {
		t.Parallel()
		r := withConfig(t, "")
		unspecified := func(ln net.Listener) net.Listener { return fakeAddr{ln, net.IPv4zero} }
		if errs := run(t, r, unspecified); strings.Contains(errs, "Warning:") {
			t.Fatalf("first generation warned: %q", errs)
		}
		dir := filepath.Join(r.dirs.State, "tls")
		if err := os.WriteFile(tlsutil.KeyPath(dir), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		if errs := run(t, r, unspecified); !strings.Contains(errs, "Warning: the stored self-signed certificate was unusable") {
			t.Errorf("stderr = %q", errs)
		}
	})
}

// Ruling M5-Rpre8: [log] level and --log-level pick what pcb serve logs; the
// flag wins. "listener started" is an info line.
func TestServeHonoursTheLogLevel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, config string
		args         []string
		wantInfo     bool
	}{
		{"default is info", "", nil, true},
		{"flag warn", "", []string{"--log-level", "warn"}, false},
		{"config warn", "[log]\nlevel = \"warn\"\n", nil, false},
		{"flag warn over config debug", "[log]\nlevel = \"debug\"\n", []string{"--log-level", "warn"}, false},
		{"flag info over config warn", "[log]\nlevel = \"warn\"\n", []string{"--log-level", "info"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := newServeStub()
			r := stubRig(t, &stub.stubAPI)
			r.svc = stub
			if tc.config != "" {
				f, err := os.OpenFile(config.Path(r.dirs), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString("\n" + tc.config); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			}
			s := startServe(t, r, nil, tc.args...)
			s.listening()
			s.stop()
			if code := s.exit(); code != exitcode.OK {
				t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
			}
			logs := s.errb.String()
			if got := strings.Contains(logs, "listener started"); got != tc.wantInfo {
				t.Errorf("\"listener started\" logged = %v, want %v:\n%s", got, tc.wantInfo, logs)
			}
		})
	}
}

func TestServeRefusesABadLogLevel(t *testing.T) {
	t.Parallel()
	for _, level := range []string{"loud", "INFO", ""} {
		stub := newServeStub()
		r := stubRig(t, &stub.stubAPI)
		r.svc = stub
		s := startServe(t, r, nil, "--log-level", level)
		if code := s.exit(); code != exitcode.Usage {
			t.Errorf("--log-level %q: exit %d, want 2", level, code)
		}
		s.neverListened()
		select {
		case <-stub.served:
			t.Errorf("--log-level %q: the service ran before the usage error", level)
		default:
		}
	}
}

// pcb serve answers on its control socket while it serves, publishes its
// CalDAV listener there, and removes the socket when it stops.
func TestServeServesTheControlSocket(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	stub := newServeStub()
	stub.statusPhase = "ready"
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	s := startServe(t, r, nil, "--log-level", "debug")
	addr := s.listening()

	c, err := ctl.Dial(t.Context(), r.dirs.Socket())
	if err != nil {
		t.Fatalf("Dial while serving: %v", err)
	}
	st, err := c.Status(t.Context())
	_ = c.Close()
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(addr)
	if st.Phase != "ready" || st.Daemon == nil || st.Daemon.PID != os.Getpid() || st.Daemon.LogLevel != "debug" ||
		st.Daemon.Socket != r.dirs.Socket() || st.Daemon.Version == "" {
		t.Errorf("status = %+v, daemon %+v", st, st.Daemon)
	}
	if st.Listener == nil || st.Listener.URL != "http://localhost:"+port+"/" || st.Listener.TLS || !strings.HasSuffix(st.Listener.Addr, ":"+port) {
		t.Errorf("listener = %+v, want the CalDAV one on port %s", st.Listener, port)
	}
	if fi, err := os.Lstat(r.dirs.Socket()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket: %v, %v; want mode 0600", fi, err)
	}

	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
	if _, err := os.Lstat(r.dirs.Socket()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the socket is still there after the stop: %v", err)
	}
	if _, err := ctl.Dial(t.Context(), r.dirs.Socket()); !errors.Is(err, ctl.ErrNoDaemon) {
		t.Errorf("Dial after the stop: %v, want ErrNoDaemon", err)
	}
}

// The socket's directory must grant group and others nothing: pcb serve
// exits 20 before it binds CalDAV.
func TestServeRefusesAnOpenStateDir(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	stub := newServeStub()
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.dirs.State, 0o750); err != nil {
		t.Fatal(err)
	}
	s := startServe(t, r, nil)
	if code := s.exit(); code != exitcode.Blocked {
		t.Fatalf("exit %d, want 20\nstderr: %s", code, s.errb.String())
	}
	if want := "Hint: chmod 700 " + r.dirs.State; !strings.Contains(s.errb.String(), want) {
		t.Errorf("stderr = %q, want %q", s.errb.String(), want)
	}
	s.neverListened()
}

// A state directory too long for a socket path: exit 20 with the
// constraint's hint, before CalDAV binds.
func TestServeRefusesATooLongSocketPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	stub := newServeStub()
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	base := filepath.Dir(r.dirs.State)
	long := filepath.Join(base, strings.Repeat("s", 120-len(base)))
	r.env["XDG_STATE_HOME"], r.dirs.State = long, filepath.Join(long, "pcb")
	s := startServe(t, r, nil)
	if code := s.exit(); code != exitcode.Blocked {
		t.Fatalf("exit %d, want 20\nstderr: %s", code, s.errb.String())
	}
	if want := "Hint: set PCB_STATE_DIR to a shorter path"; !strings.Contains(s.errb.String(), want) {
		t.Errorf("stderr = %q, want %q", s.errb.String(), want)
	}
	s.neverListened()
}

// The socket is removed on every exit after Ready, not only on a signal:
// the sync loop stopping on an error, and the CalDAV listener failing.
func TestServeRemovesTheSocketWhenItFails(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	t.Run("sync error", func(t *testing.T) {
		t.Parallel()
		stub := newServeStub()
		r := stubRig(t, &stub.stubAPI)
		r.svc = stub
		s := startServe(t, r, nil)
		s.listening()
		if _, err := os.Lstat(r.dirs.Socket()); err != nil {
			t.Fatalf("no socket while serving: %v", err)
		}
		stub.stopWith <- &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("Proton ended the session"), Hint: "run pcb login"}
		if code := s.exit(); code != exitcode.MustAct {
			t.Fatalf("exit %d, want 10\nstderr: %s", code, s.errb.String())
		}
		if _, err := os.Lstat(r.dirs.Socket()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the socket outlives a sync error: %v", err)
		}
	})
	t.Run("listener failure", func(t *testing.T) {
		t.Parallel()
		stub := newServeStub()
		r := stubRig(t, &stub.stubAPI)
		r.svc = stub
		s := startServe(t, r, func(ln net.Listener) net.Listener { return failingListener{ln} })
		if code := s.exit(); code != exitcode.Blocked {
			t.Fatalf("exit %d, want 20\nstderr: %s", code, s.errb.String())
		}
		if !strings.Contains(s.errb.String(), "control socket started") {
			t.Fatalf("the control socket never started:\n%s", s.errb.String())
		}
		if _, err := os.Lstat(r.dirs.Socket()); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the socket outlives a listener failure: %v", err)
		}
	})
}

// fastPoll is the real service with a short login poll (poll, 0: 20 ms),
// reporting Serve's states and passes to channels (the CLI's own callbacks
// still run).
type fastPoll struct {
	*service.Local
	states chan service.ServeState
	passes chan service.WatchPass
	poll   time.Duration
	run    *serveRun // set by serveWaiting
}

func (f *fastPoll) Serve(ctx context.Context, o service.ServeOptions) error {
	o.LoginPoll = 20 * time.Millisecond
	if f.poll > 0 {
		o.LoginPoll = f.poll
	}
	onState, onPass := o.OnState, o.OnPass
	o.OnState = func(s service.ServeState) {
		if onState != nil {
			onState(s)
		}
		f.states <- s
	}
	o.OnPass = func(p service.WatchPass) {
		if onPass != nil {
			onPass(p)
		}
		select {
		case f.passes <- p:
		default:
		}
	}
	return f.Local.Serve(ctx, o)
}

func wantServeState(t *testing.T, states chan service.ServeState, want service.ServeState) {
	t.Helper()
	select {
	case s := <-states:
		if s != want {
			t.Fatalf("Serve reported %q, want %q", s, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Serve never reported %q", want)
	}
}

// propfindHome sends Thunderbird's home-set PROPFIND to addr, with the
// client's password when pw is set.
func propfindHome(t *testing.T, addr, pw string) (*http.Response, string) {
	t.Helper()
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/><D:displayname/></D:prop></D:propfind>`
	req, err := http.NewRequestWithContext(t.Context(), "PROPFIND", "http://"+addr+"/dav/calendars/me/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	if pw != "" {
		req.SetBasicAuth("me@example.org", pw)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, string(got)
}

// Review Focus 2 (rulings M5-P8, M5-P9): Thunderbird polling a pcb serve
// --wait-for-login that has no login gets 503 + Retry-After: 30 (never a
// 4xx), 401 without credentials; CTL says it waits; after a pcb login by
// another process, the same request gets 207 from the same pcb serve.
func TestServeWaitForLoginAnswers503ThenServes(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	work := r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	r.f.AddEvent(work, protontest.Event{UID: "a@x", Start: baseTime, End: baseTime + 3600})
	sec := addClient(t, r, "Thunderbird")
	fp := &fastPoll{states: make(chan service.ServeState, 16), passes: make(chan service.WatchPass, 16)}
	r.mk = func() (service.API, error) { // passes wait for the next interval: no busy loop
		cfg, err := config.Load(config.Path(r.dirs), r.dirs)
		if err != nil {
			return nil, err
		}
		l, err := service.NewLocal(service.Options{Dirs: r.dirs, Config: cfg, Now: func() time.Time { return t0 },
			Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }})
		fp.Local = l
		return fp, err
	}
	s := startServe(t, r, nil, "--wait-for-login", "--listen", "127.0.0.1:0")
	addr := s.listening()
	r.useLocal() // pcb login below runs as another process would
	wantServeState(t, fp.states, service.ServeWaitingForLogin)

	resp, _ := propfindHome(t, addr, sec.Password)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("PROPFIND while waiting: %d, Retry-After %q; want 503, 30", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if resp, _ := propfindHome(t, addr, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PROPFIND without credentials while waiting: %d, want 401", resp.StatusCode)
	}
	if runtime.GOOS != "windows" {
		c, err := ctl.Dial(t.Context(), r.dirs.Socket())
		if err != nil {
			t.Fatalf("Dial while waiting: %v", err)
		}
		st, err := c.Status(t.Context())
		_ = c.Close()
		if err != nil {
			t.Fatal(err)
		}
		needsLogin := false
		for _, a := range st.Alerts {
			needsLogin = needsLogin || a.Code == "needs-login"
		}
		if st.Health == nil || st.Health.Serve != service.ServeWaitingForLogin || !needsLogin {
			t.Errorf("CTL status while waiting: health %+v, alerts %+v; want waiting-for-login and needs-login", st.Health, st.Alerts)
		}
	}
	out := s.out.String()
	if lines := strings.Split(out, "\n"); !strings.HasPrefix(lines[0], "Thunderbird URL: http://localhost:") ||
		!strings.Contains(out, "\nWaiting for a login: run pcb login\n") {
		t.Errorf("stdout = %q, want the intro then the waiting line", out)
	}
	if !strings.Contains(s.errb.String(), "waiting for a login") {
		t.Errorf("the wait is not logged:\n%s", s.errb.String())
	}

	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	wantServeState(t, fp.states, service.ServeServing)
	select {
	case p := <-fp.passes:
		if p.Err != nil {
			t.Fatalf("first pass after the login: %v", p.Err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no pass after the login")
	}
	resp, got := propfindHome(t, addr, sec.Password)
	if resp.StatusCode != http.StatusMultiStatus || !strings.Contains(got, "<D:displayname>Work</D:displayname>") {
		t.Fatalf("PROPFIND after the login = %d\n%s", resp.StatusCode, got)
	}
	select {
	case c := <-s.code:
		t.Fatalf("pcb serve exited %d around the login", c)
	default:
	}
	if n := strings.Count(s.out.String(), "Thunderbird URL:"); n != 1 {
		t.Errorf("the intro printed %d times, want once (no restart)", n)
	}
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
}

// Ruling M5-P22: a second SIGINT or SIGTERM while pcb serve stops (here the
// sync loop never ends) exits 20 at once.
func TestSecondSignalForcesExit(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	stub.hang, stub.hung = make(chan struct{}), make(chan struct{})
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	closed := make(chan struct{})
	sigs := make(chan os.Signal, 2)
	s := startServeWith(t, r, func(ln net.Listener) net.Listener { return &closeSignal{Listener: ln, closed: closed} }, sigs)
	t.Cleanup(func() { close(stub.hang) })
	s.listening()
	sigs <- syscall.SIGTERM // the stop begins: the listener drains, then the loop is cancelled and waited for
	<-closed
	<-stub.hung
	select {
	case c := <-s.code:
		t.Fatalf("pcb serve exited %d after one signal, before its sync loop ended", c)
	default:
	}
	sigs <- syscall.SIGINT
	if code := s.exit(); code != exitcode.Blocked {
		t.Fatalf("exit %d after the second signal, want 20\nstderr: %s", code, s.errb.String())
	}
	if want := "Hint: the sync loop was cut; run pcb serve again"; !strings.Contains(s.errb.String(), want) {
		t.Errorf("stderr = %q, want %q", s.errb.String(), want)
	}
	select {
	case <-stub.served:
		t.Error("the sync loop ended: the test did not reach the forced exit")
	default:
	}
}

// Ruling M5-P22, one real signal: it reaches both Execute's NotifyContext
// (the end of the context) and IO.Signals, and is still one signal: the
// listener drains (the request in flight gets its answer) and pcb serve
// exits 0, never 20.
func TestOneSignalOnBothPathsStopsCleanly(t *testing.T) {
	t.Parallel()
	stub := newServeStub()
	stub.blockCheck = true
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	closed := make(chan struct{})
	sigs := make(chan os.Signal) // unbuffered: a send returns once pcb serve took it
	s := startServeWith(t, r, func(ln net.Listener) net.Listener { return &closeSignal{Listener: ln, closed: closed} }, sigs)
	addr := s.listening()
	status := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), "PROPFIND", "http://"+addr+"/dav/calendars/me/", nil)
		req.SetBasicAuth("u", "p")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			status <- 0
			return
		}
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	<-stub.checkEntered
	s.stop()                // SIGTERM through NotifyContext
	<-closed                // the drain has begun
	sigs <- syscall.SIGTERM // the same SIGTERM through IO.Signals
	close(stub.checkRelease)
	if got := <-status; got != http.StatusUnauthorized {
		t.Errorf("the request in flight got %d, want its own answer (401)", got)
	}
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d after one signal, want 0\nstderr: %s", code, s.errb.String())
	}
	if logs := s.errb.String(); !strings.Contains(logs, "listener stopped") || strings.Contains(logs, "cut") {
		t.Errorf("logs = %s; want the drain to end, nothing cut", logs)
	}
}

// slowLocal is the rig's service whose watch waits for its context between
// passes, so a logged-in pcb serve does not loop on the fake.
func slowLocal(r *rig) func() (service.API, error) {
	return func() (service.API, error) {
		cfg, err := config.Load(config.Path(r.dirs), r.dirs)
		if err != nil {
			return nil, err
		}
		return service.NewLocal(service.Options{Dirs: r.dirs, Config: cfg, Now: func() time.Time { return t0 },
			Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }})
	}
}

// ux-I1: while pcb serve holds the sync lock and answers on the control
// socket, pcb sync has nothing to do (exit 0, it says so) and a second
// pcb serve names the running one (exit 20, as any refused start).
func TestSyncAndASecondServeNameTheRunningDaemon(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	r := newRig(t, &protontest.Options{})
	r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	r.mk = slowLocal(r)
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	s.listening()
	r.useLocal()

	r.want(exitcode.OK, "sync")
	if got, want := r.out.String(), "pcb serve is running and already syncs: see pcb status\n"; got != want || r.errb.Len() != 0 {
		t.Errorf("pcb sync: stdout %q, stderr %q; want %q", got, r.errb.String(), want)
	}
	r.want(exitcode.OK, "sync", "--json")
	var data struct {
		Calendars   []service.CalendarSync `json:"calendars"`
		DaemonSyncs bool                   `json:"daemon_syncs"`
	}
	e := r.envelope()
	mustUnmarshal(t, e.Data, &data)
	if !e.OK || e.Error != nil || !data.DaemonSyncs || data.Calendars == nil || len(data.Calendars) != 0 {
		t.Errorf("pcb sync --json = %+v, data %s", e, e.Data)
	}
	r.want(exitcode.OK, "sync", "--watch")
	if got := r.out.String(); got != "pcb serve is running and already syncs: see pcb status\n" {
		t.Errorf("pcb sync --watch: stdout %q", got)
	}

	second := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	if code := second.exit(); code != exitcode.Blocked {
		t.Fatalf("second pcb serve: exit %d, want 20\nstderr: %s", code, second.errb.String())
	}
	if got, want := second.errb.String(), "Error: pcb serve is already running\nHint: run pcb status to see it\n"; got != want {
		t.Errorf("second pcb serve: stderr %q, want %q", got, want)
	}
	second.neverListened()
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("first pcb serve: exit %d\nstderr: %s", code, s.errb.String())
	}
}

// ux-I1: logged out, pcb sync says to log in, even while pcb serve
// --wait-for-login holds the sync lock: the login is checked first.
func TestSyncChecksTheLoginBeforeTheRunningDaemon(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.mk = slowLocal(r)
	s := startServe(t, r, nil, "--wait-for-login", "--listen", "127.0.0.1:0")
	s.listening()
	r.useLocal()
	calls := r.f.Calls("", "/")
	for _, args := range [][]string{{"sync"}, {"sync", "--watch"}} {
		r.want(exitcode.MustAct, args...)
		if got, want := r.errb.String(), "Error: not logged in to Proton\nHint: run pcb login\n"; got != want {
			t.Errorf("pcb %s: stderr %q, want %q", strings.Join(args, " "), got, want)
		}
	}
	if n := r.f.Calls("", "/"); n != calls {
		t.Errorf("a logged-out pcb sync made %d requests", n-calls)
	}
	s.stop()
	_ = s.exit()
}

// ux Minor: a port in use names the address once, with the system's
// reason, never Go's "listen tcp ...: bind:" chain.
func TestServePortInUseSaysWhyOnce(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows words the reason differently")
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Close() })
	stub := newServeStub()
	r := stubRig(t, &stub.stubAPI)
	r.svc = stub
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	addr := busy.Addr().String()
	r.want(exitcode.Blocked, "serve", "--listen", addr)
	want := "Error: cannot listen on " + addr + ": address already in use\n" +
		"Hint: stop what uses that address, or choose another with --listen or [caldav] listen\n"
	if got := r.errb.String(); !strings.HasSuffix(got, want) {
		t.Errorf("stderr = %q, want it to end with %q", got, want)
	}
}

// HUMAN-CHECKS 7.3.4 (live 2026-11-13): when Proton refuses the refresh and
// the session ends, the "sync pass failed" line carries the HTTP status and
// the Proton code of the refusal, so a live run can confirm the signal. The
// message and hint stay the generic ones; nothing else of Proton's answer
// (its text, the tokens) is logged.
func TestServeLogsWhatProtonAnsweredWhenTheSessionEnds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		end        func(*protontest.Fake)
		wantStatus string
		wantCode   string
		text       string // what the fake's answer says; it must never reach the logs
	}{
		{"revoked session", func(f *protontest.Fake) { f.RevokeSessions() }, "proton_status=422", "proton_code=10013", "Invalid refresh token"},
		{"refresh refused with another status and code", func(f *protontest.Fake) {
			f.ExpireAccessTokens()
			f.FailNext("POST", "/auth/v4/refresh", http.StatusUnauthorized, 4242, 0)
		}, "proton_status=401", "proton_code=4242", "injected failure"},
		{"401 after a successful refresh", func(f *protontest.Fake) {
			f.FailNext("GET", "/calendar/v1", http.StatusUnauthorized, 4343, 0)
			f.FailNext("GET", "/calendar/v1", http.StatusUnauthorized, 4343, 0)
		}, "proton_status=401", "proton_code=4343", "injected failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, &protontest.Options{})
			r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
			r.want(exitcode.OK, "login", "--non-interactive", "--json")
			fp := &fastPoll{states: make(chan service.ServeState, 16), passes: make(chan service.WatchPass, 16)}
			r.mk = func() (service.API, error) {
				cfg, err := config.Load(config.Path(r.dirs), r.dirs)
				if err != nil {
					return nil, err
				}
				l, err := service.NewLocal(service.Options{Dirs: r.dirs, Config: cfg, Now: func() time.Time { return t0 },
					Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }})
				fp.Local = l
				return fp, err
			}
			tc.end(r.f)
			s := startServe(t, r, nil, "--wait-for-login", "--listen", "127.0.0.1:0")
			select {
			case p := <-fp.passes:
				if p.Err == nil {
					t.Fatal("the pass after the session ended did not fail")
				}
			case <-time.After(30 * time.Second):
				t.Fatal("no pass")
			}
			var line string
			for _, l := range strings.Split(s.errb.String(), "\n") {
				if strings.Contains(l, `msg="sync pass failed"`) {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("no sync pass failed line:\n%s", s.errb.String())
			}
			for _, want := range []string{tc.wantStatus, tc.wantCode, "the Proton session has ended", "hint=\"run pcb login\""} {
				if !strings.Contains(line, want) {
					t.Errorf("log line lacks %q:\n%s", want, line)
				}
			}
			for _, secret := range []string{tc.text, "fakeacc", "fakeref"} {
				if strings.Contains(s.errb.String(), secret) {
					t.Errorf("logs carry %q:\n%s", secret, s.errb.String())
				}
			}
		})
	}
}
