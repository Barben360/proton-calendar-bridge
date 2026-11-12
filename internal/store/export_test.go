package store

// OnStateLockBusy makes f run when a state.json writer finds state.lock
// taken, before it waits. It returns a restore function.
func OnStateLockBusy(f func()) (restore func()) {
	old := stateLockBusy
	stateLockBusy = f
	return func() { stateLockBusy = old }
}
