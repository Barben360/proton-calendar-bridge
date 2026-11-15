// Package filelock provides exclusive advisory file locks between processes.
// They work on Unix (flock) and Windows (LockFileEx); elsewhere every call
// returns ErrUnsupported.
package filelock

import (
	"context"
	"errors"
	"time"
)

// ErrUnsupported is returned on systems with no file lock.
var ErrUnsupported = errors.New("file locks are not supported on this system")

const retryEvery = 50 * time.Millisecond

// Lock takes the exclusive lock on path, retrying every 50 ms until it is
// free or ctx ends (then it returns ctx's error).
func Lock(ctx context.Context, path string) (unlock func(), err error) {
	for {
		unlock, ok, err := TryLock(path)
		if err != nil || ok {
			return unlock, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryEvery):
		}
	}
}
