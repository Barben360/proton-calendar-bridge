//go:build (linux || windows) && cgo

package main

import (
	"fmt"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// checkGL refuses to start when open, which creates a hidden window asking
// for an OpenGL 2.0 context, fails: Fyne cannot draw there (a VM without 3D
// acceleration, a remote desktop session; Windows' own OpenGL is 1.1).
// Exit 10, the person must act. It runs before any state is opened.
func checkGL(open func() error) error {
	if err := open(); err != nil {
		return &exitcode.Error{Code: exitcode.MustAct,
			Err:  fmt.Errorf("pcb-ui needs OpenGL 2.0, which this display does not offer (a VM without 3D acceleration, or a remote desktop session): %w", err),
			Hint: `see docs/troubleshooting.md, "pcb-ui needs OpenGL 2.0": Mesa's software opengl32.dll beside pcb-ui.exe works around it`}
	}
	return nil
}
