//go:build unix

package filelock

import (
	"errors"
	"os"
	"syscall"
)

// TryLock takes the exclusive lock on path (created 0600 if missing)
// without blocking. ok is false when another holder has it. Calling unlock
// releases the lock.
func TryLock(path string) (unlock func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { _ = f.Close() }, true, nil // closing releases the lock
}
