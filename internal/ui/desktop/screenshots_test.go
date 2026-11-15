package desktop

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/software"
	"fyne.io/fyne/v2/theme"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// fixedVariant draws the Paper theme in one variant, whatever the desktop
// asks for: the screenshots need both.
type fixedVariant struct {
	fyne.Theme
	v fyne.ThemeVariant
}

func (f fixedVariant) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	return f.Theme.Color(n, f.v)
}

// screenshotView is the made-up bridge of the README's pictures: example
// addresses, no real account, no secret.
func screenshotView() ui.View {
	synced := time.Date(2026, 11, 14, 9, 41, 0, 0, time.Local)
	return ui.View{
		Tray:     ui.TrayAttention,
		Listener: &ctl.Listener{Addr: "127.0.0.1:1080", URL: "http://127.0.0.1:1080/"},
		Accounts: []ui.AccountView{
			{
				ID: "1a2b3c4d", Address: "alice@proton.me", Short: "synced 1 min ago", Dot: ui.TrayOK,
				SignedIn: true, CalDAV: true, MCP: true, MCPMode: service.MCPReadWrite, LastSync: &synced,
				URL: "http://127.0.0.1:1080/dav/1a2b3c4d/", Username: "alice@proton.me",
				Calendars: []ui.CalendarView{
					{Name: "Personal", Color: "#3F51B5"},
					{Name: "Work", Color: "#DB3251"},
					{Name: "Holidays in France", Color: "#5ABEB4", ReadOnly: true, Info: "not a normal calendar (subscribed or holidays)"},
				},
			},
			{ID: "5e6f7a8b", Address: "team@example.org", Short: "syncing…", Dot: ui.TraySyncing, SignedIn: true, CalDAV: true, MCP: true, MCPMode: service.MCPReadOnly},
			{ID: "9c0d1e2f", Address: "bob@example.org", Short: "needs login", Dot: ui.TrayAttention, SignedIn: false, MCPMode: service.MCPOff},
		},
	}
}

// TestScreenshots renders the window as the README shows it, in the light
// and the dark variant. With PCB_SCREENSHOTS=<dir> (make screenshots) every
// picture lands there at twice the window's size. Otherwise the generator is
// still checked on every run, but on one view at 1x into a temporary
// directory: the full set costs seconds of CPU under -race, enough to starve
// the timing-sensitive login tests that CI runs next to this package.
func TestScreenshots(t *testing.T) {
	dir := os.Getenv("PCB_SCREENSHOTS")
	scale := float32(2)
	shots := []struct {
		name  string
		setup func(f *fixture)
	}{
		{"account", func(f *fixture) {
			// Taller than the default window, so the calendars show. The
			// read-only calendar's info icon shows, but the pointer stays
			// off it: its tooltip would cover the calendar above.
			f.w.win.Resize(fyne.NewSize(880, 720))
			f.w.selectAccount("1a2b3c4d")
			if tips := f.infoTips(); len(tips) != 1 {
				t.Fatalf("%d info icons in the account picture, want 1", len(tips))
			}
		}},
		{"add-account", func(f *fixture) { f.w.openWizard("carol@proton.me") }},
	}
	if dir == "" {
		dir, scale, shots = t.TempDir(), 1, shots[:1]
	}
	frame := int(2 * scale)
	variants := []struct {
		name string
		v    fyne.ThemeVariant
	}{{"light", theme.VariantLight}, {"dark", theme.VariantDark}}

	for _, s := range shots {
		var bg [2]color.Color
		for i, va := range variants {
			f := newFixture(t)
			f.app.Settings().SetTheme(fixedVariant{Theme(), va.v})
			f.w.Update(screenshotView())
			s.setup(f)
			f.wait()
			c, ok := f.w.win.Canvas().(software.WindowlessCanvas)
			if !ok {
				t.Fatalf("the test window's canvas is a %T, not a windowless one", f.w.win.Canvas())
			}
			c.SetScale(scale)
			img := framed(c.Capture(), paper[va.v][theme.ColorNameInputBorder], frame)

			size := f.w.win.Canvas().Size()
			want := image.Pt(int(size.Width*scale), int(size.Height*scale))
			if got := img.Bounds().Size(); got != want {
				t.Fatalf("%s-%s: %v pixels, want %v", s.name, va.name, got, want)
			}
			// The frame, on every edge: the window's own ground would melt
			// into a README page of the same colour.
			border := color.NRGBAModel.Convert(paper[va.v][theme.ColorNameInputBorder])
			for _, p := range []image.Point{{0, 0}, {want.X - 1, want.Y / 2}, {want.X / 2, want.Y - 1}, {frame - 1, want.Y / 2}} {
				if got := color.NRGBAModel.Convert(img.At(p.X, p.Y)); got != border {
					t.Errorf("%s-%s: pixel %v is %v, want the frame colour %v", s.name, va.name, p, got, border)
				}
			}
			// Just inside the frame: the sidebar's card, which the variant colours.
			bg[i] = color.NRGBAModel.Convert(img.At(frame+4, frame+4))
			if bg[i] != color.NRGBAModel.Convert(paper[va.v][colorCard]) {
				t.Errorf("%s-%s: corner pixel %v, want the variant's card colour %v", s.name, va.name, bg[i], paper[va.v][colorCard])
			}

			out := filepath.Join(dir, s.name+"-"+va.name+".png")
			w, err := os.Create(out)
			if err != nil {
				t.Fatal(err)
			}
			if err := png.Encode(w, img); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if bg[0] == bg[1] {
			t.Errorf("%s: the light and dark pictures have the same ground %v", s.name, bg[0])
		}
	}
}

// framed draws a border of width px and colour c over the edges of img.
func framed(img image.Image, c color.Color, px int) image.Image {
	b := img.Bounds()
	out := image.NewNRGBA(b)
	draw.Draw(out, b, img, b.Min, draw.Src)
	u := image.NewUniform(c)
	for _, r := range []image.Rectangle{
		image.Rect(b.Min.X, b.Min.Y, b.Max.X, b.Min.Y+px),
		image.Rect(b.Min.X, b.Max.Y-px, b.Max.X, b.Max.Y),
		image.Rect(b.Min.X, b.Min.Y, b.Min.X+px, b.Max.Y),
		image.Rect(b.Max.X-px, b.Min.Y, b.Max.X, b.Max.Y),
	} {
		draw.Draw(out, r, u, image.Point{}, draw.Src)
	}
	return out
}
