package daemon_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

var t0 = time.Date(2026, 11, 11, 10, 0, 7, 0, time.UTC)

// env is a fake Proton, a state directory and a Hub on it.
type env struct {
	f    *protontest.Fake
	dirs paths.Dirs
	cfg  config.Config
	hub  *service.Hub
}

func newEnv(t *testing.T) *env {
	t.Helper()
	f := protontest.New(t, protontest.Options{Now: func() time.Time { return t0 }})
	// Not t.TempDir: the control socket's path must fit the 108 bytes of sun_path.
	root, err := os.MkdirTemp("", "pcbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	e := &env{f: f, dirs: paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "state")}}
	e.cfg = config.Defaults(e.dirs)
	e.cfg.Proton.BaseURL = f.URL
	e.cfg.CalDAV.Listen = "127.0.0.1:0"
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: e.dirs, Config: e.cfg,
		Now:   func() time.Time { return t0 },
		Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() }}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	e.hub = hub
	return e
}

// login adds the fake's account and one CalDAV client: its local ID and the
// client's password.
func (e *env) login(t *testing.T) (id, password string) {
	t.Helper()
	res, err := e.hub.Login(t.Context(), service.LoginRequest{Username: e.f.Username, Password: e.f.Password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	api, _, err := e.hub.Account(res.Account)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := api.AddClient(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	return res.Account, sec.Password
}

// running is a daemon.Run in the background.
type running struct {
	info   daemon.Info
	cancel context.CancelFunc
	fin    chan struct{} // closed once Run returned
	err    error         // Run's result, set before fin closes
}

func (e *env) start(t *testing.T, hub daemon.Hub, mod func(*daemon.Options)) *running {
	t.Helper()
	ready := make(chan daemon.Info, 1)
	o := daemon.Options{Dirs: e.dirs, Config: e.cfg, Hub: hub, Kind: "tester", LogLevel: "info", WaitForLogin: true,
		Ready: func(i daemon.Info) { ready <- i }}
	if mod != nil {
		mod(&o)
	}
	ctx, cancel := context.WithCancel(t.Context())
	r := &running{cancel: cancel, fin: make(chan struct{})}
	go func() { r.err = daemon.Run(ctx, o); close(r.fin) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.fin:
		case <-time.After(30 * time.Second):
			t.Error("daemon.Run did not return within 30 s of the test's end")
		}
	})
	select {
	case r.info = <-ready:
	case <-r.fin:
		t.Fatalf("Run returned before it was ready: %v", r.err)
	case <-time.After(30 * time.Second):
		t.Fatal("Run was not ready within 30 s")
	}
	return r
}

func (r *running) propfind(ctx context.Context, url, password string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "PROPFIND", url, strings.NewReader(
		`<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/></D:prop></D:propfind>`))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	req.SetBasicAuth("me@example.org", password)
	return http.DefaultClient.Do(req)
}

