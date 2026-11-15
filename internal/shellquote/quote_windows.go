//go:build windows

package shellquote

// Quote returns s as one word of the OS shell: PowerShell here.
func Quote(s string) string { return PowerShell(s) }
