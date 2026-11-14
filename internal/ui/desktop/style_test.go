package desktop

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
)

func drawn(t *testing.T, l *textLine) string {
	t.Helper()
	return test.WidgetRenderer(l).Objects()[0].(*canvas.Text).Text
}

// A truncating line too long for its room is cut with an ellipsis; its Text
// stays whole, and it shows whole again once it fits.
func TestTextLineTruncates(t *testing.T) {
	test.NewApp()
	t.Cleanup(func() { test.NewApp() })
	long := "firstname.lastname.with.a.long.part@example.org"
	l := newTextLine(long, theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{})
	l.Truncate = true
	full := fyne.MeasureText(long, theme.TextSize(), fyne.TextStyle{}).Width
	if l.MinSize().Width >= full {
		t.Fatalf("a truncating line asks for its full width %v", l.MinSize().Width)
	}
	l.Resize(fyne.NewSize(full/2, l.MinSize().Height))
	got := drawn(t, l)
	if !strings.HasSuffix(got, "…") || len(got) >= len(long) || !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Errorf("drawn %q, want a prefix of the address and …", got)
	}
	if w := fyne.MeasureText(got, theme.TextSize(), fyne.TextStyle{}).Width; w > full/2 {
		t.Errorf("drawn width %v exceeds the room %v", w, full/2)
	}
	if l.Text != long {
		t.Errorf("Text changed to %q", l.Text)
	}
	l.Resize(fyne.NewSize(full+10, l.MinSize().Height))
	if got := drawn(t, l); got != long {
		t.Errorf("with room, drawn %q, want the whole text", got)
	}
}

// A line that does not truncate asks for its whole width.
func TestTextLineWithoutTruncateIsWhole(t *testing.T) {
	test.NewApp()
	t.Cleanup(func() { test.NewApp() })
	l := newTextLine("Personal", theme.ColorNameForeground, theme.SizeNameText, fyne.TextStyle{})
	if want := fyne.MeasureText("Personal", theme.TextSize(), fyne.TextStyle{}).Width; l.MinSize().Width < want {
		t.Errorf("min width %v, want >= %v", l.MinSize().Width, want)
	}
}
