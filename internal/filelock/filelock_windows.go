//go:build windows

package filelock

import (
	"errors"
	"io/fs"
	"math"

	"golang.org/x/sys/windows"
)

// lockOffset is where the one locked byte sits: far beyond any real file
// length, so the lock never blocks reads, writes or truncation of the
// content through other handles (Windows locks are mandatory, unlike flock).
const lockOffset uint64 = 1 << 62

// TryLock takes the exclusive lock on path (created if missing) without
// blocking. ok is false when another holder has it. Calling unlock releases
// the lock.
func TryLock(path string) (unlock func(), ok bool, err error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, false, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, false, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	ol := &windows.Overlapped{Offset: uint32(lockOffset & math.MaxUint32), OffsetHigh: uint32(lockOffset >> 32)}
	err = windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		_ = windows.CloseHandle(h)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { _ = windows.CloseHandle(h) }, true, nil // closing releases the lock
}
