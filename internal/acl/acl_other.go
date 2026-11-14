//go:build !windows

package acl

import "os"

// MkdirAll is os.MkdirAll(path, 0o700).
func MkdirAll(path string) error { return os.MkdirAll(path, 0o700) }

// Mkdir is os.Mkdir(path, 0o700).
func Mkdir(path string) error { return os.Mkdir(path, 0o700) }
