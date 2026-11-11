package events

// SetMaxPages changes ListAll's page cap and returns a function restoring it.
func SetMaxPages(n int) func() {
	old := maxPages
	maxPages = n
	return func() { maxPages = old }
}
