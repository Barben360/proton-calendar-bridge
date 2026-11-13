package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

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
			results := doctor.Run(cmd.Context(), doctor.Checks(a.Env, a.Host, a.Keyring, daemon.live()), stream)
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
// result per alert of the process, named "alert CODE[ CALENDAR]", and of
// each account, named "ACCOUNT: alert CODE[ CALENDAR]" (an error alert
// fails, a warning warns). needs-login while the account's loop waits for
// a login is the normal state of a fresh install: a warning (ux-I2). A
// daemon that does not answer cleanly is a transient error, a control
// socket pcb refuses is not. With no daemon, "daemon" is skipped: offline
// checks only.
func (d doctorDaemon) results() []doctor.Result {
	const name = "daemon"
	st := d.st
	switch {
	case d.unplaced:
		return []doctor.Result{{Name: name, Status: doctor.StatusSkip, Message: "the control socket cannot be located: offline checks only"}}
	case d.err != nil:
		// A dial or status that failed may pass; a socket pcb refuses
		// (ctl.ErrRefused) needs a person (ruling A11).
		return []doctor.Result{{Name: name, Status: doctor.StatusError, Message: d.err.Error(), Fix: exitcode.HintOf(d.err),
			Transient: !errors.Is(d.err, ctl.ErrRefused)}}
	case !d.ok:
		return []doctor.Result{{Name: name, Status: doctor.StatusSkip, Message: "not running: offline checks only"}}
	}
	results := []doctor.Result{{Name: name, Status: doctor.StatusOK, Message: daemonText(st.Daemon)}}
	// The process's needs-login means no account: a daemon that answers
	// with none waits for a login (pcb serve exits 10 without
	// --wait-for-login), a warning as for an account (ruling R44).
	results = append(results, alertResults("", st.Alerts, true)...)
	for _, as := range st.Accounts {
		waiting := as.Health != nil && as.Health.Serve == service.ServeWaitingForLogin
		results = append(results, alertResults(accountLabel(as.AccountInfo)+": ", as.Alerts, waiting)...)
	}
	return results
}

// daemonText is "running since T (pid N, version V, kind K)": the kind
// (serve, ui or mcp) tells an agent session from pcb serve or pcb-ui; an
// older daemon that sends none leaves it out.
func daemonText(d *ctl.Daemon) string {
	if d == nil {
		return "running"
	}
	kind := ""
	if d.Kind != "" {
		kind = ", kind " + d.Kind
	}
	return fmt.Sprintf("running since %s (pid %d, version %s%s)", d.StartedAt.Format(time.RFC3339), d.PID, d.Version, kind)
}

// alertResults is one result per alert, its name prefixed: an error alert
// fails, but needs-login while waiting for a login only warns.
func alertResults(prefix string, alerts []ctl.Alert, waiting bool) []doctor.Result {
	var out []doctor.Result
	for _, al := range alerts {
		r := doctor.Result{Name: prefix + "alert " + al.Code, Status: doctor.StatusWarn, Message: al.Message, Fix: al.Fix}
		if al.Calendar != "" {
			r.Name += " " + al.Calendar
		}
		if al.Severity == ctl.SeverityError && (al.Code != ctl.CodeNeedsLogin || !waiting) {
			r.Status = doctor.StatusError
		}
		out = append(out, r)
	}
	return out
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
