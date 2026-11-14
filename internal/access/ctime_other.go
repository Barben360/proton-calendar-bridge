//go:build !linux

package access

import "os"

// changeTime is unknown here; os.SameFile identifies the file instead.
func changeTime(os.FileInfo) int64 { return 0 }
