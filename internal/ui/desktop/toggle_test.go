package desktop

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
)

func newTestToggle(t *testing.T) (*toggle, *[]bool) {
	t.Helper()
	a := test.NewApp()
	t.Cleanup(func() { test.NewApp() })
	a.Settings().SetTheme(Theme())
	var got []bool
	tg := newToggle("CalDAV", func(on bool) { got = append(got, on) })
	return tg, &got
}

func TestToggleTapFlipsAndReports(t *testing.T) {
	tg, got := newTestToggle(t)
	test.Tap(tg)
	test.Tap(tg)
	if len(*got) != 2 || !(*got)[0] || (*got)[1] {
		t.Fatalf("OnChanged calls = %v, want [true false]", *got)
	}
	if tg.On {
		t.Error("two taps left the toggle on")
	}
}

// SetOn draws a state from the view: the person did nothing, so nothing runs.
func TestToggleSetOnIsSilent(t *testing.T) {
	tg, got := newTestToggle(t)
	tg.SetOn(true)
	if !tg.On || len(*got) != 0 {
		t.Fatalf("On=%v calls=%v, want on and no call", tg.On, *got)
	}
}

func TestToggleDisabledIgnoresTaps(t *testing.T) {
	tg, got := newTestToggle(t)
	tg.Disable()
	test.Tap(tg)
	if tg.On || len(*got) != 0 || !tg.Disabled() {
		t.Fatalf("On=%v calls=%v disabled=%v", tg.On, *got, tg.Disabled())
	}
	tg.Enable()
	test.Tap(tg)
	if !tg.On {
		t.Error("an enabled toggle did not flip")
	}
}

// The keyboard reaches it like a check box: focus, then space.
func TestToggleSpaceKey(t *testing.T) {
	tg, got := newTestToggle(t)
	var _ fyne.Focusable = tg
	tg.TypedKey(&fyne.KeyEvent{Name: fyne.KeySpace})
	tg.TypedKey(&fyne.KeyEvent{Name: fyne.KeyA})
	if len(*got) != 1 || !(*got)[0] {
		t.Fatalf("calls = %v, want one [true]", *got)
	}
}

// On shows the accent track with the knob on the right; off a neutral track
// with the knob on the left.
func TestToggleDrawsState(t *testing.T) {
	tg, _ := newTestToggle(t)
	tg.Resize(tg.MinSize())
	r := test.WidgetRenderer(tg)
	track := r.Objects()[0].(*canvas.Rectangle)
	knob := r.Objects()[1].(*canvas.Circle)
	th := Theme()
	v := fyne.CurrentApp().Settings().ThemeVariant()
	if track.FillColor == th.Color(theme.ColorNamePrimary, v) {
		t.Error("an off toggle has the accent track")
	}
	offX := knob.Position().X
	tg.SetOn(true)
	if track.FillColor != th.Color(theme.ColorNamePrimary, v) {
		t.Errorf("on track = %v, want the accent", track.FillColor)
	}
	if knob.Position().X <= offX {
		t.Errorf("knob x on=%v off=%v, want it to move right", knob.Position().X, offX)
	}
}
