//go:build !unix

package install

import "io/fs"

// fileOwner is unknown off Unix.
func fileOwner(fs.FileInfo) (uid, gid int, known bool) { return 0, 0, false }

// linkCount is unknown off Unix.
func linkCount(fs.FileInfo) (n uint64, known bool) { return 0, false }
