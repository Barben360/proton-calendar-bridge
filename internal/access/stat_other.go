//go:build !windows

package access

import "os"

// statFile is os.Stat: its FileInfo holds the device and inode already.
func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }
