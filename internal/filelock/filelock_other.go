//go:build !unix && !windows

package filelock

// TryLock is unsupported on this system.
func TryLock(string) (func(), bool, error) { return nil, false, ErrUnsupported }
