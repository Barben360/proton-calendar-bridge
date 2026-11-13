package ui_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/tlsutil"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// lab is a fake Proton and the PCB_* directories of a state that talks to it.
type lab struct {
	f    *protontest.Fake
	dirs paths.Dirs
	env  func(string) string
}

func newLab(t *testing.T) *lab {
	t.Helper()
	f := protontest.New(t, protontest.Options{})
	root, err := os.MkdirTemp("", "pcbu") // short: the control socket's path must fit sun_path
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dirs := paths.Dirs{Config: filepath.Join(root, "cfg"), State: filepath.Join(root, "state")}
	l := &lab{f: f, dirs: dirs, env: func(k string) string {
		switch k {
		case "PCB_CONFIG_DIR":
			return dirs.Config
		case "PCB_STATE_DIR":
			return dirs.State
		}
		return ""
	}}
	l.writeConfig(t, "127.0.0.1:0")
	return l
}

func (l *lab) writeConfig(t *testing.T, listen string) {
	t.Helper()
	if err := os.MkdirAll(l.dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[proton]\nbase_url = %q\n[caldav]\nlisten = %q\n", l.f.URL, listen)
	if err := os.WriteFile(config.Path(l.dirs), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (l *lab) bridge(t *testing.T, show func(string)) *ui.Bridge {
	t.Helper()
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, ShowUI: show})
	t.Cleanup(func() { _ = b.Stop() })
	return b
}

func (l *lab) start(t *testing.T) *ui.Bridge {
	t.Helper()
	b := l.bridge(t, nil)
	if err := b.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return b
}

func (l *lab) login(t *testing.T, b *ui.Bridge) string {
	t.Helper()
	res, err := b.Hub().Login(t.Context(), service.LoginRequest{Username: l.f.Username, Password: l.f.Password})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	return res.Account
}

func TestBridgeServesCalDAV(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	id := l.login(t, b)
	info := b.Info()
	_, port, _ := net.SplitHostPort(info.Addr)
	if got := info.AccountURL(id); !strings.HasSuffix(got, ":"+port+service.AccountPath(id)) {
		t.Fatalf("account URL = %q, want it to name the bound port %s", got, port)
	}
	url := "http://" + info.Addr + service.AccountPath(id)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if got, want := resp.Header.Get("WWW-Authenticate"), `realm="pcb `+id+`"`; !strings.Contains(got, want) {
		t.Errorf("WWW-Authenticate = %q, want it to hold %s", got, want)
	}
}

func TestQuitDrainsAndReleases(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	// The first sync pass of the new account stalls on Proton.
	l.f.StallNext("GET", "/calendar/v1", 30*time.Second)
	l.login(t, b)
	deadline := time.Now().Add(10 * time.Second)
	for l.f.Calls("GET", "/calendar/v1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the sync pass never reached Proton")
		}
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	if err := b.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("Stop took %s, want within the 5 s drain plus a margin", d)
	}
	if _, err := os.Stat(l.dirs.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pcb.sock outlives Stop: %v", err)
	}
	unlock, ok, err := filelock.TryLock(filepath.Join(l.dirs.State, "serve.lock"))
	if err != nil || !ok {
		t.Fatalf("serve.lock is still held after Stop: ok=%v err=%v", ok, err)
	}
	unlock()
}

