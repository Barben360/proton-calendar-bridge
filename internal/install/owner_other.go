//go:build !unix

package install

import "io/fs"

// fileOwner is unknown off Unix.
func fileOwner(fs.FileInfo) (uid, gid int, known bool) { return 0, 0, false }
