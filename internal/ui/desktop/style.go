package desktop

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// The building blocks of the Paper design. Each takes its colours from the
// theme when it refreshes, so a light/dark switch of the desktop redraws it.

// textLine is one line of text without a label's padding (compact lists,
// section titles, badges). With Truncate it fits any width, cut with "…".
type textLine struct {
	widget.BaseWidget
	Text      string
	ColorName fyne.ThemeColorName
	SizeName  fyne.ThemeSizeName
	Style     fyne.TextStyle
	Truncate  bool
}

func newTextLine(text string, c fyne.ThemeColorName, size fyne.ThemeSizeName, style fyne.TextStyle) *textLine {
	l := &textLine{Text: text, ColorName: c, SizeName: size, Style: style}
	l.ExtendBaseWidget(l)
	return l
}

func (l *textLine) SetText(s string) {
	l.Text = s
	l.Refresh()
}

func (l *textLine) CreateRenderer() fyne.WidgetRenderer {
	r := &textLineRenderer{l: l, t: canvas.NewText(l.Text, color.Black)}
	r.Refresh()
	return r
}

type textLineRenderer struct {
	l *textLine
	t *canvas.Text
}

func (r *textLineRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.t} }
func (r *textLineRenderer) Destroy()                     {}

func (r *textLineRenderer) size() float32 { return theme.SizeForWidget(r.l.SizeName, r.l) }

func (r *textLineRenderer) MinSize() fyne.Size {
	s := fyne.MeasureText(r.l.Text, r.size(), r.l.Style)
	if r.l.Truncate {
		s.Width = fyne.MeasureText("M…", r.size(), r.l.Style).Width
	}
	return s
}

func (r *textLineRenderer) Layout(size fyne.Size) {
	r.t.Text = r.fit(size.Width)
	r.t.Move(fyne.NewPos(0, (size.Height-r.t.MinSize().Height)/2))
	r.t.Resize(fyne.NewSize(size.Width, r.t.MinSize().Height))
}

// fit is the text cut to width, ending with "…" when cut.
func (r *textLineRenderer) fit(width float32) string {
	text := r.l.Text
	measure := func(s string) float32 { return fyne.MeasureText(s, r.size(), r.l.Style).Width }
	if !r.l.Truncate || width <= 0 || measure(text) <= width {
		return text
	}
	runes := []rune(text)
	lo, hi := 0, len(runes) // the longest prefix that fits, with the ellipsis
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if measure(string(runes[:mid])+"…") <= width {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(runes[:lo]) + "…"
}

func (r *textLineRenderer) Refresh() {
	r.t.Color = theme.ColorForWidget(r.l.ColorName, r.l)
	r.t.TextSize = r.size()
	r.t.TextStyle = r.l.Style
	r.Layout(r.l.Size())
	r.t.Refresh()
}

// panel is a rounded box behind content: fill, an optional outline, inner
// padding.
type panel struct {
	widget.BaseWidget
	content      fyne.CanvasObject
	fill, stroke fyne.ThemeColorName // stroke "" draws none
	radius       float32
	padX, padY   float32
}

func newPanel(content fyne.CanvasObject, fill, stroke fyne.ThemeColorName, radius, padX, padY float32) *panel {
	if content == nil {
		content = layout.NewSpacer()
	}
	p := &panel{content: content, fill: fill, stroke: stroke, radius: radius, padX: padX, padY: padY}
	p.ExtendBaseWidget(p)
	return p
}

func (p *panel) CreateRenderer() fyne.WidgetRenderer {
	r := &panelRenderer{p: p, bg: canvas.NewRectangle(color.Transparent)}
	r.Refresh()
	return r
}

type panelRenderer struct {
	p  *panel
	bg *canvas.Rectangle
}

func (r *panelRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.bg, r.p.content} }
func (r *panelRenderer) Destroy()                     {}

func (r *panelRenderer) MinSize() fyne.Size {
	return r.p.content.MinSize().Add(fyne.NewSize(2*r.p.padX, 2*r.p.padY))
}

func (r *panelRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	r.p.content.Move(fyne.NewPos(r.p.padX, r.p.padY))
	r.p.content.Resize(size.Subtract(fyne.NewSize(2*r.p.padX, 2*r.p.padY)))
}

func (r *panelRenderer) Refresh() {
	r.bg.FillColor = theme.ColorForWidget(r.p.fill, r.p)
	r.bg.CornerRadius = r.p.radius
	r.bg.StrokeWidth, r.bg.StrokeColor = 0, color.Transparent
	if r.p.stroke != "" {
		r.bg.StrokeWidth, r.bg.StrokeColor = 1, theme.ColorForWidget(r.p.stroke, r.p)
	}
	r.bg.Refresh()
}

// avatar is an account's initial in a rounded square: accent when signed in,
// neutral otherwise.
type avatar struct {
	widget.BaseWidget
	text   *canvas.Text
	strong bool
}

const avatarSize = 34

func newAvatar() *avatar {
	a := &avatar{text: canvas.NewText("", color.Black)}
	a.text.Alignment = fyne.TextAlignCenter
	a.text.TextStyle = fyne.TextStyle{Bold: true}
	a.ExtendBaseWidget(a)
	return a
}

func (a *avatar) set(initial string, strong bool) {
	a.text.Text, a.strong = initial, strong
	a.Refresh()
}

