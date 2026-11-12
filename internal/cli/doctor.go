package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

type doctorData struct {
	Results []doctor.Result `json:"results"`
}

func (a *app) newDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine can run pcb",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var stream func(doctor.Result)
			if !JSONMode(cmd) {
				stream = func(r doctor.Result) { printResult(cmd.OutOrStdout(), r) }
			}
			daemon := a.askDaemon(cmd.Context())
			results := doctor.Run(cmd.Context(), doctorChecks(a.Env, daemon.live()), stream)
			for _, r := range append([]doctor.Result{a.serviceResult(cmd.Context())}, daemon.results()...) {
				if stream != nil {
					stream(r)
				}
				results = append(results, r)
			}
			return Finish(cmd, doctorData{results}, "", doctorFailure(results))
		},
	}
}

// doctorFailure is pcb doctor's failure, nil without an error result (ux-I2):
// exit 20 when every error may pass by itself (Result.Transient: the
// daemon did not answer its control socket cleanly), else 10: a file, a
// mode, config.toml or an alert a person must fix.
func doctorFailure(results []doctor.Result) error {
	n, transient := 0, true
	for _, r := range results {
		if r.Status == doctor.StatusError {
			n++
			transient = transient && r.Transient
		}
	}
	if n == 0 {
		return nil
	}
	code := exitcode.MustAct
	if transient {
		code = exitcode.Blocked
	}
	what := "1 error"
	if n > 1 {
		what = fmt.Sprintf("%d errors", n)
	}
	return &exitcode.Error{Code: code, Err: errors.New("pcb doctor found " + what),
		Hint: "fix what the failed checks name (marked ✗), then run pcb doctor again"}
}

// doctorDaemon is what the doctor learnt from the control socket, asked
// once: the CalDAV checks use its listener, the daemon results its state.
type doctorDaemon struct {
	st       ctl.Status
	ok       bool
	err      error
	unplaced bool // the control socket cannot be located
}

func (a *app) askDaemon(ctx context.Context) doctorDaemon {
	dirs, err := paths.Resolve(a.Env)
	if err != nil { // the directory checks report it
		return doctorDaemon{unplaced: true}
	}
	st, ok, err := a.daemonStatus(ctx, dirs.Socket())
	return doctorDaemon{st: st, ok: ok, err: err}
}

// live is the running daemon's listener; nil without one.
func (d doctorDaemon) live() *doctor.Live {
	l := d.st.Listener
	if !d.ok || l == nil {
		return nil
	}
	return &doctor.Live{Addr: l.Addr, URL: l.URL, TLS: l.TLS, Fingerprint: l.Fingerprint, NotAfter: l.CertNotAfter}
}

// results are what a running pcb serve reports: "daemon", then one
// result per alert, named "alert CODE[ CALENDAR]" (an error alert fails,
// a warning warns). needs-login while the daemon waits for a login is the
// normal state of a fresh install: a warning (ux-I2). A daemon that does
// not answer cleanly is a transient error. With no daemon, "daemon" is
// skipped: offline checks only.
func (d doctorDaemon) results() []doctor.Result {
	const name = "daemon"
	st := d.st
	switch {
	case d.unplaced:
		return []doctor.Result{{Name: name, Status: doctor.StatusSkip, Message: "the control socket cannot be located: offline checks only"}}
	case d.err != nil:
		return []doctor.Result{{Name: name, Status: doctor.StatusError, Message: d.err.Error(), Fix: exitcode.HintOf(d.err), Transient: true}}
	case !d.ok:
		return []doctor.Result{{Name: name, Status: doctor.StatusSkip, Message: "not running: offline checks only"}}
	}
	results := []doctor.Result{{Name: name, Status: doctor.StatusOK, Message: daemonState(st)}}
	for _, al := range st.Alerts {
		r := doctor.Result{Name: "alert " + al.Code, Status: doctor.StatusWarn, Message: al.Message, Fix: al.Fix}
		if al.Calendar != "" {
			r.Name += " " + al.Calendar
		}
		waiting := st.Health != nil && st.Health.Serve == service.ServeWaitingForLogin
		if al.Severity == ctl.SeverityError && (al.Code != ctl.CodeNeedsLogin || !waiting) {
			r.Status = doctor.StatusError
		}
		results = append(results, r)
	}
	return results
}

// doctorChecks lists the checks, with one failing check per directory when
// the user directories cannot be located. env looks up an environment
// variable; live is the running daemon's listener (nil: none).
func doctorChecks(env func(string) string, live *doctor.Live) []doctor.Check {
	checks := []doctor.Check{doctor.PlatformCheck{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}}
	dirs, err := paths.Resolve(env)
	if err != nil {
		fix := func(override, xdg string) string {
			return "set HOME, " + override + " or " + xdg + " to an absolute path"
		}
		return append(checks,
			doctor.FailedCheck{Label: "config dir", Message: err.Error(), Fix: fix(paths.EnvConfigDir, "XDG_CONFIG_HOME")},
			doctor.FailedCheck{Label: "state dir", Message: err.Error(), Fix: fix(paths.EnvStateDir, "XDG_STATE_HOME")})
	}
	checks = append(checks,
		doctor.DirModeCheck{Label: "config dir", Path: dirs.Config, Want: 0o700},
		doctor.DirModeCheck{Label: "state dir", Path: dirs.State, Want: 0o700},
		doctor.ConfigCheck{Path: config.Path(dirs), Dirs: dirs})
	cfg, err := config.Load(config.Path(dirs), dirs)
	if err != nil { // ConfigCheck already reports it
		return checks
	}
	bind := &doctor.Bind{Listen: cfg.CalDAV.Listen} // resolved once for both CalDAV checks
	return append(checks,
		doctor.KeyFileCheck{Path: cfg.Secrets.KeyFile},
		doctor.FileModeCheck{Label: "sealed secrets", Path: filepath.Join(dirs.State, "secrets.sealed"), Want: 0o600},
		doctor.SecretsCheck{StateDir: dirs.State, KeyFile: cfg.Secrets.KeyFile},
		doctor.CacheCheck{StateDir: dirs.State},
		doctor.ListenerCheck{CalDAV: cfg.CalDAV, Bind: bind, Live: live},
		doctor.CertificateCheck{CalDAV: cfg.CalDAV, TLSDir: filepath.Join(dirs.State, "tls"), Bind: bind, Live: live},
		doctor.ClientsCheck{StateDir: dirs.State},
		doctor.SealedClientsCheck{StateDir: dirs.State, KeyFile: cfg.Secrets.KeyFile})
}

var marks = map[doctor.Status]string{
	doctor.StatusOK: "✓", doctor.StatusWarn: "!", doctor.StatusError: "✗", doctor.StatusSkip: "–",
}

// printResult writes one check's line, and its fix line. A failed write has
// nowhere left to be reported.
func printResult(w io.Writer, r doctor.Result) {
	_, _ = fmt.Fprintf(w, "%s  %s: %s\n", marks[r.Status], r.Name, r.Message)
	if r.Fix != "" {
		_, _ = fmt.Fprintf(w, "   fix: %s\n", r.Fix)
	}
}
