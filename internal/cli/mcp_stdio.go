package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The serve.lock race (design §5.3: the lock is the authority, the socket
// a hint): a pcb mcp that lost it waits lockFirstWait, doubling, and
// probes again, lockBudget in all.
const (
	lockFirstWait = 250 * time.Millisecond
	lockBudget    = 10 * time.Second
)

// lockBackoff is the waits of the race, in order: they add up to
// lockBudget.
func lockBackoff() []time.Duration {
	var out []time.Duration
	for d, total := lockFirstWait, time.Duration(0); total < lockBudget; d *= 2 {
		w := min(d, lockBudget-total)
		out = append(out, w)
		total += w
	}
	return out
}

// lostRace is a session daemon that could not start because another pcb
// holds the state (serve.lock, sync.lock or layout.lock): try again.
type lostRace struct{ err error }

func (l *lostRace) Error() string { return l.err.Error() }
func (l *lostRace) Unwrap() error { return l.err }

// runMCP serves the agent's session on stdin and stdout (design §5.3). At
// start, and again whenever the daemon it relays to goes away:
//  1. a daemon answers on the control socket: relay the session to it;
//  2. else, with a system-scope pcb unit installed: exit 20;
//  3. else, with a pcb user unit that systemd reports active or starting
//     (its Restart= gap included): wait for its daemon (IO.UnitWait, 30 s)
//     and relay to it, else exit 20 naming pcb service start; never take
//     the state from the unit, which would fail its restarts until it
//     gives up;
//  4. else become the session daemon (kind mcp, no listener) and serve
//     the session in process;
//  5. another pcb won the state meanwhile, or the daemon refused the
//     relay: wait (lockBackoff) and probe again; the waits spent, exit 20.
//
// The end of stdin ends it with exit 0, as does a signal.
func (a *app) runMCP(cmd *cobra.Command, g mcp.Grant) error {
	ctx := cmd.Context()
	if err := a.stateOwner(); err != nil {
		return err
	}
	dirs, cfg, err := loadConfig(a.Env)
	if err != nil {
		return err
	}
	lvl, err := config.ParseLevel(cfg.Log.Level)
	if err != nil { // Load validated it
		return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("log level %w", err), Hint: "use debug, info, warn or error"}
	}
	// stdout carries the MCP session only: every log line goes to stderr.
	log := slog.New(slog.NewTextHandler(a.Stderr, &slog.HandlerOptions{Level: lvl}))
	st := mcp.NewStdio(a.Stdin, a.Stdout)
	socket := dirs.Socket()
	waits := lockBackoff()
	for {
		up, err := a.daemonAnswers(ctx, socket)
		if err != nil {
			return err
		}
		if up {
			log.Info("relaying to the daemon on the control socket", "socket", socket)
			err := st.Relay(ctx, socket, g)
			switch {
			case errors.Is(err, mcp.ErrDaemonGone):
				log.Info("the daemon went away: looking for another, or serving the session here")
				waits = lockBackoff()
				continue
			case errors.Is(err, mcp.ErrRelayRefused):
				// Not a race, but waited for alike: no busy loop on a daemon
				// that answers its socket and refuses its /mcp.
				if len(waits) == 0 {
					return &exitcode.Error{Code: exitcode.Blocked, Err: err,
						Hint: "see the daemon's log; pcb mcp and the daemon must be the same pcb version"}
				}
				log.Warn("the daemon did not serve the relay: probing again", "error", err.Error(), "after", waits[0].String())
			default:
				return sessionEnd(ctx, err)
			}
		} else {
			if systemUnit(a.Host) {
				return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("pcb runs as a system service here"),
					Hint: "use its remote /mcp with a token ([mcp] remote = true; a loopback listen is enough)"}
			}
			if userUnitUp(ctx, a.Host) {
				wait := a.UnitWait
				if wait <= 0 {
					wait = defaultUnitWait
				}
				log.Info("the pcb user service is active or starting: waiting for its daemon", "for", wait.String())
				up, err := a.waitForDaemon(ctx, socket, wait)
				switch {
				case err != nil:
					return err
				case ctx.Err() != nil:
					return nil
				case up:
					waits = lockBackoff()
					continue
				}
				return &exitcode.Error{Code: exitcode.Blocked,
					Err:  errors.New("the pcb user service is installed and active or starting, but its daemon did not answer within " + wait.String()),
					Hint: "pcb mcp never takes the state from the service: see pcb service status, and run pcb service start if it is down"}
			}
			err = a.serveSession(ctx, st, g, dirs, cfg, log)
			var lost *lostRace
			if !errors.As(err, &lost) {
				return sessionEnd(ctx, err)
			}
			if len(waits) == 0 {
				return lost.err
			}
			log.Info("another pcb holds the state: probing again", "after", waits[0].String())
		}
		timer := time.NewTimer(waits[0])
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
		waits = waits[1:]
	}
}

