package desktop

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// The window's own colour names, next to Fyne's: the surfaces the Paper
// design adds.
const (
	colorCard     fyne.ThemeColorName = "pcb-card"     // the sidebar and the grouped boxes
	colorSelected fyne.ThemeColorName = "pcb-selected" // the selected account, an avatar
	colorAlert    fyne.ThemeColorName = "pcb-alert"    // an alert's box
	colorBadge    fyne.ThemeColorName = "pcb-badge"    // a calendar's badge
)

func hex(v uint32) color.NRGBA {
	return color.NRGBA{R: uint8(v >> 16), G: uint8(v >> 8), B: uint8(v), A: 0xff}
}

func alpha(c color.NRGBA, a uint8) color.NRGBA {
	c.A = a
	return c
}

// paper is the Paper design: light grey ground, white grouped boxes, a teal
// accent; its dark variant keeps the same roles. Text colours pass WCAG AA on
// the surface they are drawn on (theme_test.go).
var paper = map[fyne.ThemeVariant]map[fyne.ThemeColorName]color.Color{
	theme.VariantLight: {
		theme.ColorNameBackground:          hex(0xF3F4F6),
		theme.ColorNameHeaderBackground:    hex(0xF3F4F6),
		colorCard:                          hex(0xFFFFFF),
		theme.ColorNameMenuBackground:      hex(0xFFFFFF),
		theme.ColorNameOverlayBackground:   hex(0xFFFFFF),
		colorSelected:                      hex(0xE3F1EE),
		colorAlert:                         hex(0xFEF3F2),
		colorBadge:                         hex(0xFFF1DD),
		theme.ColorNameForeground:          hex(0x1B1D22),
		theme.ColorNameDisabled:            hex(0x5F6470),
		theme.ColorNamePlaceHolder:         hex(0x6B7280),
		theme.ColorNamePrimary:             hex(0x0B6E63),
		theme.ColorNameHyperlink:           hex(0x0B6E63),
		theme.ColorNameFocus:               hex(0x0B6E63),
		theme.ColorNameForegroundOnPrimary: hex(0xFFFFFF),
		theme.ColorNameError:               hex(0xB42318),
		theme.ColorNameForegroundOnError:   hex(0xFFFFFF),
		theme.ColorNameWarning:             hex(0x8A4B00),
		theme.ColorNameForegroundOnWarning: hex(0xFFFFFF),
		theme.ColorNameSuccess:             hex(0x1F7A45),
		theme.ColorNameForegroundOnSuccess: hex(0xFFFFFF),
		theme.ColorNameButton:              hex(0xEBEDF0),
		theme.ColorNameDisabledButton:      hex(0xE4E6EA),
		theme.ColorNameInputBackground:     hex(0xF7F8FA),
		theme.ColorNameInputBorder:         hex(0xD5D8DE),
		theme.ColorNameSeparator:           hex(0xE4E6EA),
		theme.ColorNameHover:               alpha(hex(0x000000), 0x0F),
		theme.ColorNamePressed:             alpha(hex(0x000000), 0x1A),
		theme.ColorNameSelection:           alpha(hex(0x0B6E63), 0x40),
		theme.ColorNameScrollBar:           alpha(hex(0x000000), 0x4D),
		theme.ColorNameShadow:              alpha(hex(0x000000), 0x26),
	},
	theme.VariantDark: {
		theme.ColorNameBackground:          hex(0x16181C),
		theme.ColorNameHeaderBackground:    hex(0x16181C),
		colorCard:                          hex(0x1F2228),
		theme.ColorNameMenuBackground:      hex(0x1F2228),
		theme.ColorNameOverlayBackground:   hex(0x1F2228),
		colorSelected:                      hex(0x1D3A36),
		colorAlert:                         hex(0x2B1C1E),
		colorBadge:                         hex(0x33291A),
		theme.ColorNameForeground:          hex(0xE8E9ED),
		theme.ColorNameDisabled:            hex(0x9BA1AB),
		theme.ColorNamePlaceHolder:         hex(0x8C929C),
		theme.ColorNamePrimary:             hex(0x3CC2B1),
		theme.ColorNameHyperlink:           hex(0x3CC2B1),
		theme.ColorNameFocus:               hex(0x3CC2B1),
		theme.ColorNameForegroundOnPrimary: hex(0x0B1F1C),
		theme.ColorNameError:               hex(0xF2837A),
		theme.ColorNameForegroundOnError:   hex(0x2A0E0B),
		theme.ColorNameWarning:             hex(0xE8A33D),
		theme.ColorNameForegroundOnWarning: hex(0x2A1C06),
		theme.ColorNameSuccess:             hex(0x4CC38A),
		theme.ColorNameForegroundOnSuccess: hex(0x0B1F14),
		theme.ColorNameButton:              hex(0x2A2E36),
		theme.ColorNameDisabledButton:      hex(0x24272E),
		theme.ColorNameInputBackground:     hex(0x16181C),
		theme.ColorNameInputBorder:         hex(0x3A3F4A),
		theme.ColorNameSeparator:           hex(0x2C3038),
		theme.ColorNameHover:               alpha(hex(0xFFFFFF), 0x0F),
		theme.ColorNamePressed:             alpha(hex(0xFFFFFF), 0x1A),
		theme.ColorNameSelection:           alpha(hex(0x3CC2B1), 0x40),
		theme.ColorNameScrollBar:           alpha(hex(0xFFFFFF), 0x4D),
		theme.ColorNameShadow:              alpha(hex(0x000000), 0x80),
	},
}

type paperTheme struct{}

// Theme is the window's look, the Paper design: Fyne's fonts and icons with
// its own colours and rounder corners.
func Theme() fyne.Theme { return paperTheme{} }

func (paperTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	if v != theme.VariantDark {
		v = theme.VariantLight
	}
	if c, ok := paper[v][n]; ok {
		return c
	}
	return theme.DefaultTheme().Color(n, v)
}

func (paperTheme) Font(s fyne.TextStyle) fyne.Resource { return theme.DefaultTheme().Font(s) }

func (paperTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }

func (paperTheme) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInputRadius, theme.SizeNameSelectionRadius:
		return 8
	}
	return theme.DefaultTheme().Size(n)
}
