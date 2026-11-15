//go:build !windows

package shellquote

// Quote returns s as one word of the OS shell: POSIX here.
func Quote(s string) string { return POSIX(s) }
