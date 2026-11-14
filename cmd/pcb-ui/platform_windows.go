//go:build windows && cgo

package main

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/wingui"
)

// preflight checks for OpenGL 2.0 before any state is opened.
func preflight() error { return checkGL(openGL) }

// showPreflightError prints a failed preflight in the terminal that started
// pcb-ui, or, started from Explorer or at login with no terminal, shows it
// in a message box: without OpenGL Fyne cannot open a window to say it.
func showPreflightError(err error) {
	attachConsole()
	if wingui.HasStderr() {
		printError(os.Stderr, err)
		return
	}
	text := err.Error()
	if h := exitcode.HintOf(err); h != "" {
		text += "\n\n" + h
	}
	t, err1 := windows.UTF16PtrFromString(strings.ReplaceAll(text, "\x00", ""))
	c, err2 := windows.UTF16PtrFromString("pcb-ui cannot start")
	if err1 == nil && err2 == nil {
		_, _ = windows.MessageBox(0, t, c, windows.MB_OK|windows.MB_ICONERROR)
	}
}

// attachConsole gives --version, --help and usage errors the terminal that
// started pcb-ui (a windowsgui binary has no console of its own).
func attachConsole() { wingui.AttachParentConsole() }

// watchSessionEnd runs end at logoff or shutdown, before Windows ends
// pcb-ui (it gets no signal: it has no console).
func watchSessionEnd(end func()) (stop func(), err error) { return wingui.WatchSessionEnd(end) }
