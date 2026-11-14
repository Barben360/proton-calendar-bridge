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
