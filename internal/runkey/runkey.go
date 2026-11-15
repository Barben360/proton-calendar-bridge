// Package runkey is pcb-ui's autostart entry on Windows: the value pcb-ui
// under HKCU\Software\Microsoft\Windows\CurrentVersion\Run, whose data is
// the quoted absolute path of pcb-ui.exe and --background. Switching
// autostart off deletes the value.
package runkey

import "strings"

const (
	// KeyPath is the Run key, under HKEY_CURRENT_USER.
	KeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	// Value is the name of pcb-ui's value in it.
	Value = "pcb-ui"
)

// Data is the value's data for the executable exe: quoted, so a path with
// spaces starts the right program.
func Data(exe string) string { return `"` + exe + `" --background` }

// Exe is the executable path data starts: the text inside the leading
// quotes, else up to the first space. ok is false for empty data, an
// unclosed quote or an empty path.
func Exe(data string) (string, bool) {
	data = strings.TrimSpace(data)
	var exe string
	if rest, quoted := strings.CutPrefix(data, `"`); quoted {
		var closed bool
		if exe, _, closed = strings.Cut(rest, `"`); !closed {
			return "", false
		}
	} else {
		exe, _, _ = strings.Cut(data, " ")
	}
	return exe, exe != ""
}

// Store is one Run value.
type Store interface {
	// Get is the value's data; ok is false (and err nil) when it is absent.
	Get() (data string, ok bool, err error)
	// Set writes the value, creating the key if needed.
	Set(data string) error
	// Delete removes the value; an absent one is not an error.
	Delete() error
}
