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
	steps, err := install.Plan(ctx, h, install.Request{Scope: s})
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	data := installData{Scope: s, Plan: plan, Steps: steps}
	running := install.SystemdRunning(h)
	if plan {
		data.Next = install.Next(ctx, h, s)
		if !running {
			data.Next = append([]string{noSystemdNote}, data.Next...)
		}
		return Finish(cmd, data, planText(steps, running), nil)
	}
	if !running {
		return Finish(cmd, nil, "", errNoSystemd())
	}
	if err := install.Apply(ctx, h, steps, install.Hooks{}); err != nil {
		return Finish(cmd, nil, "", applyFailed(err))
	}
	data.Next = install.Next(ctx, h, s)
	return Finish(cmd, data, appliedText(steps, data.Next), nil)
}

const noSystemdNote = "systemd is not running here: applying this plan fails (in a container, run pcb serve as the entrypoint, see the README)"

// planText is a plan's text: one line per step, the systemd note, the
// reminder that nothing changed.
func planText(steps []install.Step, running bool) string {
	var b strings.Builder
	for _, st := range steps {
		b.WriteString(st.String() + "\n")
	}
	if !running {
		b.WriteString("Note: " + noSystemdNote + "\n")
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
	steps, err := install.PlanUninstall(ctx, h, install.UninstallRequest{Scope: s, DeleteData: deleteData})
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	data := installData{Scope: s, Plan: plan, Steps: steps, Next: []string{}}
	running := install.SystemdRunning(h)
	if plan {
		if !running && runsSystemctl(steps) {
			data.Next = []string{noSystemdNote}
		}
		return Finish(cmd, data, planText(steps, running || !runsSystemctl(steps)), nil)
	}
	if !running && runsSystemctl(steps) {
		return Finish(cmd, nil, "", errNoSystemd())
	}
	// The directories now, while the unit that names them exists.
	dirs, err := install.ScopeDirs(h, s)
	if err != nil {
		return Finish(cmd, nil, "", &exitcode.Error{Code: exitcode.Blocked, Err: err})
	}
	if deleteData && !yes {
		if err := a.confirmDelete(cmd, dirs); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	if err := install.Apply(ctx, h, steps, install.Hooks{Logout: a.logoutHook(h, dirs)}); err != nil {
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

// confirmDelete asks the person to type "delete"; without a person it
// fails naming --yes (exit 10).
func (a *app) confirmDelete(cmd *cobra.Command, dirs paths.Dirs) error {
	if !a.interactive(cmd) {
		return &exitcode.Error{Code: exitcode.MustAct,
			Err:  fmt.Errorf("--delete-data deletes %s and %s: confirm with --yes", dirs.Config, dirs.State),
			Hint: "pcb uninstall --delete-data --yes"}
	}
	answer, err := a.Term.ReadLine(cmd.Context(),
		fmt.Sprintf("Delete %s and %s (Proton session, cache, CalDAV clients)? Type delete to confirm: ", dirs.Config, dirs.State))
	if err != nil {
		return err
	}
	if strings.TrimSpace(answer) != "delete" {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("not confirmed: nothing was changed"),
			Hint: "type delete at the prompt, or pass --yes"}
	}
	return nil
}

// logoutHook revokes the session of the scope's directories with the
// in-process service, never the caller's own directories (M5-P19).
func (a *app) logoutHook(h install.Host, dirs paths.Dirs) func(context.Context) error {
	return func(ctx context.Context) error {
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
		res, err := svc.Logout(ctx)
		if err != nil {
			return err
		}
		if res.WasLoggedIn && !res.Revoked {
			return errors.New("the Proton session could not be revoked")
		}
		return nil
	}
}
