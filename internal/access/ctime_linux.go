//go:build linux

package access

import (
	"os"
	"syscall"
)

// changeTime is the file's ctime in nanoseconds.
func changeTime(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctim.Nano()
	}
	return 0
}