func TestRestartReadsTheConfig(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	first := b.Info().Addr
	// A free port the new config names.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	want := ln.Addr().String()
	_ = ln.Close()
	l.writeConfig(t, want)
	if err := b.Restart(t.Context()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := b.Info().Addr; got != want || got == first {
		t.Fatalf("Info().Addr = %s after Restart, want the new config's %s (was %s)", got, want, first)
	}
	conn, err := net.DialTimeout("tcp", want, 2*time.Second)
	if err != nil {
		t.Fatalf("nothing listens on %s after Restart: %v", want, err)
	}
	_ = conn.Close()
	if _, err := net.DialTimeout("tcp", first, time.Second); err == nil {
		t.Errorf("the old address %s still answers", first)
	}
}

type shown struct {
	mu   sync.Mutex
	got  []string
	seen chan struct{}
}

func newShown() *shown { return &shown{seen: make(chan struct{}, 8)} }

func (s *shown) show(account string) {
	s.mu.Lock()
	s.got = append(s.got, account)
	s.mu.Unlock()
	s.seen <- struct{}{}
}

func TestSecondInstanceShowsTheFirst(t *testing.T) {
	l := newLab(t)
	rec := newShown()
	buf := new(logBuf)
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, ShowUI: rec.show, Logger: slog.New(slog.NewTextHandler(buf, nil))})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	l.login(t, b)
	buf.waitFor(t, "sync pass") // the first pass is over: it is not a call of the probe
	calls := func() int {
		return l.f.Calls("GET", "/calendar/v1") + l.f.Calls("POST", "/auth/v4") + l.f.Calls("POST", "/auth/v4/info")
	}
	n := calls()

	err := ui.Probe(t.Context(), l.env, "acc-1")
	if !errors.Is(err, ui.ErrShown) {
		t.Fatalf("Probe = %v, want ErrShown", err)
	}
	if exitcode.From(err) != exitcode.OK {
		t.Errorf("ErrShown exit code = %d, want 0", exitcode.From(err))
	}
	select {
	case <-rec.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("ShowUI was never called")
	}
	rec.mu.Lock()
	got := append([]string(nil), rec.got...)
	rec.mu.Unlock()
	if len(got) != 1 || got[0] != "acc-1" {
		t.Errorf("ShowUI got %v, want [acc-1]", got)
	}
	if after := calls(); after != n {
		t.Errorf("Proton saw %d new calls from the probe, want none", after-n)
	}
}

func TestProbeNoDaemon(t *testing.T) {
	l := newLab(t)
	if err := ui.Probe(t.Context(), l.env, ""); err != nil {
		t.Fatalf("Probe with no daemon = %v, want nil", err)
	}
}

func TestProbeNamesARunningServe(t *testing.T) {
	l := newLab(t)
	cfg, err := config.Load(config.Path(l.dirs), l.dirs)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: l.dirs, Config: cfg}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	ready, fin := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(fin)
		_ = daemon.Run(ctx, daemon.Options{Dirs: l.dirs, Config: cfg, Hub: hub, Kind: "serve", WaitForLogin: true,
			Ready: func(daemon.Info) { close(ready) }})
	}()
	t.Cleanup(func() { cancel(); <-fin })
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the serve daemon never got ready")
	}

	err = ui.Probe(t.Context(), l.env, "acc-1")
	var sr *ui.ServeRunningError
	if !errors.As(err, &sr) {
		t.Fatalf("Probe = %v, want *ServeRunningError", err)
	}
	if sr.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", sr.PID, os.Getpid())
	}
	if want := fmt.Sprintf("pcb serve (PID %d) already serves this state directory", os.Getpid()); err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if exitcode.From(err) != exitcode.Blocked {
		t.Errorf("exit code = %d, want 20", exitcode.From(err))
	}
	if got, want := exitcode.HintOf(err), "stop it (pcb service stop when installed as a service) and start pcb-ui again"; got != want {
		t.Errorf("hint = %q, want %q", got, want)
	}
}

func TestStartBlockedByAServeLeaksNothing(t *testing.T) {
	l := newLab(t)
	cfg, _ := config.Load(config.Path(l.dirs), l.dirs)
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: l.dirs, Config: cfg}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	ready, fin := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(fin)
		_ = daemon.Run(ctx, daemon.Options{Dirs: l.dirs, Config: cfg, Hub: hub, Kind: "serve", WaitForLogin: true,
			Ready: func(daemon.Info) { close(ready) }})
	}()
	t.Cleanup(func() { cancel(); <-fin })
	<-ready

	b := l.bridge(t, nil)
	err = b.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded against a daemon holding serve.lock")
	}
	if exitcode.From(err) != exitcode.Blocked {
		t.Errorf("Start error exit = %d (%v), want 20", exitcode.From(err), err)
	}
	if err := b.Stop(); err != nil {
		t.Errorf("Stop after a failed Start = %v, want nil", err)
	}
	// Nothing leaked: the Hub of the failed Start is closed, so a second
	// Start after the serve ends works.
	cancel()
	<-fin
	if err := b.Start(t.Context()); err != nil {
		t.Fatalf("Start after the serve stopped: %v", err)
	}
}

