//go:build unix

package store

import (
	"os"
	"syscall"
)

// DirOwner is the UID owning dir; ok is false when dir cannot be read (it
// does not exist yet, say) or the system has no owners.
func DirOwner(dir string) (uid int, ok bool) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
