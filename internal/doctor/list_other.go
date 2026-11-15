//go:build !windows

package doctor

import (
	"errors"
	"path/filepath"

	"github.com/Barben360/proton-calendar-bridge/internal/install"
)

// The bases the user directories resolve from (paths.Resolve), named in
// the fix when they cannot be.
const (
	configBase = "XDG_CONFIG_HOME"
	stateBase  = "XDG_STATE_HOME"
)

// dirFix is the fix for a user directory that cannot be located.
func dirFix(override, base string) string {
	return "set HOME, " + override + " or " + base + " to an absolute path"
}

// tailChecks are the checks of this OS that close the list: pcb-ui
// installed beside the pcb service ([mcp] timezone is Windows').
func tailChecks(env func(string) string, host install.Host, _ string) []Check {
	return []Check{DesktopCheck{AppDirs: desktopDirs(env, host), ServiceInstalled: func() bool { return serviceInstalled(host) }}}
}

// serviceInstalled reports whether a pcb systemd unit is installed, user or
// system scope (an ambiguous install counts: both are units).
func serviceInstalled(host install.Host) bool {
	_, _, err := install.Detect(host)
	return err == nil || errors.Is(err, install.ErrAmbiguous)
}

// desktopDirs are the applications directories pcb-ui.desktop is looked
// for in: $XDG_DATA_HOME/applications (default ~/.local/share/applications),
// then the two system ones. A custom PREFIX install is not found.
func desktopDirs(env func(string) string, host install.Host) []string {
	var dirs []string
	data := env("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		if home := env("HOME"); filepath.IsAbs(home) {
			data = filepath.Join(home, ".local", "share")
		} else {
			data = ""
		}
	}
	if data != "" {
		dirs = append(dirs, filepath.Join(data, "applications"))
	}
	return append(dirs, host.Path("/usr/local/share/applications"), host.Path("/usr/share/applications"))
}
