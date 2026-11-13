package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/daemon"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// IO is what pcb touches outside its own process state, so tests can replace
// each part.
type IO struct {
	Stdout, Stderr io.Writer
	// Env looks up an environment variable.
	Env func(string) string
	// Term asks the person questions.
	Term Terminal
	// OpenHub opens the accounts of the state directory env resolves to
	// (config.toml loaded), in process: the command closes it. ctx carries
	// the logger the accounts warn on (loggerFrom; nil: none).
	OpenHub func(ctx context.Context, env func(string) string) (Hub, error)
	// Listen opens pcb serve's listener (net.Listen).
	Listen func(network, address string) (net.Listener, error)
	// DialDaemon connects to pcb serve's control socket at path (ctl.Dial):
	// ctl.ErrNoDaemon when none answers.
	DialDaemon func(ctx context.Context, path string) (*ctl.Client, error)
	// Host is the machine pcb install, uninstall, service and doctor act
	// on (install.OSHost). One with Run set is used as it is; a nil Env
	// takes IO's Env.
	Host install.Host
	// Keyring is the desktop keyring pcb doctor probes for a state sealed
	// with it (nil: the session bus's, doctor.SystemKeyring).
	Keyring doctor.Keyring
	// Signals subscribes to SIGINT and SIGTERM (signal.Notify) and returns
	// the function that unsubscribes: pcb serve exits at once on a second
	// one while it stops (ruling M5-P22).
	Signals func() (<-chan os.Signal, func())
}

// osSignals is IO.Signals on the operating system's signals.
func osSignals() (<-chan os.Signal, func()) {
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	return c, func() { signal.Stop(c) }
}

// OSIO is the IO of the pcb process.
func OSIO(stdout, stderr io.Writer) IO {
	return IO{Stdout: stdout, Stderr: stderr, Env: os.Getenv, Term: newOSTerminal(stderr), OpenHub: localHub, Listen: net.Listen,
		DialDaemon: ctl.Dial, Host: install.OSHost(), Signals: osSignals}
}

// withDefaults fills the nil fields with the operating system's values.
func (o IO) withDefaults() IO {
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.Env == nil {
		o.Env = os.Getenv
	}
	if o.Term == nil {
		o.Term = newOSTerminal(o.Stderr)
	}
	if o.OpenHub == nil {
		o.OpenHub = localHub
	}
	if o.Listen == nil {
		o.Listen = net.Listen
	}
	if o.DialDaemon == nil {
		o.DialDaemon = ctl.Dial
	}
	if o.Keyring == nil {
		o.Keyring = doctor.SystemKeyring()
	}
	if o.Signals == nil {
		o.Signals = osSignals
	}
	if o.Host.Run == nil {
		o.Host = install.OSHost()
		o.Host.Env = nil
	}
	if o.Host.Env == nil {
		o.Host.Env = o.Env
	}
	return o
}

// resolveDirs resolves the config and state directories.
func resolveDirs(env func(string) string) (paths.Dirs, error) {
	dirs, err := paths.Resolve(env)
	if err != nil {
		return paths.Dirs{}, &exitcode.Error{Code: exitcode.Blocked, Err: err,
			Hint: "set HOME, or PCB_CONFIG_DIR and PCB_STATE_DIR, or XDG_CONFIG_HOME and XDG_STATE_HOME, to absolute paths"}
	}
	return dirs, nil
}

// loadConfig resolves the directories and loads config.toml.
func loadConfig(env func(string) string) (paths.Dirs, config.Config, error) {
	dirs, err := resolveDirs(env)
	if err != nil {
		return paths.Dirs{}, config.Config{}, err
	}
	cfg, err := config.Load(config.Path(dirs), dirs)
	if err != nil {
		return paths.Dirs{}, config.Config{}, err
	}
	return dirs, cfg, nil
}

// Hub is the accounts of a state directory (*service.Hub): what daemon.Run
// serves, the Hub calls the commands make, and Close.
type Hub interface {
	daemon.Hub
	Close() error
}

// localHub resolves the directories, loads config.toml and opens the
// in-process Hub, logging on the logger ctx carries (loggerFrom).
func localHub(ctx context.Context, env func(string) string) (Hub, error) {
	dirs, cfg, err := loadConfig(env)
	if err != nil {
		return nil, err
	}
	h, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: dirs, Config: cfg, Logger: loggerFrom(ctx)}})
	if err != nil {
		return nil, err
	}
	return h, nil
}

// app is the state the pcb commands share.
type app struct{ IO }

// hub opens the accounts for cmd's run, refusing root on another user's
// state (stateOwner). Its warnings are lines on stderr (newNoteLogger;
// ruling A2). The caller closes it (closeHub).
func (a *app) hub(cmd *cobra.Command) (Hub, error) {
	if err := a.stateOwner(); err != nil {
		return nil, err
	}
	return a.OpenHub(withLogger(cmd.Context(), newNoteLogger(a.Stderr)), a.Env)
}

// account opens the accounts and selects cmd's (selector): its API and
// what the Hub says of it. done closes the accounts.
func (a *app) account(cmd *cobra.Command) (api service.API, info service.AccountInfo, done func(), err error) {
	h, err := a.hub(cmd)
	if err != nil {
		return nil, service.AccountInfo{}, nil, err
	}
	api, info, err = h.Account(a.selector(cmd))
	if err != nil {
		a.closeHub(h)
		return nil, service.AccountInfo{}, nil, err
	}
	return api, info, func() { a.closeHub(h) }, nil
}

// stateOwner refuses to open the state as root on another user's state
// directory (service.CheckStateOwner). pcb install, uninstall and service
// run as root but do not open the state through here (uninstall's logout
// opens its own service).
func (a *app) stateOwner() error {
	if a.Host.Euid == nil {
		return nil
	}
	dirs, err := resolveDirs(a.Env)
	if err != nil {
		return nil //nolint:nilerr // the service reports it
	}
	return service.CheckStateOwner(dirs.State, a.Host.Euid(), a.Host.UserName)
}

// dialDaemon connects to the daemon on socket: nil, without an error, when
// none answers there (ctl.ErrNoDaemon). Any other failure is the caller's.
func (a *app) dialDaemon(ctx context.Context, socket string) (*ctl.Client, error) {
	c, err := a.DialDaemon(ctx, socket)
	if errors.Is(err, ctl.ErrNoDaemon) {
		return nil, nil
	}
	return c, err
}

// daemon is the client of the running daemon, nil when none answers: the
// command then runs in process (and directories that do not resolve are
// the in-process service's to report).
func (a *app) daemon(cmd *cobra.Command) (*ctl.Client, error) {
	dirs, err := resolveDirs(a.Env)
	if err != nil {
		return nil, nil //nolint:nilerr // the in-process path reports it
	}
	return a.dialDaemon(cmd.Context(), dirs.Socket())
}

// noDaemonNote tells, in text mode, that the command runs without a daemon:
// once the in-process accounts opened, so an error opening them stands alone.
func (a *app) noDaemonNote(cmd *cobra.Command) {
	if !JSONMode(cmd) {
		fmt.Fprintln(a.Stderr, "Note: no pcb daemon is running; this command runs on its own.")
	}
}

// closeHub releases h. A failure is one line on stderr: it never changes
// the command's result or exit code.
func (a *app) closeHub(h Hub) {
	if err := h.Close(); err != nil {
		fmt.Fprintf(a.Stderr, "Warning: closing the service failed: %s\n", err)
	}
}
