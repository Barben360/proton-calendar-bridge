//go:build linux && cgo

package main

import (
	"errors"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// walkTexts collects the labels and buttons under o.
func walkTexts(o fyne.CanvasObject, out *[]string, buttons map[string]*widget.Button) {
	switch v := o.(type) {
	case *widget.Label:
		*out = append(*out, v.Text)
	case *widget.Button:
		*out = append(*out, "["+v.Text+"]")
		buttons[v.Text] = v
	case *fyne.Container:
		for _, c := range v.Objects {
			walkTexts(c, out, buttons)
		}
	}
}

// The startup error is the window's own content, sized to it: no dialog
// card inside a larger window (live 2026-11-13, F9: black bars left and
// right of the dialog). Its text is the error and the hint; OK runs done.
func TestStartupErrorWindowFitsItsContent(t *testing.T) {
	a := test.NewApp()
	t.Cleanup(func() { test.NewApp() })
	err := &exitcode.Error{Code: exitcode.Blocked,
		Err:  errors.New("pcb serve (PID 19384) already serves this state directory"),
		Hint: "stop it (pcb service stop when installed as a service) and start pcb-ui again"}
	done := 0
	w := errorWindow(a, err, func() { done++ })
	w.Show()
	c := w.Canvas()
	if c.Overlays().Top() != nil {
		t.Error("the error is a dialog overlay: the window around it shows bars")
	}
	content := c.Content()
	min := content.MinSize()
	if got := c.Size(); got != min {
		t.Errorf("window %v, content needs %v: the window must be sized to its content", got, min)
	}
	if min.Width > 640 {
		t.Errorf("window %v wide: the text is not wrapped", min)
	}
	var texts []string
	buttons := map[string]*widget.Button{}
	walkTexts(content, &texts, buttons)
	all := strings.Join(texts, "\n")
	for _, want := range []string{"pcb-ui cannot start", "pcb serve (PID 19384) already serves this state directory", "stop it (pcb service stop when installed as a service)", "[OK]"} {
		if !strings.Contains(all, want) {
			t.Errorf("window lacks %q: %q", want, texts)
		}
	}
	test.Tap(buttons["OK"])
	if done != 1 {
		t.Errorf("OK ran done %d times, want 1", done)
	}
}

func TestWrapText(t *testing.T) {
	got := wrapText("aaa bbb ccc\n\nddd", 7)
	if want := "aaa bbb\nccc\n\nddd"; got != want {
		t.Errorf("wrapText = %q, want %q", got, want)
	}
	long := strings.Repeat("x", 20)
	if got := wrapText("a "+long+" b", 7); got != "a\n"+long+"\nb" {
		t.Errorf("a word longer than the width stays whole: %q", got)
	}
}
