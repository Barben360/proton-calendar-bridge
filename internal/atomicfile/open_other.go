//go:build !windows

package atomicfile

import "os"

// Open is os.Open: on Unix a rename never conflicts with an open reader.
func Open(path string) (*os.File, error) { return os.Open(path) }

// Rename is os.Rename: no retry off Windows.
func Rename(from, to string) error { return os.Rename(from, to) }
