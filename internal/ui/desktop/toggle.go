package desktop

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const (
	toggleW, toggleH = 40, 22
	knobInset        = 2
)

// toggle is an on/off switch (Fyne has none). A tap, or space while it has
// the focus, flips it and calls OnChanged; SetOn draws a state silently.
// Label names what it switches, for the row it sits in and for tests.
type toggle struct {
	widget.DisableableWidget
	Label     string
	On        bool
	OnChanged func(on bool)
	focused   bool
}

func newToggle(label string, changed func(on bool)) *toggle {
	t := &toggle{Label: label, OnChanged: changed}
	t.ExtendBaseWidget(t)
	return t
}

// SetOn shows on without calling OnChanged.
func (t *toggle) SetOn(on bool) {
	t.On = on
	t.Refresh()
}

func (t *toggle) flip() {
	if t.Disabled() {
		return
	}
	t.On = !t.On
	t.Refresh()
	if t.OnChanged != nil {
		t.OnChanged(t.On)
	}
}

func (t *toggle) Tapped(*fyne.PointEvent) { t.flip() }

func (t *toggle) FocusGained()   { t.focused = true; t.Refresh() }
func (t *toggle) FocusLost()     { t.focused = false; t.Refresh() }
func (t *toggle) TypedRune(rune) {}

func (t *toggle) TypedKey(k *fyne.KeyEvent) {
	if k.Name == fyne.KeySpace {
		t.flip()
	}
}

func (t *toggle) MinSize() fyne.Size {
	t.ExtendBaseWidget(t)
	return t.BaseWidget.MinSize()
}

func (t *toggle) CreateRenderer() fyne.WidgetRenderer {
	t.ExtendBaseWidget(t)
	r := &toggleRenderer{t: t, track: canvas.NewRectangle(color.Transparent), knob: canvas.NewCircle(color.White)}
	r.track.CornerRadius = toggleH / 2
	r.Refresh()
	return r
}

type toggleRenderer struct {
	t     *toggle
	track *canvas.Rectangle
	knob  *canvas.Circle
}

func (r *toggleRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.track, r.knob} }
func (r *toggleRenderer) Destroy()                     {}
func (r *toggleRenderer) MinSize() fyne.Size           { return fyne.NewSize(toggleW, toggleH) }

func (r *toggleRenderer) Layout(size fyne.Size) {
	top := (size.Height - toggleH) / 2
	r.track.Move(fyne.NewPos(0, top))
	r.track.Resize(fyne.NewSize(toggleW, toggleH))
	d := float32(toggleH - 2*knobInset)
	x := float32(knobInset)
	if r.t.On {
		x = toggleW - knobInset - d
	}
	r.knob.Move(fyne.NewPos(x, top+knobInset))
	r.knob.Resize(fyne.NewSquareSize(d))
}

func (r *toggleRenderer) Refresh() {
	fill, knob := theme.ColorForWidget(theme.ColorNameInputBorder, r.t), color.Color(color.White)
	switch {
	case r.t.Disabled():
		fill, knob = theme.ColorForWidget(theme.ColorNameDisabledButton, r.t), theme.ColorForWidget(theme.ColorNameDisabled, r.t)
	case r.t.On:
		fill = theme.ColorForWidget(theme.ColorNamePrimary, r.t)
	}
	r.track.FillColor, r.knob.FillColor = fill, knob
	r.track.StrokeWidth, r.track.StrokeColor = 0, color.Transparent
	if r.t.focused {
		r.track.StrokeWidth, r.track.StrokeColor = 2, theme.ColorForWidget(theme.ColorNameFocus, r.t)
	}
	r.Layout(r.t.Size())
	r.track.Refresh()
	r.knob.Refresh()
}
