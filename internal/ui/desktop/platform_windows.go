package desktop

import "github.com/Barben360/proton-calendar-bridge/internal/runkey"

// runKey is the Run value "Start at login" writes; a test swaps in a
// subkey of its own.
var runKey = runkey.System

// SetAutostart writes the Run value pcb-ui ("<exe>" --background) under
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run, or deletes it.
func SetAutostart(on bool, exe string) error {
	if on {
		return runKey().Set(runkey.Data(exe))
	}
	return runKey().Delete()
}

// Autostart reports whether the Run value pcb-ui is there.
func Autostart() bool {
	_, ok, err := runKey().Get()
	return ok && err == nil
}
