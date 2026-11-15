package update

import (
	"os"
	"path"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// installLine is install.sh's one-line install of the latest stable release.
const installLine = "curl -fsSL https://github.com/Barben360/proton-calendar-bridge/releases/latest/download/install.sh | sh"

// containerHint is how the image is updated (PCB_INSTALL=container).
const containerHint = "pull the new image: docker compose pull && docker compose up -d"

// Install is what Hint looks at: the system, the running binary (absolute,
// symlinks resolved), the home directory, the environment, and Exists (nil:
// os.Stat) for the files install.sh puts beside it.
type Install struct {
	GOOS, Exe, Home string
	Env             func(string) string
	Exists          func(string) bool
}

// Hint is how this install is updated, shown after the release page: in the
// image, pulling it; on Linux and macOS, install.sh's line with the flags
// that rebuild what is installed (--prefix off ~/.local, --no-ui without a
// pcb-ui beside pcb, --no-desktop without its desktop entry; macOS gets pcb
// only whatever the flags). Nothing on Windows, nor for a binary install.sh
// did not put in a bin directory: the release page is the hint there.
func Hint(in Install) string {
	if in.Env != nil && in.Env("PCB_INSTALL") == "container" {
		return containerHint
	}
	if in.GOOS != "linux" && in.GOOS != "darwin" {
		return ""
	}
	bin := path.Dir(in.Exe)
	if in.Exe == "" || path.Base(bin) != "bin" {
		return ""
	}
	exists := in.Exists
	if exists == nil {
		exists = func(p string) bool { _, err := os.Stat(p); return err == nil }
	}
	prefix := path.Dir(bin)
	var flags []string
	if prefix != path.Join(in.Home, ".local") {
		flags = append(flags, "--prefix", shellquote.POSIX(prefix))
	}
	if in.GOOS == "linux" {
		switch {
		case !exists(path.Join(bin, "pcb-ui")):
			flags = append(flags, "--no-ui")
		case !exists(path.Join(prefix, "share", "applications", "pcb-ui.desktop")):
			flags = append(flags, "--no-desktop")
		}
	}
	if len(flags) == 0 {
		return installLine
	}
	return installLine + " -s -- " + strings.Join(flags, " ")
}