func TestRunServesCalDAVAndControl(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, password := e.login(t)
	r := e.start(t, e.hub, nil)

	if r.info.TLS || strings.HasSuffix(r.info.Base, "/") || !strings.HasPrefix(r.info.Base, "http://") {
		t.Errorf("Base = %q (TLS %v), want http://host:port with no trailing slash", r.info.Base, r.info.TLS)
	}
	url := r.info.AccountURL(id)
	if want := r.info.Base + "/dav/" + id + "/"; url != want {
		t.Fatalf("AccountURL = %q, want %q", url, want)
	}
	// The listener reaches the account: Base names the port it bound.
	if _, port, _ := net.SplitHostPort(r.info.Addr); !strings.HasSuffix(r.info.Base, ":"+port) {
		t.Errorf("Base %q does not name the bound port of %q", r.info.Base, r.info.Addr)
	}
	_, port, _ := net.SplitHostPort(r.info.Addr)
	resp, err := r.propfind(t.Context(), "http://127.0.0.1:"+port+service.AccountPath(id), password)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMultiStatus {
		t.Errorf("PROPFIND on the account URL = %d, want 207", resp.StatusCode)
	}
	resp, err = r.propfind(t.Context(), "http://127.0.0.1:"+port+"/dav/"+id+"/", "wrong")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("PROPFIND with a wrong password = %d, want 401", resp.StatusCode)
	}

	c, err := ctl.Dial(t.Context(), e.dirs.Socket())
	if err != nil {
		t.Fatalf("the control socket does not answer: %v", err)
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st.Daemon == nil || st.Daemon.Kind != "tester" {
		t.Errorf("daemon = %+v, want kind tester (Options.Kind)", st.Daemon)
	}
	if st.Listener == nil || st.Listener.URL != r.info.Base || st.Listener.Addr != r.info.Addr {
		t.Errorf("listener = %+v, want the base %q published", st.Listener, r.info.Base)
	}
	r.cancel()
	<-r.fin
	if err := r.err; err != nil {
		t.Errorf("Run = %v, want nil after the context ended", err)
	}
	if _, err := os.Stat(e.dirs.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control socket outlives Run: %v", err)
	}
	// Run closes nothing it did not open: the Hub still answers.
	if _, _, err := e.hub.Account(id); err != nil {
		t.Errorf("the Hub was closed by Run: %v", err)
	}
}

// slowHub holds every CalDAV request in Route until release closes.
type slowHub struct {
	*service.Hub
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *slowHub) Route(ctx context.Context, accountID, password, remote string) (service.API, string, time.Duration, error) {
	h.once.Do(func() { close(h.entered) })
	<-h.release
	return h.Hub.Route(ctx, accountID, password, remote)
}

func newSlowHub(e *env) *slowHub {
	return &slowHub{Hub: e.hub, entered: make(chan struct{}), release: make(chan struct{})}
}

func TestRunDrainsOnCancel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, password := e.login(t)
	hub := newSlowHub(e)
	r := e.start(t, hub, nil)
	_, port, _ := net.SplitHostPort(r.info.Addr)
	res := make(chan error, 1)
	go func() {
		resp, err := r.propfind(t.Context(), "http://127.0.0.1:"+port+"/dav/"+id+"/", password)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusMultiStatus {
				err = errors.New("status " + resp.Status)
			}
		}
		res <- err
	}()
	<-hub.entered
	start := time.Now()
	r.cancel()
	select {
	case <-r.fin:
		t.Fatalf("Run returned (%v) while a request was in flight", r.err)
	case <-time.After(300 * time.Millisecond):
	}
	close(hub.release)
	if err := <-res; err != nil {
		t.Errorf("the request in flight did not complete: %v", err)
	}
	select {
	case <-r.fin:
		if err := r.err; err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(5500 * time.Millisecond):
		t.Fatal("Run did not return within 5.5 s of the cancel")
	}
	if d := time.Since(start); d > 5500*time.Millisecond {
		t.Errorf("Run took %v", d)
	}
}

