//go:build !linux && !windows

package desktop

import "errors"

// SetAutostart is not supported yet on this platform.
func SetAutostart(bool, string) error {
	return errors.New("start at login is not supported yet on this platform")
}

// Autostart is always false here.
func Autostart(string) bool { return false }
