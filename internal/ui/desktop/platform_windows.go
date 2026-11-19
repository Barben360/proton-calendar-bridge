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

// Autostart reports whether the Run value pcb-ui is there and starts exe
// (this executable): a stale value, after a move or a switch from the zip
// to the MSI, is not "on".
func Autostart(exe string) bool {
	data, ok, err := runKey().Get()
	return ok && err == nil && runkey.PointsTo(data, exe)
}
