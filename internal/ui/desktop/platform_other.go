//go:build !linux

package desktop

import (
	"errors"

	"fyne.io/fyne/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// TrayHost is true: Windows and macOS always have a tray (a menu bar).
func TrayHost(string) bool { return true }

// Notify falls back to Fyne's notification; a click is not reported.
func Notify(_ string, n ui.Notification, _ func(account string)) error {
	a := fyne.CurrentApp()
	if a == nil {
		return errors.New("no application")
	}
	a.SendNotification(fyne.NewNotification(n.Title, n.Body))
	return nil
}

// SetAutostart is not supported yet on this platform.
func SetAutostart(bool, string) error {
	return errors.New("start at login is not supported yet on this platform")
}

// Autostart is always false here.
func Autostart() bool { return false }
