// Package ui is the non-GUI core of the desktop app pcb-ui: the Bridge runs
// the daemon in process, and Probe finds out whether another pcb process
// already serves the state directory. It holds no GUI toolkit.
package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// kind is what the control socket reports for this daemon.
const kind = "ui"

// tick is how often Changes fires on its own, so relative times ("3 min
// ago") move.
const tick = 30 * time.Second

// BridgeOptions configure a Bridge.
type BridgeOptions struct {
	// Env looks up an environment variable (nil: no variable is set).
	Env func(string) string
	// Logger gets the daemon's and the accounts' logs (nil: none).
	Logger *slog.Logger
	// Level, when set, is set to the config's [log] level at each Start.
	Level *slog.LevelVar
	// ShowUI raises the window when a second pcb-ui asks (nil: the control
	// socket has no ui/show). It never blocks the daemon: each call runs on
	// its own goroutine.
	ShowUI func(account string)
	// Listen opens the CalDAV listener (nil: net.Listen).
	Listen func(network, addr string) (net.Listener, error)
	// Keyring is the desktop keyring a state sealed with it uses (nil: the
	// session bus's).
	Keyring service.Keyring
}

// Bridge runs the daemon (what pcb serve runs) inside the desktop app.
type Bridge struct {
	o       BridgeOptions
	changes chan struct{}

	lmu sync.Mutex // serialises Start, Stop and Restart

	mu   sync.Mutex // guards the fields below
	run  *run       // nil while stopped
	hub  *service.Hub
	info daemon.Info
	// startErr is why the last Start (or Restart's) failed, nil once one
	// succeeded or after a Stop: Err reports it while no run is live.
	startErr error
}

// run is one Start..Stop of the daemon.
type run struct {
	cancel context.CancelFunc
	done   chan struct{} // closed when Run returned and the Hub is closed
	err    error         // Run's result and the Hub's Close, set before done closes
}

// ended says whether Run returned (and the Hub is closed).
func (r *run) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// failure is the error of a run that ended by itself: never nil.
func (r *run) failure() error {
	if r.err == nil {
		return errors.New("the daemon stopped on its own")
	}
	return r.err
}

// warnWriter sends the lines daemon.Run writes for a person (certificate
// warnings) to the log at warn level.
type warnWriter struct{ log *slog.Logger }

func (w warnWriter) Write(p []byte) (int, error) {
	w.log.Warn(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(p)), "Warning: ")))
	return len(p), nil
}

// NewBridge returns a stopped Bridge.
func NewBridge(o BridgeOptions) *Bridge {
	if o.Env == nil {
		o.Env = func(string) string { return "" }
	}
	return &Bridge{o: o, changes: make(chan struct{}, 1)}
}

// poke wakes the reader of Changes, once for any number of pokes it has not
// yet read.
func (b *Bridge) poke() {
	select {
	case b.changes <- struct{}{}:
	default:
	}
}

// Changes fires (capacity 1) when the Hub's accounts change, an account's
// loop changes state or finishes a pass, and every 30 s. It survives
// Restart.
func (b *Bridge) Changes() <-chan struct{} { return b.changes }

// Hub is the accounts of the running daemon: nil before the first Start. After
// Stop it is closed.
func (b *Bridge) Hub() *service.Hub {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hub
}

// Info describes the CalDAV listener of the running daemon (zero while
// stopped).
func (b *Bridge) Info() daemon.Info {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.info
}

// Start resolves the directories, loads config.toml, opens the Hub and runs
// the daemon (kind ui, waiting for a login) on its own goroutine. It returns
// once the listener is bound, or with the error Run ended with (a pcb serve
// holding serve.lock: exit 20); a failed Start leaves nothing running and
// the Hub closed. ctx bounds the wait only: the daemon runs until Stop.
func (b *Bridge) Start(ctx context.Context) error {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	return b.start(ctx)
}

// start is startRun, its failure kept for Err (review I1: a failed Restart
// must not read as a healthy bridge with no account).
func (b *Bridge) start(ctx context.Context) error {
	err := b.startRun(ctx)
	b.mu.Lock()
	b.startErr = err
	b.mu.Unlock()
	if err != nil {
		b.poke() // the UI learns that the bridge is stopped
	}
	return err
}

