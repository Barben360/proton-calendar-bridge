//go:build !windows

package seal

import "io/fs"

// checkKeyAccess refuses a key file whose mode lets other users in.
func checkKeyAccess(path string, info fs.FileInfo) error {
	if info.Mode().Perm()&0o077 != 0 {
		return &KeyModeError{Path: path, Mode: info.Mode().Perm()}
	}
	return nil
}
