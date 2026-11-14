package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (a *app) newServe() *cobra.Command {
	var listen, logLevel string
	var waitForLogin bool
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the calendars over CalDAV and keep them in sync",
		Long: "Run the sync loop and the CalDAV listener in one process until Ctrl-C (SIGINT or SIGTERM: exit 0; a " +
			"second one while it stops: exit 20) or until you must log in again. With --wait-for-login it keeps " +
			"running instead, logged out or after the session ends: CalDAV answers 503 until a login completes " +
			"(pcb login, from any process), then serves again. The first lines name the URL to enter in Thunderbird " +
			"and, over TLS, the certificate fingerprint to check in its exception dialog. A loopback address serves " +
			"plain HTTP; any other serves TLS, with [caldav] tls_cert and tls_key or a self-signed certificate kept " +
			"in the state directory. Logs go to stderr.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if JSONMode(cmd) {
				return &exitcode.Error{Code: exitcode.Usage,
					Err:  errors.New("--json cannot be combined with serve: it runs until stopped and prints logs"),
					Hint: "drop --json"}
			}
			if cmd.Flags().Changed("listen") {
				if err := config.ValidateListen(listen); err != nil {
					return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("--listen %w", err),
						Hint: "use HOST:PORT, such as 127.0.0.1:1080 or 0.0.0.0:8443"}
				}
			}
			if cmd.Flags().Changed("log-level") {
				if _, err := config.ParseLevel(logLevel); err != nil {
					return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("--log-level %w", err),
						Hint: "use debug, info, warn or error"}
				}
			}
			dirs, cfg, err := loadConfig(a.Env)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("log-level") {
				cfg.Log.Level = logLevel
			}
			if cmd.Flags().Changed("listen") {
				cfg.CalDAV.Listen = listen
			}
			lvl, err := config.ParseLevel(cfg.Log.Level)
			if err != nil { // Load validated it
				return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("log level %w", err), Hint: "use debug, info, warn or error"}
			}
			// The accounts log on the daemon's logger: what they say while
			// serving (a login through the socket included) is its.
			log := slog.New(slog.NewTextHandler(a.Stderr, &slog.HandlerOptions{Level: lvl}))
			if err := a.stateOwner(); err != nil {
				return err
			}
			hub, err := a.OpenHub(withLogger(cmd.Context(), log), a.Env)
			if err != nil {
				return err
			}
			err = a.serve(cmd, hub, log, dirs, cfg, waitForLogin)
			if !errors.Is(err, daemon.ErrCut) { // cut: the process ends with the loops still running
				if cerr := hub.Close(); cerr != nil {
					fmt.Fprintf(a.Stderr, "Warning: closing the accounts failed: %s\n", cerr)
				}
			}
			return err
		},
	}
	c.Flags().StringVar(&listen, "listen", "", "HOST:PORT to listen on, over [caldav] listen (default "+config.DefaultListen+")")
	c.Flags().StringVar(&logLevel, "log-level", "", "debug, info, warn or error, over [log] level (default info)")
	c.Flags().BoolVar(&waitForLogin, "wait-for-login", false, "keep serving (CalDAV answers 503) while logged out or after the session ends, until a login")
	return NoJSON(c)
}

// waitingLine is what pcb serve --wait-for-login prints while it waits.
const waitingLine = "Waiting for a login: run pcb login"

// stopSignals watches sigs: stop is closed on the first signal or the end
// of ctx (Execute turns the first signal into that end too), cut on the
// second signal. end stops the watch.
func stopSignals(ctx context.Context, sigs <-chan os.Signal) (stop, cut <-chan struct{}, end func()) {
	stopC, cutC, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		var once sync.Once
		stopping := func() { once.Do(func() { close(stopC) }) }
		ctxDone := ctx.Done()
		for n := 0; ; {
			select {
			case <-done:
				return
			case <-ctxDone:
				stopping()
				ctxDone = nil
			case <-sigs:
				stopping()
				if n++; n == 2 {
					close(cutC)
					return
				}
			}
		}
	}()
	var once sync.Once
	return stopC, cutC, func() { once.Do(func() { close(done) }) }
}

// waitNotice prints waitingLine each time an account's loop begins waiting
// for a login, never before the intro: a wait reported earlier is printed
// right after it.
type waitNotice struct {
	mu        sync.Mutex
	w         io.Writer
	introDone bool
	waiting   bool
}

func (n *waitNotice) state(_ string, s service.ServeState) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.waiting = s == service.ServeWaitingForLogin
	if n.waiting && n.introDone {
		fmt.Fprintln(n.w, waitingLine)
	}
}

