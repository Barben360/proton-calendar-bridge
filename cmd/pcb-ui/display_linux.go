//go:build linux && cgo

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-gl/glfw/v3.4/glfw"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// noDisplayLine is what pcb-ui says when no graphical session is there:
// GLFW would abort without a readable message.
const noDisplayLine = "pcb-ui needs a graphical session (no DISPLAY or WAYLAND_DISPLAY)"

// displayHint is what to do without a usable display.
const displayHint = "run pcb-ui from the desktop session (its terminal, or the applications menu); on a headless host run pcb serve instead"

// checkDisplay refuses to start without a graphical session, or with one
// open cannot open (live 2026-11-13, F8: DISPLAY set over SSH without the X
// authority; Fyne printed its raw GLFW error, then panicked): exit 10, the
// person must start it elsewhere. It runs before any state is opened.
func checkDisplay(env func(string) string, open func() error) error {
	if env("DISPLAY") == "" && env("WAYLAND_DISPLAY") == "" {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New(noDisplayLine), Hint: displayHint}
	}
	if err := open(); err != nil {
		var which []string
		for _, k := range []string{"WAYLAND_DISPLAY", "DISPLAY"} {
			if v := env(k); v != "" {
				which = append(which, k+"="+v)
			}
		}
		return &exitcode.Error{Code: exitcode.MustAct,
			Err:  fmt.Errorf("pcb-ui cannot open the display (%s): %w", strings.Join(which, ", "), err),
			Hint: displayHint}
	}
	return nil
}

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
