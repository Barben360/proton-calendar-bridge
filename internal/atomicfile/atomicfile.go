// Package atomicfile writes files so that a reader never sees a partial one.
package atomicfile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Write replaces path with data: a temporary file in the same directory
// (mode 0600), fsynced, then renamed over path. The directory is fsynced
// best effort. A failure leaves no temporary file behind.
func Write(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) //nolint:errcheck // gone after a successful rename
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	SyncDir(filepath.Dir(path))
	return nil
}

// SyncDir fsyncs the directory dir, best effort: after a rename or a
// removal in it, so the change survives a power loss.
func SyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// StaleAfter is the age past which RemoveStale takes a temporary file for
// one a crash left: a write holds its file for milliseconds.
const StaleAfter = time.Hour

// The prefixes of the temporary files pcb creates next to the files it
// replaces: Write's, and the key file's creation (seal.KeyFile, a raw copy
// of the key).
const (
	TempPrefix = ".tmp-"
	KeyPrefix  = ".key-"
)

// RemoveStale removes from dir the regular files named with one of
// prefixes and last modified more than StaleAfter before now: a process
// that died between creating one and renaming or removing it left them,
// with the contents they were to hold. A symbolic link is never followed
// nor removed. Best effort: a file it cannot remove stays, and a missing
// dir is nothing to do.
func RemoveStale(dir string, now time.Time, prefixes ...string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(e.Name(), p) }) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= StaleAfter {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}
