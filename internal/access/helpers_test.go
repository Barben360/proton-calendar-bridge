package access_test

import (
	"encoding/json"
	"path/filepath"

	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func filepathJoin(e ...string) string { return filepath.Join(e...) }

// holdLock takes the clients lock like another process would; ok is false
// where the system has no file locks.
func holdLock(t *testing.T, path string) (release func(), ok bool) {
	t.Helper()
	unlock, got, err := filelock.TryLock(path)
	if err != nil || !got {
		return nil, false
	}
	return unlock, true
}
