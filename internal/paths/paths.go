// Package paths resolves where pcb keeps its files (SPEC §7.1).
package paths

import (
	"errors"
	"path/filepath"
)

// Dirs are the per-user directories of pcb.
type Dirs struct {
	Config string // settings
	State  string // state, caches and logs
}

// Resolve returns the user directories: $XDG_CONFIG_HOME/pcb and
// $XDG_STATE_HOME/pcb, falling back to ~/.config/pcb and ~/.local/state/pcb.
// Per the XDG spec a relative XDG variable is ignored; the result is always
// absolute, or Resolve fails when neither XDG nor HOME gives an absolute base.
// env looks up an environment variable (os.Getenv in production).
func Resolve(env func(string) string) (Dirs, error) {
	config, err := xdg(env, "XDG_CONFIG_HOME", ".config")
	if err != nil {
		return Dirs{}, err
	}
	state, err := xdg(env, "XDG_STATE_HOME", filepath.Join(".local", "state"))
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{Config: config, State: state}, nil
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
