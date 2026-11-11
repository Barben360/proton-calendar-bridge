//go:build unix

package session

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// lockLogin takes the exclusive, non-blocking login lock in dir. It makes
// the guard's check-then-stamp atomic across concurrent runs.
func lockLogin(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, "login.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits an int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("another pcb-lab login is running"), Hint: "wait for it to finish"}
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}
