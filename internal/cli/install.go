package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// installData is what install and uninstall print with --json.
type installData struct {
	Scope install.Scope  `json:"scope"`
	Plan  bool           `json:"plan"`
	Steps []install.Step `json:"steps"`
	Next  []string       `json:"next"`
	// Logout is what the uninstall's logout step got from Proton, one
	// entry per account that answered, as pcb logout prints it; absent when
	// no logout ran or none got an answer (its step's note says why).
	Logout []uninstallLogout `json:"logout,omitempty"`
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

// errNoService is the refusal of install and service on Windows, where
// pcb-ui's "Start at login" is the only way to run at login.
func errNoService() error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("no service on Windows"),
		Hint: `use pcb-ui; "Start at login" runs it at each login`}
}

func (a *app) newInstall() *cobra.Command {
	var scope scopeFlags
	var plan bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install pcb as a systemd service (--user by default)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.GOOS == "windows" {
				return Finish(cmd, nil, "", errNoService())
			}
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
		Short: "Remove the pcb systemd service (and with --delete-data, its data; without a service, pcb-ui's)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.GOOS == "windows" {
				return a.uninstallWindows(cmd, deleteData, yes, plan)
			}
			s, err := scope.detectScope(a.Host, "nothing to uninstall; pass --user or --system to remove what is left")
			if deleteData && errors.Is(err, install.ErrNotInstalled) {
				// No service: the data of this session's pcb (pcb-ui).
				return a.uninstallDesktop(cmd, yes, plan)
			}
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
	c.Flags().BoolVar(&deleteData, "delete-data", false, "also log out and delete the config and state directories (with no service installed: this shell's, the keyring item and pcb-ui's autostart entry)")
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
	hooks := install.Hooks{Logout: a.logoutHook(h, &data.Logout), Keyring: func(ctx context.Context, stateDir string) error {
		return service.DeleteSealingKey(ctx, stateDir, a.Keyring)
	}}
	if err := install.Apply(ctx, h, steps, hooks); err != nil {
		if len(data.Logout) > 0 {
			// A later step failed: what Proton answered still matters.
			return Finish(cmd, data, uninstallLogoutText(data.Logout), applyFailed(err))
		}
		return Finish(cmd, nil, "", applyFailed(err))
	}
	text := appliedText(steps, nil)
	if len(data.Logout) > 0 {
		text += "\n" + uninstallLogoutText(data.Logout)
	}
	return Finish(cmd, data, text, nil)
}