func (b *Bridge) startRun(ctx context.Context) error {
	if r := b.current(); r != nil {
		if !r.ended() {
			return errors.New("the daemon already runs")
		}
		b.clear(r) // it ended by itself: this Start replaces it
	}
	dirs, cfg, err := daemon.LoadConfig(b.o.Env)
	if err != nil {
		return err
	}
	if b.o.Level != nil {
		if lvl, err := config.ParseLevel(cfg.Log.Level); err == nil {
			b.o.Level.Set(lvl)
		}
	}
	log := b.o.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	hub, err := service.OpenHub(service.HubOptions{
		Options: service.Options{Dirs: dirs, Config: cfg, Logger: log},
		Keyring: b.o.Keyring,
	})
	if err != nil {
		return err
	}
	hub.OnChange(b.poke)
	var counters daemon.RenderLog

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r := &run{cancel: cancel, done: make(chan struct{})}
	ready := make(chan daemon.Info, 1)
	var showUI func(string) error
	if b.o.ShowUI != nil {
		// The control socket's handler goroutine must return quickly: the
		// callback runs on a goroutine of its own and the answer is "shown".
		showUI = func(account string) error { go b.o.ShowUI(account); return nil } //nolint:unparam // daemon.Options.ShowUI has an error to return
	}
	go func() {
		defer cancel()
		err := daemon.Run(runCtx, daemon.Options{
			Dirs: dirs, Config: cfg, Hub: hub, Kind: kind, LogLevel: cfg.Log.Level,
			Logger: log, Listen: b.o.Listen, WaitForLogin: true, ShowUI: showUI,
			OnState: func(string, service.ServeState) { b.poke() },
			// The same records as pcb serve's, for the same log file content.
			OnPass: func(id string, p service.WatchPass) {
				l := log.With("account", id)
				daemon.LogPass(l, p)
				counters.Log(l, p.Render)
				b.poke()
			},
			Warn:  warnWriter{log},
			Ready: func(i daemon.Info) { ready <- i },
		})
		// The caller opened the Hub, so this goroutine closes it, whichever
		// way Run ended (a failed Start included).
		r.err = errors.Join(err, hub.Close())
		close(r.done)
		b.poke() // the UI learns that the daemon ended, by itself or not
	}()

	select {
	case info := <-ready:
		b.mu.Lock()
		b.run, b.hub, b.info = r, hub, info
		b.mu.Unlock()
		if r.ended() { // Run ended as it became ready: not a running daemon
			b.clear(r)
			return r.failure()
		}
		go b.ticker(r)
		b.poke()
		return nil
	case <-r.done:
		b.mu.Lock()
		b.hub = hub
		b.mu.Unlock()
		return r.failure()
	case <-ctx.Done():
		cancel()
		<-r.done
		return ctx.Err()
	}
}

// ticker pokes Changes every tick until r ends.
func (b *Bridge) ticker(r *run) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			b.poke()
		case <-r.done:
			return
		}
	}
}

func (b *Bridge) current() *run {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.run
}

// Stop drains the CalDAV listener (5 s at most), stops the sync loops,
// closes the Hub and waits for all of it: serve.lock is free and pcb.sock
// gone when it returns. It returns what Run and the Hub's Close failed with;
// nil when the daemon is not running.
func (b *Bridge) Stop() error {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	b.forgetStartErr()
	return b.stop()
}

func (b *Bridge) stop() error {
	r := b.current()
	if r == nil {
		return nil
	}
	r.cancel()
	<-r.done
	b.clear(r)
	return r.err
}

// forgetStartErr drops a failed Start's error: a Stop is not a failure.
func (b *Bridge) forgetStartErr() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startErr = nil
}

// clear forgets r (an ended or stopped run).
func (b *Bridge) clear(r *run) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.run == r {
		b.run, b.info = nil, daemon.Info{}
	}
}

// Err is why the daemon ended by itself (Hub.Serve returned, the listener
// failed) or why the last Start or Restart failed (config.toml invalid, the
// Hub cannot open, serve.lock held): nil while it runs, after a Stop and
// before the first Start. It stays until the next Start or Restart; Changes
// fires when it is set.
func (b *Bridge) Err() error {
	b.mu.Lock()
	r, startErr := b.run, b.startErr
	b.mu.Unlock()
	if r == nil {
		return startErr
	}
	if !r.ended() {
		return nil
	}
	return r.failure()
}

// Restart is Stop then Start: config.toml is read again, so a new listen
// address or log level applies. When Stop failed, Start is not tried.
func (b *Bridge) Restart(ctx context.Context) error {
	b.lmu.Lock()
	defer b.lmu.Unlock()
	if r := b.current(); r != nil && !r.ended() {
		if err := b.stop(); err != nil {
			return err
		}
	}
	return b.start(ctx)
}

// ErrShown is what Probe returns when a running pcb-ui showed its window:
// the caller exits 0.
var ErrShown error = &exitcode.Error{Code: exitcode.OK, Err: errors.New("pcb-ui already runs; its window is shown")}

// ServeRunningError is what Probe returns when a pcb serve daemon serves the
// state directory: exit 20, with the hint to stop it.
type ServeRunningError struct{ PID int }

func (e *ServeRunningError) Error() string {
	return fmt.Sprintf("pcb serve (PID %d) already serves this state directory", e.PID)
}

// Unwrap gives the exit code and the hint.
func (e *ServeRunningError) Unwrap() error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New(e.Error()),
		Hint: "stop it (pcb service stop when installed as a service) and start pcb-ui again"}
}

// Probe asks the daemon on the state directory's control socket who it is:
// nil when none answers; ErrShown after a ui daemon showed its window
// (account selects what to show, "" for none); *ServeRunningError for a serve
// daemon. It is a fast path: Start still fails on serve.lock when another
// process wins the race.
func Probe(ctx context.Context, env func(string) string, account string) error {
	dirs, err := daemon.ResolveDirs(env)
	if err != nil {
		return err
	}
	c, err := ctl.Dial(ctx, dirs.Socket())
	if errors.Is(err, ctl.ErrNoDaemon) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(ctx)
	if err != nil {
		return err
	}
	if st.Daemon == nil {
		return errors.New("the daemon on the control socket did not say who it is")
	}
	if st.Daemon.Kind == kind {
		if err := c.ShowUI(ctx, account); err != nil {
			return err
		}
		return ErrShown
	}
	return &ServeRunningError{PID: st.Daemon.PID}
}
