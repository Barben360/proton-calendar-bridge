package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

// Step actions.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionKeep   = "keep"
	ActionRun    = "run"
	ActionRemove = "remove"
	ActionSkip   = "skip"
)

// Step kinds.
const (
	KindDir     = "dir"
	KindFile    = "file"
	KindCommand = "command"
	KindUser    = "user"
	KindLogout  = "logout"
)

// Step is one thing an install or uninstall does, or would do. Paths are
// as on the host (without Root).
type Step struct {
	Action  string `json:"action"`
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Command string `json:"command,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Note    string `json:"note,omitempty"`

	content []byte // a file to create or update: what to write
}

// String is the step's line in a plan:
// "<action> <kind> <path|command> [<mode> <owner>] [— note]".
func (s Step) String() string {
	parts := []string{s.Action, s.Kind}
	if s.Path != "" {
		parts = append(parts, s.Path)
	}
	if s.Command != "" {
		parts = append(parts, s.Command)
	}
	if s.Mode != "" {
		parts = append(parts, s.Mode)
	}
	if s.Owner != "" {
		parts = append(parts, s.Owner)
	}
	line := strings.Join(parts, " ")
	if s.Note != "" {
		line += " — " + s.Note
	}
	return line
}

// Request is what to install.
type Request struct {
	Scope Scope
}

// UninstallRequest is what to remove. DeleteData also removes the config
// and state directories, after a logout (M5-P19).
type UninstallRequest struct {
	Scope      Scope
	DeleteData bool
}

// useradd creates the system user of a --system install.
const useradd = "useradd --system --user-group --home-dir " + SystemStateDir + " --no-create-home --shell /usr/sbin/nologin " + SystemUser

// systemOwner owns the system install's directories and config.
const systemOwner = SystemUser + ":" + SystemUser

// Plan lists the steps of an install, comparing what exists with what
// should: running it twice gives only keep steps and reloads nothing. It
// changes nothing.
func Plan(_ context.Context, h Host, req Request) ([]Step, error) {
	if err := checkRoot(h, req.Scope, "install"); err != nil {
		return nil, err
	}
	if req.Scope == ScopeSystem {
		return planSystem(h)
	}
	return planUser(h)
}

// checkRoot refuses a user scope as root and a system scope as anyone else.
func checkRoot(h Host, s Scope, command string) error {
	switch s {
	case ScopeUser:
		if h.Euid() == 0 {
			return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("pcb %s --user refuses to run as root", command),
				Hint: "use --system"}
		}
	case ScopeSystem:
		if h.Euid() != 0 {
			return &exitcode.Error{Code: exitcode.MustAct, Err: fmt.Errorf("pcb %s --system needs root", command),
				Hint: "sudo pcb " + command + " --system"}
		}
	default:
		return &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("unknown scope %q", s)}
	}
	return nil
}

func blocked(err error, hint string) error {
	return &exitcode.Error{Code: exitcode.Blocked, Err: err, Hint: hint}
}

func planUser(h Host) ([]Step, error) {
	dirs, err := paths.Resolve(h.Env)
	if err != nil {
		return nil, blocked(err, "set HOME, or PCB_CONFIG_DIR and PCB_STATE_DIR, to absolute paths")
	}
	unitDir, err := UserUnitDir(h)
	if err != nil {
		return nil, blocked(err, "set HOME to your home directory")
	}
	exe, err := h.Executable()
	if err != nil {
		return nil, blocked(fmt.Errorf("cannot locate the running pcb binary: %w", err), "run pcb from its installed path")
	}
	params := UnitParams{Exec: exe, ConfigDir: dirs.Config, StateDir: dirs.State}
	if err := checkUnitParams(params); err != nil {
		return nil, blocked(err, "use paths made of letters, digits and /._-+@,:=~ (set PCB_CONFIG_DIR and PCB_STATE_DIR, or move the binary)")
	}
	return planFiles(h, []fileSpec{
		{kind: KindDir, path: dirs.Config, mode: 0o700},
		{kind: KindDir, path: dirs.State, mode: 0o700},
		{kind: KindFile, path: config.Path(dirs), mode: 0o600, content: DefaultConfig(), createOnly: true},
		{kind: KindDir, path: unitDir},
		{kind: KindFile, path: filepath.Join(unitDir, UnitName), mode: 0o644, content: UserUnit(params), unit: true},
	}, ScopeUser, nil)
}

func planSystem(h Host) ([]Step, error) {
	exe, err := h.Executable()
	if err != nil {
		return nil, blocked(fmt.Errorf("cannot locate the running pcb binary: %w", err), "run pcb from its installed path")
	}
	bin, err := os.ReadFile(h.Path(exe))
	if err != nil {
		return nil, blocked(fmt.Errorf("cannot read the running pcb binary: %w", err), "run pcb from its installed path")
	}
	_, _, ok, err := h.LookupUser(SystemUser)
	if err != nil {
		return nil, blocked(fmt.Errorf("look up the user %s: %w", SystemUser, err), "")
	}
	first := Step{Action: ActionRun, Kind: KindUser, Command: useradd}
	if ok {
		first = Step{Action: ActionSkip, Kind: KindUser, Command: useradd, Note: "the user " + SystemUser + " exists"}
	}
	params := UnitParams{Exec: SystemBinary, ConfigDir: SystemConfigDir, StateDir: SystemStateDir}
	return planFiles(h, []fileSpec{
		{kind: KindDir, path: SystemConfigDir, mode: 0o700, owner: systemOwner},
		{kind: KindDir, path: SystemStateDir, mode: 0o700, owner: systemOwner},
		{kind: KindFile, path: filepath.Join(SystemConfigDir, "config.toml"), mode: 0o600, owner: systemOwner, content: DefaultConfig(), createOnly: true},
		{kind: KindFile, path: SystemBinary, mode: 0o755, owner: "root", content: bin, restart: true},
		{kind: KindFile, path: SystemUnitPath, mode: 0o644, content: SystemUnit(params), unit: true},
	}, ScopeSystem, []Step{first})
}

// fileSpec is a directory or file an install wants.
type fileSpec struct {
	kind       string
	path       string
	mode       fs.FileMode // 0: not managed
	owner      string      // "": not managed
	content    []byte
	createOnly bool // never updated once it exists (config.toml)
	unit       bool // the unit: daemon-reload when created or updated
	restart    bool // the service runs it: restart when updated
}

// planFiles compares each spec with the host, then adds the systemctl
// steps: daemon-reload only when the unit was created or updated, enable
// --now always, restart only when the unit (or the system binary it runs)
// was updated: a running service keeps the old one until then.
func planFiles(h Host, specs []fileSpec, s Scope, steps []Step) ([]Step, error) {
	reloads, restarts := false, false
	for _, sp := range specs {
		st, err := planFile(h, sp)
		if err != nil {
			return nil, err
		}
		reloads = reloads || sp.unit && st.Action != ActionKeep
		restarts = restarts || (sp.unit || sp.restart) && st.Action == ActionUpdate
		steps = append(steps, st)
	}
	reload := Step{Action: ActionRun, Kind: KindCommand, Command: strings.Join(Systemctl(s, "daemon-reload"), " ")}
	if !reloads {
		reload.Action, reload.Note = ActionSkip, "the unit is unchanged"
	}
	enable := Step{Action: ActionRun, Kind: KindCommand, Command: strings.Join(Systemctl(s, "enable", "--now", UnitName), " ")}
	restart := Step{Action: ActionRun, Kind: KindCommand, Command: strings.Join(Systemctl(s, "restart", UnitName), " ")}
	if !restarts {
		restart.Action, restart.Note = ActionSkip, "the unit was not updated"
		if s == ScopeSystem {
			restart.Note = "neither the unit nor the binary was updated"
		}
	}
	return append(steps, reload, enable, restart), nil
}

func planFile(h Host, sp fileSpec) (Step, error) {
	st := Step{Action: ActionCreate, Kind: sp.kind, Path: sp.path, Owner: sp.owner, content: sp.content}
	if sp.mode != 0 {
		st.Mode = fmt.Sprintf("%04o", sp.mode)
	}
	p := h.Path(sp.path)
	fi, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, nil
	case err != nil:
		return Step{}, blocked(err, "")
	case sp.createOnly:
		st.Action, st.Note = ActionKeep, "an existing config is never overwritten"
		return st, nil
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		if fi, err = os.Stat(p); err != nil {
			return Step{}, blocked(err, "")
		}
	}
	same := sp.mode == 0 || fi.Mode().Perm() == sp.mode
	switch sp.kind {
	case KindDir:
		if !fi.IsDir() {
			return Step{}, blocked(fmt.Errorf("%s exists and is not a directory", sp.path), "move it away, then run the command again")
		}
	default:
		if !fi.Mode().IsRegular() {
			return Step{}, blocked(fmt.Errorf("%s exists and is not a regular file", sp.path), "move it away, then run the command again")
		}
		cur, err := os.ReadFile(p)
		if err != nil {
			return Step{}, blocked(err, "")
		}
		same = same && bytes.Equal(cur, sp.content)
	}
	owned, err := ownedBy(h, fi, sp.owner)
	if err != nil {
		return Step{}, err
	}
	st.Action = ActionUpdate
	if same && owned {
		st.Action = ActionKeep
		st.content = nil
	}
	return st, nil
}

// ownedBy reports whether fi belongs to owner ("user" or "user:group",
// the group being the user's own). Off Unix ownership is not checked.
func ownedBy(h Host, fi fs.FileInfo, owner string) (bool, error) {
	if owner == "" {
		return true, nil
	}
	fuid, fgid, known := fileOwner(fi)
	if !known {
		return true, nil
	}
	name, _, _ := strings.Cut(owner, ":")
	uid, gid, ok, err := h.LookupUser(name)
	if err != nil {
		return false, blocked(fmt.Errorf("look up the user %s: %w", name, err), "")
	}
	return ok && uid == fuid && gid == fgid, nil
}

// PlanUninstall lists the steps that remove the service (M5-P19): disable
// and stop it, remove the unit, reload; then, with DeleteData, log out and
// delete the config and state directories (and the pcb user of a system
// install). It changes nothing.
func PlanUninstall(_ context.Context, h Host, req UninstallRequest) ([]Step, error) {
	if err := checkRoot(h, req.Scope, "uninstall"); err != nil {
		return nil, err
	}
	unit, err := UnitPath(h, req.Scope)
	if err != nil {
		return nil, blocked(err, "set HOME to your home directory")
	}
	dirs, err := ScopeDirs(h, req.Scope)
	if err != nil {
		return nil, blocked(err, "set HOME, or PCB_CONFIG_DIR and PCB_STATE_DIR, to absolute paths")
	}
	installed := exists(h, unit)
	steps := []Step{
		{Action: ActionRun, Kind: KindCommand, Command: strings.Join(Systemctl(req.Scope, "disable", "--now", UnitName), " ")},
		{Action: ActionRemove, Kind: KindFile, Path: unit},
		{Action: ActionRun, Kind: KindCommand, Command: strings.Join(Systemctl(req.Scope, "daemon-reload"), " ")},
	}
	if !installed {
		for i := range steps {
			steps[i].Action, steps[i].Note = ActionSkip, "no unit"
		}
	}
	if !req.DeleteData {
		for _, d := range []string{dirs.Config, dirs.State} {
			if exists(h, d) {
				steps = append(steps, Step{Action: ActionKeep, Kind: KindDir, Path: d, Note: "kept: pcb uninstall --delete-data removes it"})
			}
		}
	} else {
		more, err := planDeleteData(h, req.Scope, dirs)
		if err != nil {
			return nil, err
		}
		steps = append(steps, more...)
	}
	if req.Scope == ScopeSystem && exists(h, SystemBinary) {
		steps = append(steps, Step{Action: ActionKeep, Kind: KindFile, Path: SystemBinary, Note: "kept " + SystemBinary + ": rm it if unused"})
	}
	return steps, nil
}

func planDeleteData(h Host, s Scope, dirs paths.Dirs) ([]Step, error) {
	logout := Step{Action: ActionRun, Kind: KindLogout, Path: dirs.State, Note: "revoke the Proton session, best effort"}
	if !exists(h, dirs.State) {
		logout.Action, logout.Note = ActionSkip, "no state directory"
	}
	steps := []Step{logout}
	keyFile, readable := configuredKeyFile(h, dirs)
	for _, d := range []string{dirs.Config, dirs.State} {
		st := Step{Action: ActionRemove, Kind: KindDir, Path: d}
		switch {
		case !exists(h, d):
			st.Action, st.Note = ActionSkip, "absent"
		case !strings.Contains(filepath.Base(d), "pcb"):
			// PCB_*_DIR are used verbatim (M5-P1): never wipe a directory
			// that may hold more than pcb's files.
			st.Action, st.Note = ActionKeep, "not removed: its name does not say pcb, so it may hold other files; delete pcb's files yourself"
		case d == dirs.Config && !readable:
			st.Note = "config.toml is unreadable: a key_file it names elsewhere is not removed"
		}
		steps = append(steps, st)
	}
	if keyFile != "" && !within(keyFile, dirs.Config) && !within(keyFile, dirs.State) && exists(h, keyFile) {
		steps = append(steps, Step{Action: ActionKeep, Kind: KindFile, Path: keyFile,
			Note: "the key_file lies outside the config directory: kept, delete it if unused"})
	}
	if s == ScopeSystem {
		_, _, ok, err := h.LookupUser(SystemUser)
		if err != nil {
			return nil, blocked(fmt.Errorf("look up the user %s: %w", SystemUser, err), "")
		}
		del := Step{Action: ActionRun, Kind: KindUser, Command: "userdel " + SystemUser}
		if !ok {
			del.Action, del.Note = ActionSkip, "no user "+SystemUser
		}
		steps = append(steps, del)
	}
	return steps, nil
}

// configuredKeyFile is the key_file of the scope's config.toml (a host
// path); readable is false when config.toml cannot be loaded.
func configuredKeyFile(h Host, dirs paths.Dirs) (keyFile string, readable bool) {
	cfg, err := config.Load(h.Path(config.Path(dirs)), dirs)
	if err != nil {
		return "", false
	}
	return filepath.Clean(cfg.Secrets.KeyFile), true
}

// within reports whether p is dir or lies under it.
func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Next lists what to run after an install: enabling linger (when loginctl
// does not say it is on), then login and the first CalDAV client.
func Next(ctx context.Context, h Host, s Scope) []string {
	if s == ScopeSystem {
		run := "sudo -u " + SystemUser + " env PCB_CONFIG_DIR=" + SystemConfigDir + " PCB_STATE_DIR=" + SystemStateDir + " " + SystemBinary
		return []string{run + " login", run + " client add NAME"}
	}
	var next []string
	name := h.Env("USER")
	if name == "" {
		name = h.Env("LOGNAME")
	}
	if name == "" {
		name = strconv.Itoa(h.Euid())
	}
	out, code, err := h.Run.Output(ctx, "loginctl", "show-user", name, "-p", "Linger")
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != "Linger=yes" {
		next = append(next, "loginctl enable-linger "+name)
	}
	return append(next, "pcb login", "pcb client add NAME")
}
