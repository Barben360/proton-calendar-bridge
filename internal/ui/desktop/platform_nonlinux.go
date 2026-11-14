//go:build !linux

package desktop

import (
	"errors"

	"fyne.io/fyne/v2"

	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// TrayHost is true: Windows and macOS always have a tray (a menu bar).
func TrayHost(string) bool { return true }

// Notify falls back to Fyne's notification (a toast on Windows); a click is
// not reported, so the text says where to look, and the tray icon's
// attention state carries the same news when the toast does not show.
func Notify(_ string, n ui.Notification, _ func(account string)) error {
	a := fyne.CurrentApp()
	if a == nil {
		return errors.New("no application")
	}
	a.SendNotification(fyne.NewNotification(n.Title, n.Body))
	return nil
}
