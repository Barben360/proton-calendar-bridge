//go:build !windows

package tlsutil

import (
	"fmt"
	"io/fs"
	"os"
)

// guard refuses path when another user owns it and narrows its mode to
// want; it returns path when it narrowed it.
func guard(path string, info fs.FileInfo, want fs.FileMode, _ bool) ([]string, error) {
	if uid, ok := ownerOf(info); ok && uid != os.Getuid() {
		return nil, fmt.Errorf("%s is owned by another user (uid %d): pcb will not use it", path, uid)
	}
	if info.Mode().Perm()&^want == 0 {
		return nil, nil
	}
	if err := os.Chmod(path, want); err != nil {
		return nil, fmt.Errorf("restrict %s: %w", path, err)
	}
	return []string{path}, nil
}
