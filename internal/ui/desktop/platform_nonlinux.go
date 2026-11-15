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
// attention state carries the same news when the toast does not show. The
// title and body go through toastText: Fyne's Windows toast would show a
// line break as \n and run what ends its PowerShell string.
func Notify(_ string, n ui.Notification, _ func(account string)) error {
	a := fyne.CurrentApp()
	if a == nil {
		return errors.New("no application")
	}
	a.SendNotification(fyne.NewNotification(toastText(n.Title), toastText(n.Body)))
	return nil
}
