//go:build !windows

package acltest

import (
	"io/fs"
	"os"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// WantPrivate fails t unless path has exactly mode.
func WantPrivate(t testing.TB, path string, mode fs.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Errorf("%s mode %04o, want %04o", path, got, mode)
	}
}

// Unreadable makes the file path unreadable by this user until t ends:
// mode 0000. Root reads it anyway: the test is skipped then.
func Unreadable(t testing.TB, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0000 file")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, info.Mode().Perm()) }) // the path may be gone or replaced by then
}

// OpenToOthers lets other users read path: mode 0644 (0755 for a
// directory).
func OpenToOthers(t testing.TB, path string) {
	t.Helper()
	mode := fs.FileMode(0o644)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		mode = 0o755
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// OthersFix is the fix pcb doctor gives for OpenToOthers(path) on a file.
func OthersFix(path string) string { return "chmod 600 " + shellquote.Quote(path) }
