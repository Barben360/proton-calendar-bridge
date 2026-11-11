package tlsutil

import "io/fs"

// SetOwner replaces the owner lookup and returns a restore function.
func SetOwner(f func(fs.FileInfo) (uid int, ok bool)) (restore func()) {
	old := ownerOf
	ownerOf = f
	return func() { ownerOf = old }
}
