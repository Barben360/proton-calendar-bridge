package daemon

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// startTestServer serves h on 127.0.0.1:0; entered gets a value when a
// request reaches h; shutdown is closed when Shutdown starts.
func startTestServer(t *testing.T, h http.HandlerFunc) (srv *http.Server, url string, shutdown chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv = &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	shutdown = make(chan struct{})
	srv.RegisterOnShutdown(func() { close(shutdown) })
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + ln.Addr().String() + "/", shutdown
}

func get(url string, res chan<- error) {
	resp, err := http.Get(url) //nolint:gosec,noctx // a test request to the test's own server
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}
	res <- err
}

// A request in flight when pcb serve stops is drained: it completes.
func TestStopListenerDrainsARequestInFlight(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	srv, url, shutdown := startTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	res := make(chan error, 1)
	go get(url, res)
	<-entered
	stopped := make(chan struct{})
	go func() {
		stopListener(srv, slog.New(slog.DiscardHandler), drainTimeout)
		close(stopped)
	}()
	select {
	case <-shutdown: // the drain started while the request runs
	case <-time.After(10 * time.Second):
		t.Fatal("stopListener did not shut the server down")
	}
	close(release)
	if err := <-res; err != nil {
		t.Errorf("the request in flight failed: %v", err)
	}
	<-stopped
}

// A request still running at the end of the drain is cut, and stopListener
// returns: pcb serve never hangs on a stuck client.
func TestStopListenerCutsWhatOutlivesTheDrain(t *testing.T) {
	t.Parallel()
	entered, never := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(never) })
	srv, url, _ := startTestServer(t, func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-never:
		case <-r.Context().Done():
		}
	})
	res := make(chan error, 1)
	go get(url, res)
	<-entered
	stopListener(srv, slog.New(slog.DiscardHandler), 50*time.Millisecond)
	select {
	case err := <-res:
		if err == nil {
			t.Error("the stuck request completed; want it cut")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stuck request still runs after stopListener returned")
	}
}

// Go's server default is TLS 1.2 today; the listener does not rely on it.
func TestListenerTLSNeedsTLS12(t *testing.T) {
	t.Parallel()
	conf, fp, err := listenerTLS(slog.New(slog.DiscardHandler), io.Discard, net.IPv4zero, filepath.Join(t.TempDir(), "tls"), config.CalDAV{Listen: "0.0.0.0:8443"})
	if err != nil {
		t.Fatal(err)
	}
	if conf == nil || conf.MinVersion != tls.VersionTLS12 || len(conf.Certificates) != 1 || fp == "" {
		t.Fatalf("TLS config = %+v, fingerprint %q; want TLS 1.2 minimum and the certificate", conf, fp)
	}
}

func TestURLHostIsLocalhostOnlyForTheUsualLoopbacks(t *testing.T) {
	t.Parallel()
	for ip, want := range map[string]string{"127.0.0.1": "localhost", "::1": "localhost", "127.0.0.2": "127.0.0.2", "192.168.1.5": "192.168.1.5"} {
		if got, err := urlHost(config.CalDAV{}, net.ParseIP(ip), net.InterfaceAddrs); got != want || err != nil {
			t.Errorf("urlHost(%s) = %q, %v; want %q", ip, got, err, want)
		}
	}
	if got, _ := urlHost(config.CalDAV{Hostnames: []string{"pcb.lan"}}, net.ParseIP("127.0.0.1"), net.InterfaceAddrs); got != "pcb.lan" {
		t.Errorf("urlHost with a hostname = %q", got)
	}
}

// An unspecified bind names the first interface address; when the
// interfaces cannot be listed (a unit without AF_NETLINK), the error comes
// back with the localhost fallback, so serve can log it (final review I1).
func TestURLHostReportsAnInterfaceListingError(t *testing.T) {
	t.Parallel()
	lan := func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.IPv4(127, 0, 0, 1)}, &net.IPNet{IP: net.IPv4(192, 168, 1, 7)}}, nil
	}
	if got, err := urlHost(config.CalDAV{}, net.IPv4zero, lan); got != "192.168.1.7" || err != nil {
		t.Errorf("urlHost(0.0.0.0) = %q, %v; want 192.168.1.7", got, err)
	}
	denied := func() ([]net.Addr, error) { return nil, errors.New("address family not supported by protocol") }
	got, err := urlHost(config.CalDAV{}, net.IPv4zero, denied)
	if got != "localhost" || err == nil || !strings.Contains(err.Error(), "address family not supported") {
		t.Errorf("urlHost(0.0.0.0) with no interface list = %q, %v; want localhost and the cause", got, err)
	}
}

