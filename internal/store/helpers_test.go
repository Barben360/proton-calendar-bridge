package store_test

import (
	"os"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

// lockFor holds the secrets lock, to make a competing Update wait.
func lockFor(t *testing.T, f fixture) func() {
	t.Helper()
	if err := os.MkdirAll(f.state, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Lock(ctx(t), f.secrets.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}
