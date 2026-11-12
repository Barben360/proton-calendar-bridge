//go:build !unix

package store

// DirOwner reports no owner off Unix.
func DirOwner(string) (uid int, ok bool) { return 0, false }

func fileOwner(string) (uid int, ok bool) { return 0, false }
