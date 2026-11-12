package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
)

// serviceAction is what start, stop and restart print with --json.
type serviceAction struct {
	Scope  install.Scope `json:"scope"`
	Unit   string        `json:"unit"`
	Action string        `json:"action"`
}

func (a *app) newService() *cobra.Command {
	var scope scopeFlags
	var subs []*cobra.Command
	for _, v := range []struct{ verb, short, done string }{
		{"start", "Start the pcb service", "started"},
		{"stop", "Stop the pcb service", "stopped"},
		{"restart", "Restart the pcb service", "restarted"},
	} {
		subs = append(subs, &cobra.Command{
			Use: v.verb, Short: v.short, Args: NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error { return a.serviceVerb(cmd, scope, v.verb, v.done) },
		})
	}
	status := &cobra.Command{
		Use: "status", Short: "Show the state of the pcb service", Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.serviceStatus(cmd, scope) },
	}
	var follow bool
	logs := &cobra.Command{
		Use: "logs", Short: "Print the last 200 lines of the pcb service's journal", Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.serviceLogs(cmd, scope, follow) },
	}
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	NoJSON(logs)
	g := Group("service", "Start, stop and inspect the pcb systemd service", append(subs, status, logs)...)
	scope.add(g, true)
	return g
}

const notInstalledHint = "pcb install"

// serviceScope is the scope a pcb service command acts on. Changing the
// system service (start, stop, restart: changes) needs root (M5-R7):
// refused before anything runs, as systemctl run without a terminal cannot
// ask for a password. Reading it (status, logs) does not.
func (a *app) serviceScope(scope scopeFlags, verb string, changes bool) (install.Scope, error) {
	s, err := scope.detectScope(a.Host, notInstalledHint)
	if err != nil {
		return "", err
	}
	if changes && s == install.ScopeSystem && a.Host.Euid() != 0 {
		return "", &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("needs root: sudo pcb service " + verb),
			Hint: "sudo pcb service " + verb}
	}
	return s, nil
}

func (a *app) serviceVerb(cmd *cobra.Command, scope scopeFlags, verb, done string) error {
	h := a.Host
	s, err := a.serviceScope(scope, verb, true)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	argv := install.Systemctl(s, verb, install.UnitName)
	var stderr bytes.Buffer
	code, err := h.Run.Stream(cmd.Context(), io.Discard, &stderr, argv[0], argv[1:]...)
	if err == nil && code != 0 {
		err = fmt.Errorf("systemctl %s exited with %d", verb, code)
		if line := install.LastLine(stderr.Bytes()); line != "" {
			err = fmt.Errorf("systemctl %s failed: %s", verb, line)
		}
	}
	if err != nil {
		return Finish(cmd, nil, "", &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: "pcb service logs"})
	}
	unit, _ := install.UnitPath(h, s)
	return Finish(cmd, serviceAction{Scope: s, Unit: unit, Action: verb},
		fmt.Sprintf("%s (%s): %s.", install.UnitName, s, done), nil)
}

func (a *app) serviceStatus(cmd *cobra.Command, scope scopeFlags) error {
	h := a.Host
	s, err := a.serviceScope(scope, "status", false)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	st, err := install.Show(cmd.Context(), h, s)
	if err != nil {
		return Finish(cmd, nil, "", &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: "check that systemd runs here: systemctl status"})
	}
	text := fmt.Sprintf("%s (%s): %s (%s), %s", install.UnitName, s, st.ActiveState, st.SubState, st.UnitFileState)
	if st.MainPID != 0 {
		text += fmt.Sprintf(", PID %d", st.MainPID)
	}
	return Finish(cmd, st, text, nil)
}

func (a *app) serviceLogs(cmd *cobra.Command, scope scopeFlags, follow bool) error {
	if JSONMode(cmd) {
		return UsageError(errors.New("pcb service logs streams the journal: --json is not supported"))
	}
	h := a.Host
	s, err := a.serviceScope(scope, "logs", false)
	if err != nil {
		return err
	}
	// Without root or the systemd-journal (or adm) group, journalctl shows
	// a system unit's lines as nothing: say why.
	hint := ""
	if s == install.ScopeSystem && h.Euid() != 0 {
		hint = journalHint
	}
	ctx := cmd.Context()
	argv := install.Journalctl(s, follow)
	out := &journalWriter{w: cmd.OutOrStdout()}
	code, err := h.Run.Stream(ctx, out, cmd.ErrOrStderr(), argv[0], argv[1:]...)
	switch {
	case ctx.Err() != nil: // Ctrl-C ends a follow
		return nil
	case err != nil:
		return &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: hint}
	case code != 0:
		return &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("journalctl exited with %d", code), Hint: hint}
	}
	if hint != "" && !out.entries {
		fmt.Fprintf(cmd.ErrOrStderr(), "Hint: %s\n", hint)
	}
	return nil
}

const journalHint = "system logs need root or the systemd-journal group: sudo pcb service logs"

// journalWriter passes journalctl's output on and notes whether it held
// any entry (anything but "-- No entries --").
type journalWriter struct {
	w       io.Writer
	entries bool
}

func (j *journalWriter) Write(p []byte) (int, error) {
	for _, l := range strings.Split(string(p), "\n") {
		if l = strings.TrimSpace(l); l != "" && l != "-- No entries --" {
			j.entries = true
		}
	}
	return j.w.Write(p)
}

// serviceResult is doctor's "service" result: the scope and ActiveState,
// or why there is none.
func (a *app) serviceResult(ctx context.Context) doctor.Result {
	const name = "service"
	h := a.Host
	if !install.SystemdRunning(h) {
		return doctor.Result{Name: name, Status: doctor.StatusSkip, Message: "no systemd here"}
	}
	s, _, err := install.Detect(h)
	switch {
	case errors.Is(err, install.ErrNotInstalled):
		return doctor.Result{Name: name, Status: doctor.StatusSkip, Message: "not installed as a service (pcb install)"}
	case err != nil:
		return doctor.Result{Name: name, Status: doctor.StatusWarn, Message: err.Error(), Fix: "pcb uninstall --user or pcb uninstall --system"}
	}
	st, err := install.Show(ctx, h, s)
	if err != nil {
		return doctor.Result{Name: name, Status: doctor.StatusWarn, Message: fmt.Sprintf("%s: %s", s, err)}
	}
	r := doctor.Result{Name: name, Status: doctor.StatusOK, Message: fmt.Sprintf("%s: %s", s, st.ActiveState)}
	if st.ActiveState != "active" {
		r.Status, r.Fix = doctor.StatusWarn, "pcb service logs, then pcb service start"
	}
	return r
}
