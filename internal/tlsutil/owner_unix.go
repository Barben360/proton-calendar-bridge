//go:build unix

package tlsutil

import (
	"io/fs"
	"syscall"
)

// ownerOf returns the UID owning a file.
var ownerOf = func(fi fs.FileInfo) (uid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
