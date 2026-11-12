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

// Resolve returns the user directories, unless PCB_CONFIG_DIR or
// PCB_STATE_DIR overrides one (used verbatim, and refused when relative): $XDG_CONFIG_HOME/pcb and
// $XDG_STATE_HOME/pcb, falling back to ~/.config/pcb and ~/.local/state/pcb.
// Per the XDG spec a relative XDG variable is ignored; the result is always
// absolute, or Resolve fails when neither XDG nor HOME gives an absolute base.
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
	return xdg(env, xdgName, fallback)
}

func xdg(env func(string) string, name, fallback string) (string, error) {
	if base := env(name); filepath.IsAbs(base) {
		return filepath.Join(base, "pcb"), nil
	}
	if home := env("HOME"); filepath.IsAbs(home) {
		return filepath.Join(home, fallback, "pcb"), nil
	}
	return "", errors.New("cannot locate the user directories: set HOME or " + name + " to an absolute path")
}
