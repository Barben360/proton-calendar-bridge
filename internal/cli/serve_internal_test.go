package cli

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
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
		if got := urlHost(config.CalDAV{}, net.ParseIP(ip)); got != want {
			t.Errorf("urlHost(%s) = %q, want %q", ip, got, want)
		}
	}
	if got := urlHost(config.CalDAV{Hostnames: []string{"pcb.lan"}}, net.ParseIP("127.0.0.1")); got != "pcb.lan" {
		t.Errorf("urlHost with a hostname = %q", got)
	}
}
