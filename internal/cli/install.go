package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// installData is what install and uninstall print with --json.
type installData struct {
	Scope install.Scope  `json:"scope"`
	Plan  bool           `json:"plan"`
	Steps []install.Step `json:"steps"`
	Next  []string       `json:"next"`
}

// scopeFlags are --user and --system.
type scopeFlags struct{ user, system bool }

func (f *scopeFlags) add(c *cobra.Command, persistent bool) {
	fs := c.Flags()
	if persistent {
		fs = c.PersistentFlags()
	}
	fs.BoolVar(&f.user, "user", false, "the user service (the caller's systemd user manager)")
	fs.BoolVar(&f.system, "system", false, "the system service (runs as the pcb user; needs root)")
}

// chosen is the scope the flags name, if any.
func (f scopeFlags) chosen() (install.Scope, bool, error) {
	switch {
	case f.user && f.system:
		return "", false, UsageError(errors.New("--user and --system exclude each other"))
	case f.user:
		return install.ScopeUser, true, nil
	case f.system:
		return install.ScopeSystem, true, nil
	}
	return "", false, nil
}

// detectScope is the scope the flags name, else the installed one.
func (f scopeFlags) detectScope(h install.Host, notInstalledHint string) (install.Scope, error) {
	s, ok, err := f.chosen()
	if err != nil || ok {
		return s, err
	}
	s, _, err = install.Detect(h)
	switch {
	case errors.Is(err, install.ErrAmbiguous):
		return "", &exitcode.Error{Code: exitcode.Usage, Err: err, Hint: "pass --user or --system"}
	case errors.Is(err, install.ErrNotInstalled):
		return "", &exitcode.Error{Code: exitcode.MustAct, Err: err, Hint: notInstalledHint}
	case err != nil:
		return "", &exitcode.Error{Code: exitcode.Blocked, Err: err}
	}
	return s, nil
}

func errNoSystemd() error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("systemd is not running here"),
		Hint: "in a container, run pcb serve as the entrypoint (README)"}
}

func (a *app) newInstall() *cobra.Command {
	var scope scopeFlags
	var plan bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install pcb as a systemd service (--user by default)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, ok, err := scope.chosen()
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			if !ok {
				s = install.ScopeUser
			}
			return a.install(cmd, s, plan)
		},
	}
	scope.add(c, false)
	c.Flags().BoolVar(&plan, "plan", false, "print the steps without changing anything")
	return c
}

