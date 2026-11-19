package desktop

import (
	"image/color"
	"strconv"
	"unicode"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// tipMaxWidth is the widest a tip gets before its text wraps.
const tipMaxWidth = 320

// tipLayer draws hover tips over the window's content. It sits in the
// content, above everything else, not in a canvas overlay: the driver looks
// for what is under the pointer in the top overlay alone, so a tip drawn in
// one would take the pointer from the icon that showed it, and the icon's
// MouseOut would hide it at once. Nothing in the layer answers the pointer.
type tipLayer struct {
	box   *fyne.Container // no layout: the tip is placed by hand
	owner *infoTip        // the icon whose tip shows; nil: none
}

func newTipLayer() *tipLayer { return &tipLayer{box: container.NewWithoutLayout()} }

// show draws owner's tip under it, kept inside the window (above it when
// there is no room below).
func (l *tipLayer) show(owner *infoTip) {
	label := widget.NewLabel(owner.text)
	label.Wrapping = fyne.TextWrapWord
	tip := newPanel(label, colorCard, theme.ColorNameSeparator, 6, 4, 0)
	natural := fyne.MeasureText(owner.text, theme.TextSize(), fyne.TextStyle{}).Width + 2*theme.InnerPadding() + 16
	w := min(natural, tipMaxWidth)
	tip.Resize(fyne.NewSize(w, tip.MinSize().Height))
	size := fyne.NewSize(w, tip.MinSize().Height) // the wrapped height at that width
	tip.Resize(size)

	at := fyne.CurrentApp().Driver().AbsolutePositionForObject(owner)
	area := l.box.Size()
	x := at.X + owner.Size().Width - size.Width // right-aligned on the icon
	x = max(8, min(x, area.Width-size.Width-8))
	y := at.Y + owner.Size().Height + 4
	if y+size.Height > area.Height-8 {
		y = at.Y - size.Height - 4
	}
	tip.Move(fyne.NewPos(x, max(8, y)))
	l.box.Objects = []fyne.CanvasObject{tip}
	l.owner = owner
	l.box.Refresh()
}

// hide removes the tip when owner shows it; a nil owner removes any tip.
func (l *tipLayer) hide(owner *infoTip) {
	if l.owner == nil || (owner != nil && owner != l.owner) {
		return
	}
	l.box.Objects = nil
	l.owner = nil
	l.box.Refresh()
}

// infoTip is a small info icon whose text shows while the pointer is on it,
// or after a tap.
type infoTip struct {
	widget.BaseWidget
	text  string
	layer *tipLayer
	icon  *widget.Icon
}

var (
	_ desktop.Hoverable = (*infoTip)(nil)
	_ fyne.Tappable     = (*infoTip)(nil)
	_ fyne.Focusable    = (*infoTip)(nil)
)

func newInfoTip(layer *tipLayer, text string) *infoTip {
	t := &infoTip{text: text, layer: layer}
	t.ExtendBaseWidget(t)
	return t
}

func (t *infoTip) CreateRenderer() fyne.WidgetRenderer {
	t.icon = widget.NewIcon(theme.NewColoredResource(theme.InfoIcon(), theme.ColorNameDisabled))
	return widget.NewSimpleRenderer(t.icon)
}

// FocusGained shows the tip, so the reason is reachable from the keyboard
// (Tab); the icon takes the primary colour as the focus mark.
func (t *infoTip) FocusGained() {
	if t.icon != nil {
		t.icon.SetResource(theme.NewColoredResource(theme.InfoIcon(), theme.ColorNamePrimary))
	}
	t.layer.show(t)
}

func (t *infoTip) FocusLost() {
	if t.icon != nil {
		t.icon.SetResource(theme.NewColoredResource(theme.InfoIcon(), theme.ColorNameDisabled))
	}
	t.layer.hide(t)
}

func (t *infoTip) TypedRune(rune)          {}
func (t *infoTip) TypedKey(*fyne.KeyEvent) {}

func (t *infoTip) MouseIn(*desktop.MouseEvent)    { t.layer.show(t) }
func (t *infoTip) MouseMoved(*desktop.MouseEvent) {}
func (t *infoTip) MouseOut()                      { t.layer.hide(t) }

// Tapped shows the tip (a click already showed it on hover: no toggle).
func (t *infoTip) Tapped(*fyne.PointEvent) { t.layer.show(t) }

// calendarDot is the dot of a calendar's colour before its name: clear when
// the colour is unknown or unreadable, so the names still line up.
func calendarDot(hex string) fyne.CanvasObject {
	var c color.Color = color.Transparent
	if v, ok := parseHexColor(hex); ok {
		c = v
	}
	_, o := dot(c, 10)
	return o
}

// parseHexColor reads Proton's "#RRGGBB" (any case).
func parseHexColor(s string) (color.NRGBA, bool) {
	if len(s) != 7 || s[0] != '#' {
		return color.NRGBA{}, false
	}
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return color.NRGBA{}, false
	}
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xFF}, true
}

// sentence is s with its first letter capitalised (the gate's reasons are
// written to follow "read-only: ").
func sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}