func TestChangesFireOnLogin(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	select { // drain what Start itself poked
	case <-b.Changes():
	default:
	}
	l.login(t, b)
	select {
	case <-b.Changes():
	case <-time.After(5 * time.Second):
		t.Fatal("Changes did not fire on a login")
	}
}

// logBuf is a log sink safe for the daemon's goroutines.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func (l *logBuf) waitFor(t *testing.T, sub string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(l.String(), sub) {
		if time.Now().After(deadline) {
			t.Fatalf("the log never held %q:\n%s", sub, l.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return l.String()
}

func TestBridgeLogsPassesLikeServe(t *testing.T) {
	l := newLab(t)
	buf := new(logBuf)
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, Logger: slog.New(slog.NewTextHandler(buf, nil))})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := l.login(t, b)
	out := buf.waitFor(t, "sync pass")
	if !strings.Contains(out, "account="+id) {
		t.Errorf("the pass record does not name the account %s:\n%s", id, out)
	}
}

func TestBridgeLogsTheCertificateWarning(t *testing.T) {
	l := newLab(t)
	tlsDir := filepath.Join(l.dirs.State, "tls")
	if _, err := tlsutil.SelfSigned(tlsutil.Options{Dir: tlsDir, BindIP: net.IPv4(127, 0, 0, 1),
		Now: func() time.Time { return time.Now().Add(-1000 * 24 * time.Hour) }}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("[proton]\nbase_url = %q\n[caldav]\nlisten = \"127.0.0.1:0\"\ntls_cert = %q\ntls_key = %q\n",
		l.f.URL, filepath.Join(tlsDir, "cert.pem"), filepath.Join(tlsDir, "key.pem"))
	if err := os.WriteFile(config.Path(l.dirs), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	buf := new(logBuf)
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, Logger: slog.New(slog.NewTextHandler(buf, nil))})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	out := buf.waitFor(t, "expired on")
	if !strings.Contains(out, "level=WARN") {
		t.Errorf("the certificate warning is not at warn level:\n%s", out)
	}
}

// dyingListener accepts nothing and fails its Accept when kill closes: the
// CalDAV listener of the daemon fails.
type dyingListener struct {
	net.Listener
	kill   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (d *dyingListener) Accept() (net.Conn, error) {
	select {
	case <-d.kill:
		return nil, errors.New("listener broke")
	case <-d.closed:
		return nil, net.ErrClosed
	}
}

func (d *dyingListener) Close() error {
	d.once.Do(func() { close(d.closed) })
	return d.Listener.Close()
}

func TestBridgeNoticesTheDaemonEnding(t *testing.T) {
	l := newLab(t)
	kill := make(chan struct{})
	var calls int
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, Listen: func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if calls++; err != nil || calls > 1 {
			return ln, err
		}
		return &dyingListener{Listener: ln, kill: kill, closed: make(chan struct{})}, nil
	}})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.Err(); err != nil {
		t.Fatalf("Err while running = %v, want nil", err)
	}
	select {
	case <-b.Changes():
	default:
	}
	close(kill)
	select {
	case <-b.Changes():
	case <-time.After(10 * time.Second):
		t.Fatal("Changes did not fire when the daemon ended")
	}
	deadline := time.Now().Add(5 * time.Second)
	for b.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	err := b.Err()
	if err == nil || !strings.Contains(err.Error(), "listener failed") {
		t.Fatalf("Err = %v, want the listener failure", err)
	}
	if err := b.Restart(t.Context()); err != nil {
		t.Fatalf("Restart after the daemon ended: %v", err)
	}
	if err := b.Err(); err != nil {
		t.Errorf("Err after the Restart = %v, want nil", err)
	}
	conn, err := net.DialTimeout("tcp", b.Info().Addr, 2*time.Second)
	if err != nil {
		t.Fatalf("nothing serves after the Restart: %v", err)
	}
	_ = conn.Close()
}

