// Package atomicfile writes files so that a reader never sees a partial one.
package atomicfile

import (
	"os"
	"path/filepath"
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
	if d, err := os.Open(filepath.Dir(path)); err == nil { // best effort
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
