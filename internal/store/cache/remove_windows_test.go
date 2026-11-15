package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// SQLite holds pcb.db open without FILE_SHARE_DELETE: Remove waits for the
// holder (a serve closing its handle on a login-state change) and fails
// once its bound has passed.
func TestRemoveWaitsForAnOpenCacheToClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pcb.db")
	c, err := Open(path, "acc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	old := RemoveRetry
	t.Cleanup(func() { RemoveRetry = old })
	RemoveRetry = 100 * time.Millisecond
	if err := Remove(path); err == nil {
		t.Fatal("Remove of a cache held open succeeded, want its sharing violation after the bound")
	}

	RemoveRetry = 5 * time.Second
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = c.Close()
	}()
	if err := Remove(path); err != nil {
		t.Fatalf("Remove after the holder closed: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pcb.db still there: %v", err)
	}
}
