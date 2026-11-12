// Package paths resolves where pcb keeps its files (SPEC §7.1).
package paths

import (
	"errors"
	"path/filepath"
)

// Overrides of the directories (M5-P1): used verbatim, without a "/pcb"
// suffix, and absolute. A service sets them (/etc/pcb and /var/lib/pcb, or
// /data/config and /data/state in the container).
const (
	EnvConfigDir = "PCB_CONFIG_DIR"
	EnvStateDir  = "PCB_STATE_DIR"
)

// Dirs are the per-user directories of pcb.
type Dirs struct {
	Config string // settings
	State  string // state, caches and logs
}

// Socket is the control socket of pcb serve, in the state directory.
func (d Dirs) Socket() string { return filepath.Join(d.State, "pcb.sock") }

// Resolve returns the config and state directories.
// PCB_CONFIG_DIR and PCB_STATE_DIR, when set, are used as they are and must
// be absolute. Otherwise they are $XDG_CONFIG_HOME/pcb and
// $XDG_STATE_HOME/pcb, or ~/.config/pcb and ~/.local/state/pcb. A relative
// XDG variable is ignored. Resolve fails when no absolute base is found.
// env looks up an environment variable (os.Getenv in production).
func Resolve(env func(string) string) (Dirs, error) {
	config, err := dir(env, EnvConfigDir, "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Dirs{}, err
	}
	state, err := dir(env, EnvStateDir, "XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{Config: config, State: state}, nil
}

func dir(env func(string) string, override, xdgName, fallback string) (string, error) {
	if v := env(override); v != "" {
		if !filepath.IsAbs(v) {
			return "", errors.New(override + " must be an absolute path")
		}
		return filepath.Clean(v), nil
	}
	return xdg(env, override, xdgName, fallback)
}

func xdg(env func(string) string, override, name, fallback string) (string, error) {
	if base := env(name); filepath.IsAbs(base) {
		return filepath.Join(base, "pcb"), nil
	}
	if home := env("HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, fallback, "pcb"), nil
	}
	return "", errors.New("cannot locate the user directories: set HOME, " + override + " or " + name + " to an absolute path")
}