// sessionEnd is the result of a session: a signal (ctx's end) is a normal
// end, as for pcb serve.
func sessionEnd(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// daemonAnswers reports whether a daemon serves the state on socket
// (ui.Probe's logic): one that is stopping counts as none.
func (a *app) daemonAnswers(ctx context.Context, socket string) (bool, error) {
	c, err := a.dialDaemon(ctx, socket)
	if err != nil || c == nil {
		return false, err
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Status(ctx); err != nil {
		return false, nil //nolint:nilerr // stopping: the race decides
	}
	return true, nil
}

// defaultUnitWait is how long pcb mcp waits for a starting user unit's
// daemon: longer than the unit's RestartSec (10 s) and its start.
const defaultUnitWait = 30 * time.Second

// unitPoll is how often that wait probes the control socket.
const unitPoll = 250 * time.Millisecond

// userUnitUp reports whether a pcb user unit is installed and systemd
// reports it active, starting (activating, its Restart= gap included),
// reloading or stopping on its way to a restart. A unit inactive, failed or
// that systemctl cannot tell about counts as down: the person stopped it,
// or there is no systemd user manager to ask.
func userUnitUp(ctx context.Context, h install.Host) bool {
	if scope, _, err := install.Detect(h); err != nil || scope != install.ScopeUser {
		return false
	}
	st, err := install.Show(ctx, h, install.ScopeUser)
	if err != nil {
		return false
	}
	switch st.ActiveState {
	case "active", "activating", "reloading", "refreshing", "deactivating":
		return true
	}
	return false
}

// waitForDaemon probes the control socket every unitPoll until a daemon
// answers (true) or wait has passed (false); ctx's end ends it (false).
func (a *app) waitForDaemon(ctx context.Context, socket string, wait time.Duration) (bool, error) {
	deadline := time.Now().Add(wait)
	for {
		up, err := a.daemonAnswers(ctx, socket)
		if err != nil || up {
			return up, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return false, nil
		}
		timer := time.NewTimer(min(unitPoll, left))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return false, nil
		}
	}
}

// systemUnit reports whether a system-scope pcb unit is installed (as pcb
// uninstall detects it): its state is another user's, which a session
// daemon here would serve unseen by its serve.lock.
func systemUnit(h install.Host) bool {
	scope, _, err := install.Detect(h)
	return scope == install.ScopeSystem || errors.Is(err, install.ErrAmbiguous)
}

// serveSession makes this process the session daemon (kind mcp: the
// control socket, its /mcp for other pcb mcp, no network listener) and
// serves st in process until stdin ends. A start refused because another
// pcb holds the state is a *lostRace.
func (a *app) serveSession(ctx context.Context, st *mcp.Stdio, g mcp.Grant, dirs paths.Dirs, cfg config.Config, log *slog.Logger) error {
	// Opening a new state chooses its sealer: refused before, not after.
	if err := sealerGuard(ctx, dirs, cfg, a.Keyring); err != nil {
		return err
	}
	hub, err := a.OpenHub(withLogger(ctx, log), a.Env)
	if err != nil {
		return raced(err)
	}
	defer func() {
		if err := hub.Close(); err != nil {
			fmt.Fprintf(a.Stderr, "Warning: closing the accounts failed: %s\n", err)
		}
	}()
	mh, ok := hub.(mcp.Hub)
	if !ok {
		return errors.New("these accounts cannot be served over MCP")
	}
	srv := mcp.NewServer(mcp.Options{Hub: mh, Zone: cfg.MCP.Location(), Logger: log, Version: buildinfo.Version,
		LoginGuard: func(ctx context.Context) error { return sealerGuard(ctx, dirs, cfg, a.Keyring) }})

	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	ready, ran := make(chan struct{}), make(chan error, 1)
	go func() {
		ran <- daemon.Run(runCtx, daemon.Options{
			Dirs: dirs, Config: cfg, Hub: hub, Kind: "mcp", LogLevel: cfg.Log.Level, Logger: log, Warn: a.Stderr,
			NoListener: true, WaitForLogin: true, MCP: srv,
			Ready: func(daemon.Info) { close(ready) },
		})
	}()
	select {
	case err := <-ran:
		return raced(err)
	case <-ready:
	}
	log.Info("serving this agent session: kind mcp, no network listener")
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	served := make(chan error, 1)
	go func() { served <- srv.ServeStdio(serveCtx, st, g) }()
	select {
	case err := <-served:
		stopRun()
		if derr := <-ran; err == nil {
			err = derr
		}
		return err
	case derr := <-ran: // the daemon ended on its own: a person must act
		stopServe()
		if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
			log.Warn("the agent session ended on an error", "error", err.Error())
		}
		return derr
	}
}

// raced marks a start another pcb blocked (exit 20 before the session
// daemon was ready) as a lost race.
func raced(err error) error {
	var ee *exitcode.Error
	if errors.As(err, &ee) && ee.Code == exitcode.Blocked {
		return &lostRace{err: err}
	}
	return err
}

// sealerGuard keeps pcb mcp from choosing how a new state's secrets are
// sealed (design §5.3): on a state with no layout.json yet, under
// [secrets] backend = auto, with no Secret Service answering (no session
// bus, none on it, or a locked keyring: auto would fall back to the key
// file), it refuses (must-act). An agent host that scrubs the session bus
// cannot fix a desktop's state to the key file by accident. An explicit
// backend, and a state whose sealer is recorded, pass.
//
// The session daemon checks it before it opens the accounts (opening a
// new state records its sealer), and passes it to the MCP server for its
// login tool.
func sealerGuard(ctx context.Context, dirs paths.Dirs, cfg config.Config, kr service.Keyring) error {
	if cfg.Secrets.Backend != "auto" {
		return nil
	}
	layout, err := doctor.Accounts(dirs.State)
	if err != nil || !layout.Fresh {
		return nil //nolint:nilerr // a state that exists, or that the accounts' opening reports
	}
	if r := (doctor.KeyringCheck{Keyring: kr}).Run(ctx); r.Status == doctor.StatusOK {
		return nil
	}
	return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("pcb mcp will not choose how secrets are sealed: set [secrets] backend in " +
		config.Path(dirs) + ", or run the first pcb login in a terminal")}
}
