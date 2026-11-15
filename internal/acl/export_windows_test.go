package acl

// SetMkdir replaces the Mkdir MkdirAll calls and returns a restore function.
func SetMkdir(f func(string) error) (restore func()) {
	old := mkdir
	mkdir = f
	return func() { mkdir = old }
}

// SetOwnerOf replaces the owner lookup of a directory made meanwhile and
// returns a restore function.
func SetOwnerOf(f func(string) (string, error)) (restore func()) {
	old := ownerOf
	ownerOf = f
	return func() { ownerOf = old }
}
