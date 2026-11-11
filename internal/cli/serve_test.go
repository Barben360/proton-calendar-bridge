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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
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
	addr      chan net.Addr
	code      chan exitcode.Code
	stop      context.CancelFunc
	out, errb *syncBuffer
}

func startServe(t *testing.T, r *rig, wrap func(net.Listener) net.Listener, args ...string) *serveRun {
	t.Helper()
	s := &serveRun{t: t, requested: make(chan string, 1), addr: make(chan net.Addr, 1), code: make(chan exitcode.Code, 1),
		out: &syncBuffer{}, errb: &syncBuffer{}}
	io := cli.IO{Stdout: s.out, Stderr: s.errb, Term: r.term, Env: func(k string) string { return r.env[k] },
		Listen: func(network, address string) (net.Listener, error) {
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
}

func newServeStub() *serveStub {
	return &serveStub{stopWith: make(chan error, 1), served: make(chan struct{})}
}

func (s *serveStub) Serve(ctx context.Context, o service.ServeOptions) error {
	defer close(s.served)
	o.Ready()
	o.OnPass(service.WatchPass{Result: service.SyncResult{Calendars: []service.CalendarSync{{ID: "cal-1", Name: "Secret name", Mode: "incremental", Added: 1, Events: 2}}}, Next: 30 * time.Second})
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.stopWith:
		o.OnPass(service.WatchPass{Err: err})
		return err
	}
}

func (s *serveStub) Clients(context.Context) ([]service.Client, error) { return s.clients, nil }

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
	if !strings.Contains(s.out.String(), "pcb client add NAME") {
		t.Errorf("stdout lacks the client hint with no client: %q", s.out.String())
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
