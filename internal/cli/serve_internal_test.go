package cli

import (
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
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
