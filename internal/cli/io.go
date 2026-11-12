package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
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
	// NewService opens the service a command calls (in process for now; the
	// socket client replaces it later). The command closes it.
	NewService func(ctx context.Context, env func(string) string) (service.API, error)
	// Listen opens pcb serve's listener (net.Listen).
	Listen func(network, address string) (net.Listener, error)
	// DialDaemon connects to pcb serve's control socket at path (ctl.Dial):
	// ctl.ErrNoDaemon when none answers.
	DialDaemon func(ctx context.Context, path string) (*ctl.Client, error)
}

// OSIO is the IO of the pcb process.
func OSIO(stdout, stderr io.Writer) IO {
	return IO{Stdout: stdout, Stderr: stderr, Env: os.Getenv, Term: newOSTerminal(stderr), NewService: localService, Listen: net.Listen,
		DialDaemon: ctl.Dial}
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
	if o.NewService == nil {
		o.NewService = localService
	}
	if o.Listen == nil {
		o.Listen = net.Listen
	}
	if o.DialDaemon == nil {
		o.DialDaemon = ctl.Dial
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

// localService resolves the directories, loads config.toml and returns the
// in-process service.
func localService(_ context.Context, env func(string) string) (service.API, error) {
	dirs, cfg, err := loadConfig(env)
	if err != nil {
		return nil, err
	}
	l, err := service.NewLocal(service.Options{Dirs: dirs, Config: cfg})
	if err != nil {
		return nil, err
	}
	return l, nil
}

// app is the state the pcb commands share.
type app struct{ IO }

// service opens the service for cmd's run.
func (a *app) service(cmd *cobra.Command) (service.API, error) {
	return a.NewService(cmd.Context(), a.Env)
}

// closeService releases svc. A failure is one line on stderr: it never
// changes the command's result or exit code.
func (a *app) closeService(svc service.API) {
	if err := svc.Close(); err != nil {
		fmt.Fprintf(a.Stderr, "Warning: closing the service failed: %s\n", err)
	}
}