func (a *app) install(cmd *cobra.Command, s install.Scope, plan bool) error {
	ctx := cmd.Context()
	h := a.Host
	if !plan {
		// Applying needs root for --system; planning never does (M5-R4).
		if err := install.NeedsRoot(h, s, "install"); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	steps, err := install.Plan(ctx, h, install.Request{Scope: s})
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	data := installData{Scope: s, Plan: plan, Steps: steps}
	running := install.SystemdRunning(h)
	if plan {
		notes := planNotes(h, s, "install", !running)
		data.Next = append(append([]string{}, notes...), install.Next(ctx, h, s, steps)...)
		return Finish(cmd, data, planText(steps, notes), nil)
	}
	if !running {
		return Finish(cmd, nil, "", errNoSystemd())
	}
	if err := install.Apply(ctx, h, steps, install.Hooks{}); err != nil {
		return Finish(cmd, nil, "", applyFailed(err))
	}
	data.Next = install.Next(ctx, h, s, steps)
	return Finish(cmd, data, appliedText(steps, data.Next), nil)
}

const noSystemdNote = "systemd is not running here: applying this plan fails (in a container, run pcb serve as the entrypoint, see the README)"

// planNotes are what a plan ends with: systemd missing (noSystemd), then
// root needed to apply it (M5-R4).
func planNotes(h install.Host, s install.Scope, command string, noSystemd bool) []string {
	notes := []string{}
	if noSystemd {
		notes = append(notes, noSystemdNote)
	}
	if install.NeedsRoot(h, s, command) != nil {
		notes = append(notes, install.RootNote(command))
	}
	return notes
}

// planText is a plan's text: one line per step, its notes, the reminder
// that nothing changed.
func planText(steps []install.Step, notes []string) string {
	var b strings.Builder
	for _, st := range steps {
		b.WriteString(st.String() + "\n")
	}
	for _, n := range notes {
		b.WriteString("Note: " + n + "\n")
	}
	b.WriteString("Plan only: nothing was changed.")
	return b.String()
}

// appliedText is the steps done, then what to run next.
func appliedText(steps []install.Step, next []string) string {
	var b strings.Builder
	for _, st := range steps {
		b.WriteString(st.String() + "\n")
	}
	if len(next) > 0 {
		b.WriteString("\nNext:\n")
		for _, n := range next {
			b.WriteString("  " + n + "\n")
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// applyFailed names the failed step (exit 40): the steps before it stay
// done, and a new run plans them as kept.
func applyFailed(err error) error {
	var se *install.StepError
	if errors.As(err, &se) {
		return &exitcode.Error{Code: exitcode.Partial, Err: err, Hint: "fix it, then run the command again: finished steps are kept"}
	}
	return err
}

func (a *app) newUninstall() *cobra.Command {
	var scope scopeFlags
	var deleteData, yes, plan bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the pcb systemd service (and with --delete-data, its data)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := scope.detectScope(a.Host, "nothing to uninstall; pass --user or --system to remove what is left")
			if plan && errors.Is(err, install.ErrNotInstalled) {
				// A plan is not a failure: there is nothing in it (M5-R16).
				const nothing = "Nothing to remove: pcb is not installed as a service (pass --user or --system to plan the removal of what is left)."
				return Finish(cmd, installData{Plan: true, Steps: []install.Step{}, Next: []string{}}, nothing+"\nPlan only: nothing was changed.", nil)
			}
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return a.uninstall(cmd, s, deleteData, yes, plan)
		},
	}
	scope.add(c, false)
	c.Flags().BoolVar(&deleteData, "delete-data", false, "also log out and delete the config and state directories")
	c.Flags().BoolVar(&yes, "yes", false, "confirm --delete-data without asking")
	c.Flags().BoolVar(&plan, "plan", false, "print the steps without changing anything")
	return c
}

func (a *app) uninstall(cmd *cobra.Command, s install.Scope, deleteData, yes, plan bool) error {
	ctx := cmd.Context()
	h := a.Host
	if !plan {
		if err := install.NeedsRoot(h, s, "uninstall"); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	// The plan resolves the directories once (M3): the logout step carries
	// them and the prompt names its remove steps.
	steps, err := install.PlanUninstall(ctx, h, install.UninstallRequest{Scope: s, DeleteData: deleteData})
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	data := installData{Scope: s, Plan: plan, Steps: steps}
	noSystemd := !install.SystemdRunning(h) && runsSystemctl(steps)
	if plan {
		data.Next = planNotes(h, s, "uninstall", noSystemd)
		return Finish(cmd, data, planText(steps, data.Next), nil)
	}
	data.Next = []string{}
	if noSystemd {
		return Finish(cmd, nil, "", errNoSystemd())
	}
	if d := deletionOf(steps); d.any() && !yes {
		if err := a.confirmDelete(cmd, d); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	if err := install.Apply(ctx, h, steps, install.Hooks{Logout: a.logoutHook(h)}); err != nil {
		return Finish(cmd, nil, "", applyFailed(err))
	}
	return Finish(cmd, data, appliedText(steps, nil), nil)
}

// runsSystemctl reports whether steps run systemctl.
func runsSystemctl(steps []install.Step) bool {
	for _, st := range steps {
		if st.Action == install.ActionRun && strings.HasPrefix(st.Command, "systemctl ") {
			return true
		}
	}
	return false
}

// deletion is what a --delete-data plan destroys: directories, the
// Proton session (logout), the system user.
type deletion struct {
	dirs   []string
	logout string // the state directory whose session is revoked
	user   bool
}

func deletionOf(steps []install.Step) deletion {
	d := deletion{dirs: install.RemovedDirs(steps)}
	for _, st := range steps {
		switch {
		case st.Action == install.ActionRun && st.Kind == install.KindLogout:
			d.logout = st.Path
		case st.Action == install.ActionRun && st.Kind == install.KindUser && strings.HasPrefix(st.Command, "userdel "):
			d.user = true
		}
	}
	return d
}

func (d deletion) any() bool { return len(d.dirs) > 0 || d.logout != "" || d.user }

// question is the prompt's question, in the brief's words when directories
// go (their "Proton session" covers the logout).
func (d deletion) question() string {
	var q string
	switch {
	case len(d.dirs) > 0:
		q = "Delete " + strings.Join(d.dirs, " and ") + " (Proton session, cache, CalDAV clients)"
		if d.user {
			q += " and the user " + install.SystemUser
		}
	case d.logout != "":
		q = "Log out of Proton (the session in " + d.logout + ")"
		if d.user {
			q += " and delete the user " + install.SystemUser
		}
	default:
		q = "Delete the user " + install.SystemUser
	}
	return q + "?"
}

// summary lists what goes, for the refusal without a person.
func (d deletion) summary() string {
	var what []string
	if len(d.dirs) > 0 {
		what = append(what, "deletes "+strings.Join(d.dirs, " and "))
	}
	if d.logout != "" {
		what = append(what, "logs out of Proton")
	}
	if d.user {
		what = append(what, "deletes the user "+install.SystemUser)
	}
	return strings.Join(what, ", ")
}

// confirmDelete asks the person to type "delete" before anything of d goes
// (M5-Rpre5: --delete-data needs a confirmation or --yes); without a
// person it fails naming --yes (exit 10).
func (a *app) confirmDelete(cmd *cobra.Command, d deletion) error {
	if !a.interactive(cmd) {
		return &exitcode.Error{Code: exitcode.MustAct,
			Err:  fmt.Errorf("--delete-data %s: confirm with --yes", d.summary()),
			Hint: "pcb uninstall --delete-data --yes"}
	}
	answer, err := a.Term.ReadLine(cmd.Context(), d.question()+" Type delete to confirm: ")
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) != "delete" {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("not confirmed: nothing was changed"),
			Hint: "type delete at the prompt, or pass --yes"}
	}
	return nil
}

// logoutHook revokes the session of the directories the plan resolved,
// with the in-process service, never the caller's own (M5-P19).
func (a *app) logoutHook(h install.Host) func(context.Context, paths.Dirs) error {
	return func(ctx context.Context, dirs paths.Dirs) error {
		env := func(k string) string {
			switch k {
			case paths.EnvConfigDir:
				return h.Path(dirs.Config)
			case paths.EnvStateDir:
				return h.Path(dirs.State)
			}
			return a.Env(k)
		}
		svc, err := a.NewService(ctx, env)
		if err != nil {
			return err
		}
		defer a.closeService(svc)
		res, err := svc.Logout(ctx, service.LogoutRequest{})
		if err != nil {
			return err
		}
		if res.WasLoggedIn && !res.Revoked {
			return errors.New("the Proton session could not be revoked")
		}
		return nil
	}
}
