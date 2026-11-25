// Command winres renders pcb-ui's icon (internal/ui/desktop/icons/app.svg)
// to a PNG, which go-winres embeds with the version resource in the .syso
// that gives pcb-ui.exe its icon (make winres, on Windows only). It is a
// build tool, never part of a binary.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"

	"github.com/fyne-io/oksvg"
	"github.com/srwiley/rasterx"
)

func main() {
	svg := flag.String("svg", "internal/ui/desktop/icons/app.svg", "the SVG to render")
	out := flag.String("o", "", "the PNG to write")
	size := flag.Int("size", 256, "the PNG's width and height, in pixels")
	flag.Parse()
	if *out == "" || flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := write(*svg, *out, *size); err != nil {
		fmt.Fprintln(os.Stderr, "winres:", err)
		os.Exit(1)
	}
}

// write renders the SVG at svgPath to a size×size PNG at out.
func write(svgPath, out string, size int) error {
	b, err := os.ReadFile(svgPath)
	if err != nil {
		return err
	}
	img, err := render(b, size)
	if err != nil {
		return fmt.Errorf("%s: %w", svgPath, err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(out, buf.Bytes(), 0o644) //nolint:gosec // G306: a build-time PNG icon, public
}

// render draws svg, scaled to fill size×size, on a transparent image.
func render(svg []byte, size int) (image.Image, error) {
	icon, err := oksvg.ReadIconStream(bytes.NewReader(svg), oksvg.StrictErrorMode)
	if err != nil {
		return nil, err
	}
	if icon.ViewBox.W <= 0 || icon.ViewBox.H <= 0 {
		return nil, errors.New("not an SVG with a view box")
	}
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	icon.SetTarget(0, 0, float64(size), float64(size))
	icon.Draw(rasterx.NewDasher(size, size, rasterx.NewScannerGV(size, size, img, img.Bounds())), 1)
	return img, nil
}
