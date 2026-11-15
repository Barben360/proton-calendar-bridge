//go:build !windows

package main

import (
	"fmt"
	"io/fs"
)

// private refuses an account file others can read (mode bits): the error
// and the hint that fixes it.
func private(path string, fi fs.FileInfo) (hint string, err error) {
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "chmod 600 " + path, fmt.Errorf("account file %s has mode %#o: others can read it", path, perm)
	}
	return "", nil
}
