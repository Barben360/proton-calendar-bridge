package install

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

// Scope is where pcb runs as a service: the caller's user manager or the
// system manager.
type Scope string

// The scopes.
const (
	ScopeUser   Scope = "user"
	ScopeSystem Scope = "system"
)

// What a --system install uses (M5-P1, M5-P16).
const (
	UnitName        = "pcb.service"
	SystemUnitPath  = "/etc/systemd/system/" + UnitName
	SystemConfigDir = "/etc/pcb"
	SystemStateDir  = "/var/lib/pcb"
	SystemBinary    = "/usr/local/bin/pcb"
	SystemUser      = "pcb"
)

// systemdDir exists while systemd is the running init (sd_booted).
const systemdDir = "/run/systemd/system"

// Detect's failures.
var (
	ErrAmbiguous    = errors.New("pcb is installed both as a user service and as a system service")
	ErrNotInstalled = errors.New("pcb is not installed as a service")
)

// UserUnitDir is where the user manager reads units:
// $XDG_CONFIG_HOME/systemd/user, else ~/.config/systemd/user.
func UserUnitDir(h Host) (string, error) {
	base := h.Env("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		home := h.Env("HOME")
		if !filepath.IsAbs(home) {
			return "", errors.New("cannot locate the systemd user directory: set HOME or XDG_CONFIG_HOME to an absolute path")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user"), nil
}

// UnitPath is where scope's unit lies.
func UnitPath(h Host, s Scope) (string, error) {
	if s == ScopeSystem {
		return SystemUnitPath, nil
	}
	dir, err := UserUnitDir(h)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, UnitName), nil
}

// Detect finds the installed unit: ErrAmbiguous when both exist,
// ErrNotInstalled when neither does.
func Detect(h Host) (Scope, string, error) {
	system := exists(h, SystemUnitPath)
	userPath, err := UnitPath(h, ScopeUser)
	user := err == nil && exists(h, userPath)
	switch {
	case system && user:
		return "", "", ErrAmbiguous
	case system:
		return ScopeSystem, SystemUnitPath, nil
	case user:
		return ScopeUser, userPath, nil
	}
	return "", "", ErrNotInstalled
}

// SystemdRunning reports whether systemd is the init of h (a container
// usually has none).
func SystemdRunning(h Host) bool {
	fi, err := os.Stat(h.Path(systemdDir))
	return err == nil && fi.IsDir()
}

func exists(h Host, p string) bool {
	_, err := os.Lstat(h.Path(p))
	return err == nil
}

// Systemctl is the systemctl command line for scope.
func Systemctl(s Scope, args ...string) []string {
	argv := []string{"systemctl"}
	if s == ScopeUser {
		argv = append(argv, "--user")
	}
	return append(argv, args...)
}

// Journalctl is the journalctl command line showing the unit's last 200
// lines, then following it when follow is set.
func Journalctl(s Scope, follow bool) []string {
	argv := []string{"journalctl"}
	if s == ScopeUser {
		argv = append(argv, "--user")
	}
	argv = append(argv, "-u", UnitName, "--no-pager", "-n", "200")
	if follow {
		argv = append(argv, "-f")
	}
	return argv
}

// ScopeDirs are the config and state directories scope's service uses:
// /etc/pcb and /var/lib/pcb for the system; for the user, those the
// installed unit names (M5-P18), else those paths.Resolve gives.
func ScopeDirs(h Host, s Scope) (paths.Dirs, error) {
	if s == ScopeSystem {
		return paths.Dirs{Config: SystemConfigDir, State: SystemStateDir}, nil
	}
	if unit, err := UnitPath(h, s); err == nil {
		if data, err := os.ReadFile(h.Path(unit)); err == nil {
			env := unitEnv(data)
			if c, st := env[paths.EnvConfigDir], env[paths.EnvStateDir]; filepath.IsAbs(c) && filepath.IsAbs(st) {
				return paths.Dirs{Config: c, State: st}, nil
			}
		}
	}
	return paths.Resolve(h.Env)
}

// unitEnv reads a unit's Environment=NAME=VALUE lines.
func unitEnv(unit []byte) map[string]string {
	env := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(unit))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "Environment="); ok {
			if name, value, ok := strings.Cut(v, "="); ok {
				env[name] = value
			}
		}
	}
	return env
}

// UnitStatus is the unit's state as systemctl show reports it.
type UnitStatus struct {
	Scope         Scope  `json:"scope"`
	Unit          string `json:"unit"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	UnitFileState string `json:"unit_file_state"`
	MainPID       int    `json:"main_pid"`
}

// Show asks systemctl for the unit's state.
func Show(ctx context.Context, h Host, s Scope) (UnitStatus, error) {
	unit, err := UnitPath(h, s)
	if err != nil {
		return UnitStatus{}, err
	}
	argv := Systemctl(s, "show", UnitName, "--property=ActiveState,SubState,UnitFileState,MainPID")
	out, code, err := h.Run.Output(ctx, argv[0], argv[1:]...)
	switch {
	case err != nil:
		return UnitStatus{}, fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	case code != 0:
		return UnitStatus{}, fmt.Errorf("%s exited with %d", strings.Join(argv, " "), code)
	}
	st := UnitStatus{Scope: s, Unit: unit}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		name, value, _ := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		switch name {
		case "ActiveState":
			st.ActiveState = value
		case "SubState":
			st.SubState = value
		case "UnitFileState":
			st.UnitFileState = value
		case "MainPID":
			if st.MainPID, err = strconv.Atoi(value); err != nil {
				return UnitStatus{}, fmt.Errorf("systemctl show gave MainPID %q", value)
			}
		}
	}
	if st.ActiveState == "" {
		return UnitStatus{}, errors.New("systemctl show gave no ActiveState")
	}
	return st, nil
}
