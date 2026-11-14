//go:build unix

package install

import (
	"io/fs"
	"syscall"
)

// fileOwner is the owner of fi.
func fileOwner(fi fs.FileInfo) (uid, gid int, known bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}

// linkCount is fi's number of hard links.
func linkCount(fi fs.FileInfo) (n uint64, known bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true //nolint:unconvert // uint16 or uint32 on other Unix
}
