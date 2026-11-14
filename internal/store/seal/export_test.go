package seal

// SetLink replaces the hard link function and returns a restore function.
func SetLink(f func(oldname, newname string) error) (restore func()) {
	old := link
	link = f
	return func() { link = old }
}

// Bounded is the error mapping run applies once fn returns.
var Bounded = bounded
