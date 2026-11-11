//go:build unix

package doctor

import (
	"os"
	"syscall"
)

// device returns the ID of the device holding path.
func device(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return uint64(st.Dev), ok //nolint:unconvert // Dev is int32 or uint64 by platform
}
