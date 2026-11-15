package main

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

const appSVG = "../../../internal/ui/desktop/icons/app.svg"

// The app icon at 256 px, as the .syso embeds it: scaled to the full size
// (drawn in the middle, which a 64 px drawing in the top-left corner leaves
// empty), transparent outside its frame (the SVG's 2.5 px margin).
func TestRenderIcon(t *testing.T) {
	svg, err := os.ReadFile(appSVG)
	if err != nil {
		t.Fatal(err)
	}
	img, err := render(svg, 256)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 256 {
		t.Fatalf("bounds %v, want 256×256", b)
	}
	if _, _, _, a := img.At(128, 128).RGBA(); a == 0 {
		t.Error("the centre is transparent: nothing was drawn")
	}
	for _, p := range [][2]int{{0, 0}, {255, 0}, {0, 255}, {255, 255}} {
		if _, _, _, a := img.At(p[0], p[1]).RGBA(); a != 0 {
			t.Errorf("corner %v has alpha %d, want 0 (outside the frame)", p, a)
		}
	}
}

func TestRenderRefusesWhatIsNotSVG(t *testing.T) {
	if _, err := render([]byte("not an svg"), 256); err == nil {
		t.Error("render of a non-SVG: no error")
	}
}

// main writes the PNG named by -o.
func TestMainWritesThePNG(t *testing.T) {
	out := filepath.Join(t.TempDir(), "icon.png")
	if err := write(appSVG, out, 256); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width != 256 || cfg.Height != 256 {
		t.Errorf("PNG %dx%d, %v; want 256×256", cfg.Width, cfg.Height, err)
	}
}