func (a *avatar) CreateRenderer() fyne.WidgetRenderer {
	r := &avatarRenderer{a: a, bg: canvas.NewRectangle(color.Transparent)}
	r.bg.CornerRadius = 9
	r.Refresh()
	return r
}

type avatarRenderer struct {
	a  *avatar
	bg *canvas.Rectangle
}

func (r *avatarRenderer) Objects() []fyne.CanvasObject { return []fyne.CanvasObject{r.bg, r.a.text} }
func (r *avatarRenderer) Destroy()                     {}
func (r *avatarRenderer) MinSize() fyne.Size           { return fyne.NewSquareSize(avatarSize) }

func (r *avatarRenderer) Layout(size fyne.Size) {
	r.bg.Resize(size)
	h := r.a.text.MinSize().Height
	r.a.text.Move(fyne.NewPos(0, (size.Height-h)/2))
	r.a.text.Resize(fyne.NewSize(size.Width, h))
}

func (r *avatarRenderer) Refresh() {
	if r.a.strong {
		r.bg.FillColor = theme.ColorForWidget(colorSelected, r.a)
		r.a.text.Color = theme.ColorForWidget(theme.ColorNamePrimary, r.a)
	} else {
		r.bg.FillColor = theme.ColorForWidget(theme.ColorNameButton, r.a)
		r.a.text.Color = theme.ColorForWidget(theme.ColorNameForeground, r.a)
	}
	r.a.text.TextSize = theme.SizeForWidget(theme.SizeNameText, r.a) + 1
	r.Layout(r.a.Size())
	r.bg.Refresh()
	r.a.text.Refresh()
}

// minSize lays its objects stacked, at least this size.
type minSize fyne.Size

func (m minSize) MinSize(objs []fyne.CanvasObject) fyne.Size {
	return layout.NewStackLayout().MinSize(objs).Max(fyne.Size(m))
}

func (minSize) Layout(objs []fyne.CanvasObject, size fyne.Size) {
	layout.NewStackLayout().Layout(objs, size)
}

func fixedWidth(w float32, o fyne.CanvasObject) *fyne.Container {
	return container.New(minSize{Width: w}, o)
}

// vbox stacks objects with gap between them.
func vbox(gap float32, objs ...fyne.CanvasObject) *fyne.Container {
	return container.New(layout.NewCustomPaddedVBoxLayout(gap), objs...)
}

func pad(top, bottom, left, right float32, o fyne.CanvasObject) *fyne.Container {
	return container.New(layout.NewCustomPaddedLayout(top, bottom, left, right), o)
}

// sectionTitle is the small capitals over a grouped box.
func sectionTitle(s string) fyne.CanvasObject {
	return pad(0, 0, 4, 0, newTextLine(s, theme.ColorNameDisabled, theme.SizeNameCaptionText, fyne.TextStyle{Bold: true}))
}

// group is a titled box of rows, the Paper design's unit: rows after the
// first are passed through divided, so a hidden row takes its line with it.
func group(title string, rows ...fyne.CanvasObject) *fyne.Container {
	return vbox(6, sectionTitle(title), newPanel(vbox(0, rows...), colorCard, theme.ColorNameSeparator, 12, 0, 0))
}

// divided is a row with the hairline above it.
func divided(row fyne.CanvasObject) *fyne.Container {
	return vbox(0, widget.NewSeparator(), row)
}

// groupRow is a grouped box's line: key on the left (fixed width when keyWidth >
// 0), value filling the middle, actions on the right.
func groupRow(key fyne.CanvasObject, keyWidth float32, value fyne.CanvasObject, actions ...fyne.CanvasObject) fyne.CanvasObject {
	if key != nil && keyWidth > 0 {
		key = fixedWidth(keyWidth, key)
	}
	var right fyne.CanvasObject
	if len(actions) > 0 {
		right = container.NewHBox(actions...)
	}
	if value == nil {
		value = layout.NewSpacer()
	}
	return container.New(minSize{Height: rowHeight}, pad(4, 4, 12, 8, container.NewBorder(nil, nil, key, right, value)))
}

// rowHeight is a grouped row's least height.
const rowHeight = 46

// badge is a short warning-coloured tag (a calendar's access).
func badge(text string) fyne.CanvasObject {
	l := newTextLine(text, theme.ColorNameWarning, theme.SizeNameCaptionText, fyne.TextStyle{Bold: true})
	return container.NewCenter(newPanel(l, colorBadge, "", 6, 8, 4))
}

// dot is a status dot of colour c.
func dot(c color.Color, size float32) (*canvas.Circle, fyne.CanvasObject) {
	d := canvas.NewCircle(c)
	return d, container.NewCenter(container.NewGridWrap(fyne.NewSquareSize(size), d))
}

// alertBox is a red-tinted box: a warning icon, the message in bold, then
// what to do.
func alertBox(content fyne.CanvasObject) fyne.CanvasObject {
	icon := widget.NewIcon(theme.NewErrorThemedResource(theme.WarningIcon()))
	return newPanel(container.NewBorder(nil, nil, pad(0, 0, 4, 0, vcenter(icon)), nil, content), colorAlert, theme.ColorNameError, 10, 8, 2)
}

// vcenter centres o vertically in the height it is given, keeping its width.
func vcenter(o fyne.CanvasObject) fyne.CanvasObject {
	return container.NewVBox(layout.NewSpacer(), o, layout.NewSpacer())
}
