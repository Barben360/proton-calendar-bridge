//go:build unix

package store

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"strconv"
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

// describeOwner is Unreadable's "owner NAME, mode 0000" for path.
func describeOwner(path string, info fs.FileInfo) string {
	owner := "unknown"
	if uid, ok := DirOwner(path); ok {
		owner = "#" + strconv.Itoa(uid)
		if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
			owner = u.Username
		}
	}
	return fmt.Sprintf("owner %s, mode %04o", owner, info.Mode().Perm())
}