// uninstallDesktop is pcb uninstall --delete-data with no service
// installed (pcb-ui; live 2026-11-13, F10): the directories this invocation
// resolves, the state's keyring item and pcb-ui's autostart entry, with the
// plan and the confirmation of a service's. A daemon serving the state is
// refused (exit 20, named); serve.lock is held meanwhile so none starts.
func (a *app) uninstallDesktop(cmd *cobra.Command, yes, plan bool) error {
	h := a.Host
	dirs, err := resolveDirs(a.Env)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	steps, err := install.PlanDeleteDesktop(h, dirs, autostartEntry(a.Env), defaultState(a.Env) == dirs.State)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	const note = "pcb is not installed as a service: this deletes the directories of this session's pcb (pcb-ui)"
	data := installData{Plan: plan, Steps: steps, Next: []string{}}
	// serve.lock is probed or taken only in a state the plan removes: a
	// kept directory gets no file of pcb's.
	removed := install.StateRemoved(steps, dirs.State)
	if plan {
		data.Next = []string{note}
		if removed {
			if err := service.ServeHeld(h.Path(dirs.State)); err != nil {
				data.Next = append(data.Next, servingNote(err))
			}
		}
		return Finish(cmd, data, planText(steps, data.Next), nil)
	}
	hooks := a.desktopHooks(&data)
	if state := h.Path(dirs.State); removed && dirExists(state) {
		release, err := service.HoldServe(state, "uninstall")
		if err != nil {
			return Finish(cmd, nil, "", err)
		}
		release = sync.OnceFunc(release)
		defer release()
		hooks.HeldDir, hooks.HeldFile, hooks.ReleaseServe = state, "serve.lock", release
	}
	if d := deletionOf(steps); d.any() && !yes {
		if err := a.confirmDelete(cmd, d); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	return a.applyUninstall(cmd, steps, data, hooks)
}

// desktopHooks are the hooks of an uninstall with no service: the Proton
// logout and the keyring item (Linux and Windows alike).
func (a *app) desktopHooks(data *installData) install.Hooks {
	return install.Hooks{Logout: a.logoutHook(a.Host, &data.Logout), Keyring: func(ctx context.Context, stateDir string) error {
		return service.DeleteSealingKey(ctx, stateDir, a.Keyring)
	}}
}

// applyUninstall runs steps and prints their result and the logout's.
func (a *app) applyUninstall(cmd *cobra.Command, steps []install.Step, data installData, hooks install.Hooks) error {
	if err := install.Apply(cmd.Context(), a.Host, steps, hooks); err != nil {
		if len(data.Logout) > 0 {
			return Finish(cmd, data, uninstallLogoutText(data.Logout), applyFailed(err))
		}
		return Finish(cmd, nil, "", applyFailed(err))
	}
	text := appliedText(steps, nil)
	if len(data.Logout) > 0 {
		text += "\n" + uninstallLogoutText(data.Logout)
	}
	return Finish(cmd, data, text, nil)
}

// uninstallWindows is pcb uninstall on Windows, where there is no service:
// it deletes pcb-ui's Run value and, with --delete-data, logs out and
// deletes the directories this invocation resolves. It refuses (exit 20,
// naming the holder) while pcb-ui, pcb serve or an agent session serves the
// state; with the state to delete it holds serve.lock meanwhile and
// releases it last (Windows cannot remove a locked file).
func (a *app) uninstallWindows(cmd *cobra.Command, deleteData, yes, plan bool) error {
	h := a.Host
	dirs, err := resolveDirs(a.Env)
	if err != nil {
		return Finish(cmd, nil, "", err)
	}
	state := h.Path(dirs.State)
	held := dirExists(state)
	if held && !plan {
		if err := service.ServeHeld(state); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	ours := defaultState(a.Env) == dirs.State
	var steps []install.Step
	if deleteData {
		if steps, err = install.PlanDeleteDesktop(h, dirs, "", ours); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	data0, present, err := a.RunKey.Get()
	if err != nil {
		return Finish(cmd, nil, "", fmt.Errorf("reading pcb-ui's Run value: %w", err))
	}
	steps = append(steps, install.AutostartStep(present, a.runValueNotOurs(data0, ours)))
	data := installData{Plan: plan, Steps: steps, Next: []string{}}
	if plan {
		if held {
			if err := service.ServeHeld(state); err != nil {
				data.Next = append(data.Next, servingNote(err))
			}
		}
		return Finish(cmd, data, planText(steps, data.Next), nil)
	}
	hooks := a.desktopHooks(&data)
	hooks.Autostart = a.RunKey.Delete
	if deleteData && install.StateRemoved(steps, dirs.State) && held {
		release, err := service.HoldServe(state, "uninstall")
		if err != nil {
			return Finish(cmd, nil, "", err)
		}
		release = sync.OnceFunc(release)
		defer release()
		hooks.HeldDir, hooks.HeldFile, hooks.ReleaseServe = state, "serve.lock", release
	}
	if d := deletionOf(steps); deleteData && d.any() && !yes {
		if err := a.confirmDelete(cmd, d); err != nil {
			return Finish(cmd, nil, "", err)
		}
	}
	return a.applyUninstall(cmd, steps, data, hooks)
}

// runValueNotOurs is why pcb-ui's Run value is not this invocation's to
// delete, "" when it is: it carries no environment, so it starts pcb-ui on
// the default directories (ours false: other ones here), and it names one
// executable (another copy's entry is kept).
func (a *app) runValueNotOurs(data string, ours bool) string {
	if !ours {
		return "starts pcb-ui on the default directories, not these"
	}
	exe, err := a.Host.Executable()
	if err != nil {
		return ""
	}
	want := filepath.Join(filepath.Dir(exe), "pcb-ui.exe")
	if got, ok := runkey.Exe(data); ok && !strings.EqualFold(filepath.Clean(got), filepath.Clean(want)) {
		return "starts " + got + ", not the pcb-ui.exe beside this pcb (" + want + ")"
	}
	return ""
}

// defaultState is the state directory pcb resolves for env without
// PCB_*_DIR: the one pcb-ui's autostart entry starts (it sets no
// environment); "" when none resolves.
func defaultState(env func(string) string) string {
	d, err := paths.Resolve(func(k string) string {
		if k == paths.EnvConfigDir || k == paths.EnvStateDir {
			return ""
		}
		return env(k)
	})
	if err != nil {
		return ""
	}
	return d.State
}

// servingNote is a plan's note for the refusal the run would get: the
// daemon serving the state, and how to stop it.
func servingNote(err error) string {
	var ee *exitcode.Error
	if errors.As(err, &ee) && ee.Hint != "" {
		return ee.Err.Error() + ": " + ee.Hint
	}
	return err.Error()
}

// autostartEntry is pcb-ui's autostart entry for env:
// $XDG_CONFIG_HOME/autostart/pcb-ui.desktop, ~/.config by default (as
// pcb-ui writes it); "" when neither resolves.
func autostartEntry(env func(string) string) string {
	dir := env("XDG_CONFIG_HOME")
	if !filepath.IsAbs(dir) {
		home := env("HOME")
		if !filepath.IsAbs(home) {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "pcb-ui.desktop")
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// uninstallLogout is what the uninstall's logout step got from Proton for
// one account, as pcb logout prints it.
type uninstallLogout struct {
	Account string `json:"account"`
	Address string `json:"address"`
	service.LogoutResult
}

// uninstallLogoutText is pcb logout's first line for each answer, after
// the account's name when there are several: its advice to run pcb logout
// again after a sync makes no sense once the directories are deleted.
func uninstallLogoutText(answers []uninstallLogout) string {
	lines := make([]string, 0, len(answers))
	for _, l := range answers {
		first, _, _ := strings.Cut(logoutText(l.LogoutResult), "\n")
		if len(answers) > 1 {
			first = accountName(service.AccountInfo{ID: l.Account, Address: l.Address}) + ": " + first
		}
		lines = append(lines, first)
	}
	return strings.Join(lines, "\n")
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
	dirs      []string
	logout    string // the state directory whose session is revoked
	user      bool
	keyring   bool // the state's "pcb sealing key" item
	autostart bool // pcb-ui's autostart entry
}

func deletionOf(steps []install.Step) deletion {
	d := deletion{dirs: install.RemovedDirs(steps)}
	for _, st := range steps {
		switch {
		case st.Action == install.ActionRun && st.Kind == install.KindLogout:
			d.logout = st.Path
		case st.Action == install.ActionRun && st.Kind == install.KindUser && strings.HasPrefix(st.Command, "userdel "):
			d.user = true
		case st.Action == install.ActionRemove && st.Kind == install.KindKeyring:
			d.keyring = true
		case st.Action == install.ActionRemove && (st.Kind == install.KindFile || st.Kind == install.KindAutostart) && st.Note == install.AutostartNote:
			d.autostart = true
		}
	}
	return d
}

func (d deletion) any() bool {
	return len(d.dirs) > 0 || d.logout != "" || d.user || d.keyring || d.autostart
}

// keyringItem and autostartEntryName name the deletion's other things.
const (
	keyringItem        = `the "pcb sealing key" item in the desktop keyring`
	autostartEntryName = "pcb-ui's autostart entry"
)

// extras is what d deletes besides directories, a session and a user.
func (d deletion) extras() []string {
	var x []string
	if d.keyring {
		x = append(x, keyringItem)
	}
	if d.autostart {
		x = append(x, autostartEntryName)
	}
	return x
}

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
	case d.user:
		q = "Delete the user " + install.SystemUser
	}
	if x := d.extras(); len(x) > 0 {
		if q == "" {
			q = "Delete " + strings.Join(x, " and ")
		} else {
			q += ", " + strings.Join(x, " and ")
		}
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
	for _, x := range d.extras() {
		what = append(what, "deletes "+x)
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

// logoutHook signs out every account of the directories the plan
// resolved (design §4.6), with the in-process accounts, never the caller's
// own (M5-P19), and appends each account's answer to *out. Each account is
// tried; only a failure to get an answer is an error (the first one, named
// after its account when there are several).
func (a *app) logoutHook(h install.Host, out *[]uninstallLogout) func(context.Context, paths.Dirs) error {
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
		hub, err := a.OpenHub(withLogger(ctx, newNoteLogger(a.Stderr)), env)
		if err != nil {
			return err
		}
		defer a.closeHub(hub)
		var failure error
		accounts := hub.Accounts()
		for _, info := range accounts {
			res, err := hub.SignOut(ctx, info.ID, service.LogoutRequest{})
			if err != nil {
				if failure == nil {
					failure = err
					if len(accounts) > 1 {
						failure = fmt.Errorf("%s: %w", accountName(info), err)
					}
				}
				continue
			}
			*out = append(*out, uninstallLogout{Account: info.ID, Address: info.Address, LogoutResult: res})
		}
		// Not revoked is an answer: the outcome line says so, in pcb
		// logout's words, not a note of the step.
		return failure
	}
}
