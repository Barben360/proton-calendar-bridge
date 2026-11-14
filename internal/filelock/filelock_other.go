//go:build !unix

package filelock

// TryLock is unsupported off Unix.
func TryLock(string) (func(), bool, error) { return nil, false, ErrUnsupported }
