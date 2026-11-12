package store

import "time"

// OnStateLockBusy makes f run when a state.json writer finds state.lock
// taken, before it waits. It returns a restore function.
func OnStateLockBusy(f func()) (restore func()) {
	old := stateLockBusy
	stateLockBusy = f
	return func() { stateLockBusy = old }
}

// SetStateLockTimeout replaces the state-file lock's timeout and returns a
// restore function. Tests that use it must not run in parallel.
func SetStateLockTimeout(d time.Duration) (restore func()) {
	old := stateLockTimeout
	stateLockTimeout = d
	return func() { stateLockTimeout = old }
}

// OnSyncDir makes f see every directory fsync of the store and returns a
// restore function. Tests that use it must not run in parallel.
func OnSyncDir(f func(dir string)) (restore func()) {
	old := syncDir
	syncDir = func(dir string) { f(dir); old(dir) }
	return func() { syncDir = old }
}

// StateWriteCause exposes stateWriteCause.
func StateWriteCause(err error) string { return stateWriteCause(err) }
