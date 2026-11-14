//go:build !windows

package winzone

// Key is the Windows zone key of the process; there is none off Windows.
func Key() (string, bool) { return "", false }
