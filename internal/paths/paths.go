// Package paths resolves where pcb keeps its files (SPEC §7.1).
package paths

import "path/filepath"

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

// Socket is the control socket of pcb serve, in the state directory. On
// Windows there is no socket file: the directory names the pipe.
func (d Dirs) Socket() string { return filepath.Join(d.State, "pcb.sock") }

// Resolve returns the config and state directories.
// PCB_CONFIG_DIR and PCB_STATE_DIR, when set, are used as they are and must
// be absolute. Otherwise, on Unix they are $XDG_CONFIG_HOME/pcb and
// $XDG_STATE_HOME/pcb, or ~/.config/pcb and ~/.local/state/pcb (a relative
// XDG variable is ignored); on Windows they are %APPDATA%\pcb and
// %LOCALAPPDATA%\pcb. Resolve fails when no absolute base is found.
// env looks up an environment variable (os.Getenv in production).
func Resolve(env func(string) string) (Dirs, error) { return resolve(env) }
