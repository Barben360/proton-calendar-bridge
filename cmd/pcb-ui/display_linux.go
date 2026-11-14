//go:build linux && cgo

package main

import (
	"os"

	"github.com/go-gl/glfw/v3.4/glfw"
)

// openDisplay initialises GLFW as Fyne's driver will (FYNE_PLATFORM
// honoured, else GLFW's own choice), then terminates it: an error means
// Fyne could not open a window. It must run on the main thread, before
// Fyne starts.
func openDisplay() error {
	switch os.Getenv("FYNE_PLATFORM") {
	case "x11":
		glfw.InitHint(glfw.PlatformHint, int(glfw.PlatformX11))
	case "wayland":
		glfw.InitHint(glfw.PlatformHint, int(glfw.PlatformWayland))
	}
	if err := glfw.Init(); err != nil {
		return err
	}
	glfw.Terminate()
	return nil
}

// setWindowClass makes appID the X11 class (WM_CLASS) of every window GLFW
// opens from now on; on Wayland the app ID is already the app_id. The
// desktop entry's StartupWMClass names it, which is how the shell ties a
// window to the entry and shows its icon. Without it GLFW takes the title
// ("pcb"), and Fyne runs on XWayland under GNOME, so GNOME showed no logo.
// It must run on the main thread, after Fyne initialised GLFW (its first
// NewWindow does) and before the window is shown: Fyne opens the native
// window at the first Show, and glfw.Init resets the hints.
func setWindowClass() {
	glfw.WindowHintString(glfw.X11InstanceName, appID)
	glfw.WindowHintString(glfw.X11ClassName, appID)
}
