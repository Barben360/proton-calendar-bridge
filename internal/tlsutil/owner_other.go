//go:build !unix

package tlsutil

import "io/fs"

// ownerOf is unknown where there are no Unix owners: nothing is checked.
var ownerOf = func(fs.FileInfo) (uid int, ok bool) { return 0, false }
