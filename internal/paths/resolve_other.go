//go:build !windows

package paths

import (
	"errors"
	"path/filepath"
)

func resolve(env func(string) string) (Dirs, error) {
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