// intro marks the intro printed. none: there is no account, so no loop
// reports a wait (Hub.Serve has none to run) and the line is printed now.
func (n *waitNotice) intro(none bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.introDone = true
	if n.waiting || none {
		fmt.Fprintln(n.w, waitingLine)
	}
}

// serve runs the daemon (daemon.Run) on hub: a signal (the end of cmd's
// context, or the first of a.Signals) drains the listener and ends with
// exit 0; a second signal meanwhile ends at once with exit 20 (ruling
// M5-P22). Hub.Serve stopping on an error drains the listener and ends
// with that error.
func (a *app) serve(cmd *cobra.Command, hub Hub, log *slog.Logger, dirs paths.Dirs, cfg config.Config, waitForLogin bool) error {
	ctx := cmd.Context()
	sigs, unsubscribe := a.Signals()
	defer unsubscribe()
	stopping, cut, endSignals := stopSignals(ctx, sigs)
	defer endSignals()
	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	go func() {
		select {
		case <-stopping:
			stopRun()
		case <-runCtx.Done():
		}
	}()
	var counters daemon.RenderLog
	notice := &waitNotice{w: cmd.OutOrStdout()}
	var mcpServer *mcp.Server
	if mh, ok := hub.(mcp.Hub); ok { // the accounts' Hub always is; a test's stand-in may not be
		mcpServer = mcp.NewServer(mcp.Options{Hub: mh, Zone: cfg.MCP.Location(), Logger: log, Version: buildinfo.Version})
	}
	return daemon.Run(runCtx, daemon.Options{
		Dirs: dirs, Config: cfg, Hub: hub, Kind: "serve", LogLevel: cfg.Log.Level, MCP: mcpServer,
		Logger: log, Warn: a.Stderr, Listen: a.Listen, WaitForLogin: waitForLogin, Cut: cut,
		OnState: notice.state,
		OnPass: func(id string, p service.WatchPass) {
			l := log.With("account", id)
			daemon.LogPass(l, p)
			counters.Log(l, p.Render)
		},
		Ready: func(info daemon.Info) {
			n := a.printIntro(cmd, hub, log, info)
			notice.intro(waitForLogin && n == 0)
		},
	})
}

// printIntro writes what to enter in Thunderbird: the URL of each account
// CalDAV serves (neither a pending add nor one whose adapter is off), the
// fingerprint over TLS, and how to get a password when an account has no
// client. It returns how many accounts there are.
func (a *app) printIntro(cmd *cobra.Command, hub Hub, log *slog.Logger, info daemon.Info) int {
	w := cmd.OutOrStdout()
	accounts := hub.Accounts()
	var served []service.AccountInfo
	for _, ac := range accounts {
		if !ac.Pending && ac.CalDAV {
			served = append(served, ac)
		}
	}
	for _, ac := range served {
		fmt.Fprintf(w, "Thunderbird URL (%s): %s\n", accountLabel(ac), info.AccountURL(ac.ID))
	}
	if info.TLS {
		fmt.Fprintf(w, "TLS certificate SHA-256 fingerprint: %s (check it in Thunderbird's certificate exception dialog)\n", info.Fingerprint)
	}
	if len(accounts) == 0 {
		fmt.Fprintln(w, "No CalDAV client yet: run pcb login (it creates the client default), then pcb client show for its password.")
	}
	for _, ac := range served {
		svc, _, err := hub.Account(ac.ID)
		if err != nil {
			log.Warn("cannot read an account", "account", ac.ID, "error", err.Error())
			continue
		}
		clients, err := svc.Clients(cmd.Context())
		switch {
		case err != nil:
			log.Warn("cannot list the CalDAV clients", "account", ac.ID, "error", err.Error())
		case len(clients) == 0:
			// Decision 0013: a login creates the client default; once logged
			// in, only client add makes one.
			if ac.Phase == string(service.StateReady) {
				fmt.Fprintf(w, "No CalDAV client for %s: run pcb client add NAME --account %s, then use its password in Thunderbird.\n", accountLabel(ac), ac.ID)
			} else {
				fmt.Fprintf(w, "No CalDAV client yet for %s: run pcb login (it creates the client default), then pcb client show for its password.\n", accountLabel(ac))
			}
		}
	}
	return len(accounts)
}

// accountLabel is how the intro names an account: its address, else its ID.
func accountLabel(ac service.AccountInfo) string {
	if ac.Address != "" {
		return ac.Address
	}
	return ac.ID
}
