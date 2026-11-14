//go:build !unix && !windows

package tlsutil

import "io/fs"

// ownerOf is unknown where there are no Unix owners: no UID is checked
// (Windows checks access lists and owner SIDs instead, secure_windows.go).
var ownerOf = func(fs.FileInfo) (uid int, ok bool) { return 0, false }
