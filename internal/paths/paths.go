// Package paths resolves where pcb keeps its files (SPEC §7.1).
package paths

import "path/filepath"

// Dirs are the per-user directories of pcb.
type Dirs struct {
	Config string // settings
	State  string // state, caches and logs
}

// Resolve returns the user directories: $XDG_CONFIG_HOME/pcb and
// $XDG_STATE_HOME/pcb, falling back to ~/.config/pcb and ~/.local/state/pcb.
// env looks up an environment variable (os.Getenv in production).
func Resolve(env func(string) string) Dirs {
	return Dirs{
		Config: xdg(env, "XDG_CONFIG_HOME", ".config"),
		State:  xdg(env, "XDG_STATE_HOME", filepath.Join(".local", "state")),
	}
}

func xdg(env func(string) string, name, fallback string) string {
	base := env(name)
	if base == "" {
		base = filepath.Join(env("HOME"), fallback)
	}
	return filepath.Join(base, "pcb")
}
