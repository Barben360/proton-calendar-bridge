package desktop

import (
	"go/build"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// internal/ui/desktop follows the CLI's rule (the window talks to the
// bridge through internal/ui, never the Proton adapters or the store), and
// never imports fyne.io/fyne/v2/app nor a driver: the app is passed in, so the
// package builds and tests with CGO_ENABLED=0 (main picks the driver).
func TestDesktopArch(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		ctx := build.Default
		ctx.GOOS = goos
		pkg, err := ctx.ImportDir(".", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(pkg.Imports) == 0 {
			t.Fatal("no imports found: the check would pass vacuously")
		}
		for _, imp := range pkg.Imports {
			for _, banned := range []string{module + "/internal/proton", module + "/internal/store", "github.com/ProtonMail/go-proton-api", "fyne.io/fyne/v2/app", "fyne.io/fyne/v2/driver"} {
				if imp == banned || strings.HasPrefix(imp, banned+"/") {
					t.Errorf("GOOS=%s: internal/ui/desktop imports %s", goos, imp)
				}
			}
		}
	}
}
