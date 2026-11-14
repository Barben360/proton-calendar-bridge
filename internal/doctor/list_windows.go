//go:build windows

package doctor

import (
	"os"
	"path/filepath"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
	"github.com/Barben360/proton-calendar-bridge/internal/winzone"
)

// The bases the user directories resolve from (paths.Resolve), named in
// the fix when they cannot be.
const (
	configBase = "APPDATA"
	stateBase  = "LOCALAPPDATA"
)

// dirFix is the fix for a user directory that cannot be located.
func dirFix(override, base string) string {
	return "set " + override + " or " + base + " to an absolute path"
}

// tailChecks are the checks of Windows that close the list: elevation,
// the time zone and Start at login. Windows has no systemd, desktop entry
// or Secret Service to check. mcpZone is [mcp] timezone.
func tailChecks(_ func(string) string, _ install.Host, mcpZone string) []Check {
	key, keyOK := winzone.Key()
	return []Check{
		ElevatedCheck{Elevated: acl.Elevated()},
		ZoneCheck{Key: key, KeyOK: keyOK, Configured: mcpZone},
		AutostartCheck{Read: runkey.System().Get, Beside: besidePcb("pcb-ui.exe"), Exists: exists},
	}
}

// besidePcb is the path of name in this executable's directory; "" when
// that directory is unknown.
func besidePcb(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Join(filepath.Dir(exe), name)
}
