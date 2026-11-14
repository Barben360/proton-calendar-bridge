package desktop

import (
	"image/color"
	"math"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// luminance is WCAG's relative luminance of c.
func luminance(c color.Color) float64 {
	r, g, b, _ := c.RGBA()
	lin := func(v uint32) float64 {
		s := float64(v) / 0xffff
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(a, b color.Color) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Every text the window draws stays readable (WCAG AA, 4.5:1) on the surface
// it is drawn on, in the light and the dark variant.
func TestThemeContrast(t *testing.T) {
	th := Theme()
	pairs := []struct{ text, on fyne.ThemeColorName }{
		{theme.ColorNameForeground, theme.ColorNameBackground},
		{theme.ColorNameForeground, colorCard},
		{theme.ColorNameForeground, colorSelected},
		{theme.ColorNameForeground, theme.ColorNameButton},
		{theme.ColorNameForeground, theme.ColorNameInputBackground},
		{theme.ColorNameDisabled, colorCard}, // muted text: Low importance labels
		{theme.ColorNameDisabled, theme.ColorNameBackground},
		{theme.ColorNamePrimary, colorCard},     // accent text: High importance labels
		{theme.ColorNamePrimary, colorSelected}, // the avatar's initial
		{theme.ColorNameForegroundOnPrimary, theme.ColorNamePrimary},
		{theme.ColorNameForegroundOnError, theme.ColorNameError},
		{theme.ColorNameError, colorAlert}, // an alert's text
		{theme.ColorNameForeground, colorAlert},
		{theme.ColorNameWarning, colorBadge}, // a calendar's badge
	}
	for _, v := range []fyne.ThemeVariant{theme.VariantLight, theme.VariantDark} {
		for _, p := range pairs {
			if c := contrast(th.Color(p.text, v), th.Color(p.on, v)); c < 4.5 {
				t.Errorf("variant %d: %s on %s has contrast %.2f, want >= 4.5", v, p.text, p.on, c)
			}
		}
	}
}

// The light variant is light and the dark one dark: the theme follows the
// desktop's choice instead of forcing one.
func TestThemeVariants(t *testing.T) {
	th := Theme()
	light := luminance(th.Color(theme.ColorNameBackground, theme.VariantLight))
	dark := luminance(th.Color(theme.ColorNameBackground, theme.VariantDark))
	if light < 0.8 || dark > 0.05 {
		t.Errorf("background luminance light=%.2f dark=%.2f, want a light and a dark one", light, dark)
	}
	if th.Color(colorCard, theme.VariantLight) == th.Color(theme.ColorNameBackground, theme.VariantLight) {
		t.Error("cards do not stand out from the background")
	}
}

// The window draws in the Paper theme: its accent is the teal of the design.
func TestNewWindowAppliesTheme(t *testing.T) {
	f := newFixture(t)
	got := f.app.Settings().Theme().Color(theme.ColorNamePrimary, theme.VariantLight)
	if got != Theme().Color(theme.ColorNamePrimary, theme.VariantLight) || got == theme.DefaultTheme().Color(theme.ColorNamePrimary, theme.VariantLight) {
		t.Errorf("primary = %v, want the Paper teal", got)
	}
}
