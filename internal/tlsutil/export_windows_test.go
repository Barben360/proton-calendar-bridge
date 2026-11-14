package tlsutil

// SetOwnerSID replaces the owner SID lookup and returns a restore function.
func SetOwnerSID(f func(string) (string, error)) (restore func()) {
	old := ownerSID
	ownerSID = f
	return func() { ownerSID = old }
}
