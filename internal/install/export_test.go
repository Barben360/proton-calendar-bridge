package install

// SetLink replaces os.Link, returning the function that restores it.
func SetLink(f func(oldname, newname string) error) (restore func()) {
	old := link
	link = f
	return func() { link = old }
}
