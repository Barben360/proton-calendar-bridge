//go:build windows

package paths

import (
	"errors"
	"path/filepath"
)

func resolve(env func(string) string) (Dirs, error) {
	config, err := dir(env, EnvConfigDir, "APPDATA")
	if err != nil {
		return Dirs{}, err
	}
	state, err := dir(env, EnvStateDir, "LOCALAPPDATA")
	if err != nil {
		return Dirs{}, err
	}
	return Dirs{Config: config, State: state}, nil
}

// dir is the override when set, else <base>\pcb.
func dir(env func(string) string, override, base string) (string, error) {
	if v := env(override); v != "" {
		if !filepath.IsAbs(v) {
			return "", errors.New(override + " must be an absolute path")
		}
		return filepath.Clean(v), nil
	}
	if b := env(base); filepath.IsAbs(b) {
		return filepath.Join(b, "pcb"), nil
	}
	return "", errors.New("cannot locate the user directories: set " + override + " or " + base + " to an absolute path")
}