// A request that outlives the 5 s drain is cut: Run still returns.
func TestRunCutsWhatOutlivesTheDrain(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, password := e.login(t)
	hub := newSlowHub(e)
	t.Cleanup(func() { close(hub.release) })
	r := e.start(t, hub, nil)
	_, port, _ := net.SplitHostPort(r.info.Addr)
	go func() {
		if resp, err := r.propfind(t.Context(), "http://127.0.0.1:"+port+"/dav/"+id+"/", password); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-hub.entered
	start := time.Now()
	r.cancel()
	select {
	case <-r.fin:
		if err := r.err; err != nil {
			t.Errorf("Run = %v, want nil", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("a stuck request held Run past the drain")
	}
	if d := time.Since(start); d < 4*time.Second {
		t.Errorf("Run returned after %v: the drain did not wait for the request in flight", d)
	}
}

// Closing Cut during the drain ends Run at once with ErrCut (exit 20).
func TestRunReturnsErrCutWhenCutCloses(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	id, password := e.login(t)
	hub := newSlowHub(e)
	t.Cleanup(func() { close(hub.release) })
	cut := make(chan struct{})
	r := e.start(t, hub, func(o *daemon.Options) { o.Cut = cut })
	_, port, _ := net.SplitHostPort(r.info.Addr)
	go func() {
		if resp, err := r.propfind(t.Context(), "http://127.0.0.1:"+port+"/dav/"+id+"/", password); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-hub.entered
	r.cancel()
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	close(cut)
	select {
	case <-r.fin:
		if err := r.err; !errors.Is(err, daemon.ErrCut) || exitcode.From(err) != exitcode.Blocked {
			t.Errorf("Run = %v (exit %d), want ErrCut with exit 20", err, exitcode.From(err))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return at once after Cut closed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Run took %v after the cut", d)
	}
}

func TestRunReturnsServeError(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // no account
	ready := false
	err := daemon.Run(t.Context(), daemon.Options{Dirs: e.dirs, Config: e.cfg, Hub: e.hub, Kind: "tester",
		Ready: func(daemon.Info) { ready = true }})
	if exitcode.From(err) != exitcode.MustAct {
		t.Fatalf("Run = %v (exit %d), want exit 10: no account is logged in", err, exitcode.From(err))
	}
	if ready {
		t.Error("Ready ran although Hub.Serve refused to start")
	}
	if _, err := os.Stat(e.dirs.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused start bound the control socket: %v", err)
	}
}

func TestRunListenFailureIsExit20AndReleasesEverything(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login(t)
	ready := false
	err := daemon.Run(t.Context(), daemon.Options{Dirs: e.dirs, Config: e.cfg, Hub: e.hub, Kind: "tester", WaitForLogin: true,
		Listen: func(string, string) (net.Listener, error) { return nil, errors.New("address already in use") },
		Ready:  func(daemon.Info) { ready = true }})
	if exitcode.From(err) != exitcode.Blocked || !strings.Contains(err.Error(), "cannot listen on 127.0.0.1:0") {
		t.Fatalf("Run = %v (exit %d), want exit 20 naming the address", err, exitcode.From(err))
	}
	if ready {
		t.Error("Ready ran without a listener")
	}
	if _, err := os.Stat(e.dirs.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the control socket outlives a failed start: %v", err)
	}
	// Hub.Serve ended: its serve.lock is free for a new Run.
	r := e.start(t, e.hub, nil)
	r.cancel()
	<-r.fin
	if err := r.err; err != nil {
		t.Errorf("second Run = %v, want nil", err)
	}
}

func TestDaemonPassesShowUI(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var mu sync.Mutex
	var got []string
	r := e.start(t, e.hub, func(o *daemon.Options) {
		o.ShowUI = func(account string) error {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, account)
			return nil
		}
	})
	c, err := ctl.Dial(t.Context(), e.dirs.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.ShowUI(t.Context(), "acc1"); err != nil {
		t.Fatalf("ShowUI: %v", err)
	}
	mu.Lock()
	if len(got) != 1 || got[0] != "acc1" {
		t.Errorf("Options.ShowUI got %q, want [acc1]", got)
	}
	mu.Unlock()
	// The listener the socket publishes is Info.CTL().
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := r.info.CTL()
	if st.Listener == nil || st.Listener.Addr != want.Addr || st.Listener.URL != want.URL || st.Listener.TLS != want.TLS || want.URL != r.info.Base {
		t.Errorf("published listener = %+v, want Info.CTL() = %+v", st.Listener, want)
	}
}

func TestDaemonWithoutShowUIHasNoRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.start(t, e.hub, nil)
	c, err := ctl.Dial(t.Context(), e.dirs.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := c.ShowUI(t.Context(), ""); err == nil || !strings.Contains(err.Error(), "no such control endpoint") {
		t.Errorf("ShowUI = %v, want the unknown-endpoint refusal", err)
	}
}
