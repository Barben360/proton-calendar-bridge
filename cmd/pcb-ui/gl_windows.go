//go:build windows && cgo

package main

import (
	"errors"

	"github.com/go-gl/glfw/v3.4/glfw"
)

// openGL creates and destroys a hidden 1×1 window asking for the OpenGL 2.0
// context Fyne draws with: glfw.Init alone succeeds on Windows' own OpenGL
// 1.1, where Fyne would then fail. It must run on the main thread, before
// Fyne starts.
func openGL() error {
	if err := glfw.Init(); err != nil {
		return err
	}
	defer glfw.Terminate()
	glfw.WindowHint(glfw.Visible, glfw.False)
	glfw.WindowHint(glfw.ContextVersionMajor, 2)
	glfw.WindowHint(glfw.ContextVersionMinor, 0)
	w, err := glfw.CreateWindow(1, 1, "pcb-ui", nil, nil)
	if err != nil {
		return err
	}
	if w == nil { // a platform error, which go-gl logs and does not return
		return errors.New("GLFW could not create an OpenGL window")
	}
	w.Destroy()
	return nil
}
