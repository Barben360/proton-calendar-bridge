//go:build !unix && !windows

package store

import (
	"fmt"
	"io/fs"
)

// DirOwner reports no owner off Unix.
func DirOwner(string) (uid int, ok bool) { return 0, false }

// describeOwner is Unreadable's text where owners are unknown.
func describeOwner(_ string, info fs.FileInfo) string {
	return fmt.Sprintf("owner unknown, mode %04o", info.Mode().Perm())
}
