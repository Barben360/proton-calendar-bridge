//go:build !unix

package tlsutil

import "io/fs"

// ownerOf is unknown where there are no Unix owners: no UID is checked
// (Windows checks access lists and owner SIDs, secure_windows.go).
var ownerOf = func(fs.FileInfo) (uid int, ok bool) { return 0, false }