// A Restart that fails (here a config.toml edited with an invalid log
// level, the Settings flow) shows the bridge stopped with its reason and the
// attention tray, and the actions refuse; a Restart with a fixed config
// clears it (review I1, ruling R7: never a dead bridge shown alive).
func TestFailedRestartShowsTheBridgeStopped(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	l.login(t, b)
	good, err := os.ReadFile(config.Path(l.dirs))
	if err != nil {
		t.Fatal(err)
	}
	bad := append(append([]byte{}, good...), "[log]\nlevel = \"loud\"\n"...)
	if err := os.WriteFile(config.Path(l.dirs), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	rerr := b.Restart(t.Context())
	if rerr == nil {
		t.Fatal("Restart with an invalid config.toml succeeded")
	}
	if b.Err() == nil {
		t.Error("Err() = nil after a failed Restart")
	}
	v := b.View(t.Context(), time.Now())
	if want := "bridge stopped: " + rerr.Error(); v.Bridge != want {
		t.Errorf("View.Bridge = %q, want %q", v.Bridge, want)
	}
	if v.Tray != ui.TrayAttention {
		t.Errorf("View.Tray = %q, want attention", v.Tray)
	}
	refused(t, ui.NewActions(b))

	if err := os.WriteFile(config.Path(l.dirs), good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Restart(t.Context()); err != nil {
		t.Fatalf("Restart with the fixed config: %v", err)
	}
	if b.Err() != nil {
		t.Errorf("Err() = %v after a good Restart, want nil", b.Err())
	}
	if v := b.View(t.Context(), time.Now()); v.Bridge != "" || len(v.Accounts) != 1 {
		t.Errorf("after a good Restart: Bridge = %q, %d accounts; want none and the one account", v.Bridge, len(v.Accounts))
	}
}

// An agent session (pcb mcp's session daemon, kind mcp, no listener)
// blocks pcb-ui like a pcb serve, and the dialog says so: the probe names
// the session, its PID and kind, with the hint to end it; a Start that
// loses the race to it gets serve.lock's refusal, worded the same way.
func TestBridgeNamesAnAgentSession(t *testing.T) {
	l := newLab(t)
	cfg, err := config.Load(config.Path(l.dirs), l.dirs)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: l.dirs, Config: cfg}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	ready, fin := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(fin)
		_ = daemon.Run(ctx, daemon.Options{Dirs: l.dirs, Config: cfg, Hub: hub, Kind: "mcp", NoListener: true, WaitForLogin: true,
			Ready: func(daemon.Info) { close(ready) }})
	}()
	t.Cleanup(func() { cancel(); <-fin })
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("the session daemon never got ready")
	}
	const hint = "end the agent session (pcb mcp), then try again"

	err = ui.Probe(t.Context(), l.env, "")
	var sr *ui.ServeRunningError
	if !errors.As(err, &sr) || sr.PID != os.Getpid() || sr.Kind != "mcp" {
		t.Fatalf("Probe = %#v, want a *ServeRunningError of PID %d, kind mcp", err, os.Getpid())
	}
	if want := fmt.Sprintf("an agent session (pcb mcp, PID %d, kind mcp) already serves this state directory", os.Getpid()); err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if exitcode.From(err) != exitcode.Blocked || exitcode.HintOf(err) != hint {
		t.Errorf("exit %d, hint %q; want 20, %q", exitcode.From(err), exitcode.HintOf(err), hint)
	}

	b := ui.NewBridge(ui.BridgeOptions{Env: l.env})
	t.Cleanup(func() { _ = b.Stop() })
	err = b.Start(t.Context())
	if want := fmt.Sprintf("pcb is already serving (pid %d, kind mcp)", os.Getpid()); err == nil || err.Error() != want {
		t.Errorf("Start = %v, want %q", err, want)
	}
	if exitcode.From(err) != exitcode.Blocked || exitcode.HintOf(err) != hint {
		t.Errorf("Start: exit %d, hint %q; want 20, %q", exitcode.From(err), exitcode.HintOf(err), hint)
	}
}