// A certificate replaced while another was stored (here: a new hostname)
// rotates the fingerprint Thunderbird pinned: Warn, old and new fingerprint
// (audit surface-M4). A first generation and a plain reuse stay quiet.
func TestListenerTLSWarnsWhenAStoredCertificateIsReplaced(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	dir := filepath.Join(t.TempDir(), "tls")
	cc := config.CalDAV{Listen: "0.0.0.0:8443", Hostnames: []string{"a.lan"}}
	_, first, err := listenerTLS(log, io.Discard, net.IPv4zero, dir, cc)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenerTLS(log, io.Discard, net.IPv4zero, dir, cc); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("a first generation and a reuse warned:\n%s", buf.String())
	}
	cc.Hostnames = []string{"b.lan"}
	_, second, err := listenerTLS(log, io.Discard, net.IPv4zero, dir, cc)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("test bug: the certificate was not replaced")
	}
	got := buf.String()
	for _, want := range []string{"level=WARN", `msg="the TLS certificate changed: Thunderbird will ask to confirm the new fingerprint"`,
		"old_fingerprint=" + first, "new_fingerprint=" + second} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
}

// chanHandler sends each record to ch.
type chanHandler struct{ ch chan slog.Record }

func (h chanHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h chanHandler) Handle(_ context.Context, r slog.Record) error {
	h.ch <- r
	return nil
}
func (h chanHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h chanHandler) WithGroup(string) slog.Handler      { return h }

// A failed TLS handshake (a port scan, a client that distrusts the
// certificate) is logged at Debug, not Warn (audit surface-M7); other
// errors of the server stay Warn.
func TestServerErrorLogPutsFailedTLSHandshakesAtDebug(t *testing.T) {
	t.Parallel()
	records := make(chan slog.Record, 16)
	log := slog.New(chanHandler{records})
	conf, _, err := listenerTLS(slog.New(slog.DiscardHandler), io.Discard, net.IPv4zero, filepath.Join(t.TempDir(), "tls"), config.CalDAV{Listen: "0.0.0.0:8443"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{TLSConfig: conf, ErrorLog: serverErrorLog(log), Handler: http.NotFoundHandler()}
	t.Cleanup(func() { _ = srv.Close() })
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	r := <-records
	if !strings.Contains(r.Message, "TLS handshake error") || r.Level != slog.LevelDebug {
		t.Errorf("handshake failure logged as %v %q, want DEBUG", r.Level, r.Message)
	}
	serverErrorLog(log).Print("http: Accept error: boom")
	if r := <-records; r.Level != slog.LevelWarn {
		t.Errorf("another server error logged at %v, want WARN", r.Level)
	}
}

// 0.0.0.0 binds IPv4 only (audit surface-M1); [::], an empty host and a
// name keep the dual stack.
func TestListenNetworkIsTCP4OnlyForIPv4Literals(t *testing.T) {
	t.Parallel()
	for listen, want := range map[string]string{
		"0.0.0.0:8443": "tcp4", "127.0.0.1:1080": "tcp4", "192.168.1.5:8443": "tcp4",
		"[::]:8443": "tcp", ":8443": "tcp", "[::1]:8443": "tcp", "pcb.lan:8443": "tcp", "[::ffff:10.0.0.1]:1": "tcp",
	} {
		if got := listenNetwork(listen); got != want {
			t.Errorf("listenNetwork(%q) = %q, want %q", listen, got, want)
		}
	}
}

// The base has no trailing slash: AccountURL adds /dav/<id>/ to it.
func TestThunderbirdBaseHasNoTrailingSlash(t *testing.T) {
	t.Parallel()
	addr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1080}
	if got, err := thunderbirdBase(config.CalDAV{}, addr.IP, addr, false); got != "http://localhost:1080" || err != nil {
		t.Errorf("plain base = %q, %v", got, err)
	}
	addr = &net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 8443}
	if got, _ := thunderbirdBase(config.CalDAV{}, addr.IP, addr, true); got != "https://192.168.1.5:8443" {
		t.Errorf("TLS base = %q", got)
	}
}

func TestOfflineInfoNamesTheBaseAndNothingWhenListenIsMalformed(t *testing.T) {
	t.Parallel()
	dirs := paths.Dirs{State: t.TempDir()}
	got := OfflineInfo(t.Context(), dirs, config.CalDAV{Listen: "127.0.0.1:1080"})
	if got.Base != "http://localhost:1080" || got.TLS || got.Fingerprint != "" {
		t.Errorf("OfflineInfo = %+v, want the plain loopback base", got)
	}
	if got := OfflineInfo(t.Context(), dirs, config.CalDAV{Listen: "not a listen address"}); got.Base != "" {
		t.Errorf("OfflineInfo of a malformed listen = %+v, want no base", got)
	}
	if _, err := os.Stat(filepath.Join(dirs.State, "tls")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("OfflineInfo created the tls directory: %v", err)
	}
}

// A session that ended with no Proton answer to read (no refresh available)
// logs without a status or code: none is invented.
func TestLogPassInventsNoProtonAnswer(t *testing.T) {
	var b bytes.Buffer
	log := slog.New(slog.NewTextHandler(&b, nil))
	LogPass(log, service.WatchPass{Err: &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("the Proton session has ended"), Hint: "run pcb login"}})
	got := b.String()
	if !strings.Contains(got, `msg="sync pass failed"`) || !strings.Contains(got, "run pcb login") {
		t.Fatalf("the failed pass is not logged:\n%s", got)
	}
	if strings.Contains(got, "proton_status") || strings.Contains(got, "proton_code") {
		t.Errorf("a status or code was invented:\n%s", got)
	}
}
