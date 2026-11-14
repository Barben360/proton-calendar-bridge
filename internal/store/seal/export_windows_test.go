package seal

// SetKeyDirOwner replaces the owner lookup of a new key's directory and
// returns a restore function.
func SetKeyDirOwner(f func(string) (string, error)) (restore func()) {
	old := keyDirOwner
	keyDirOwner = f
	return func() { keyDirOwner = old }
}
