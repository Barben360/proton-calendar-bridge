package ui_test

import (
	"go/build"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// internal/ui follows the CLI's rule (it goes through internal/service,
// never the Proton adapters or the store) and holds no GUI toolkit: the
// widgets live in internal/ui/desktop.
func TestUIArch(t *testing.T) {
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
			for _, banned := range []string{module + "/internal/proton", module + "/internal/store", "github.com/ProtonMail/go-proton-api", "fyne.io"} {
				if imp == banned || strings.HasPrefix(imp, banned+"/") {
					t.Errorf("GOOS=%s: internal/ui imports %s; go through internal/service, and keep widgets in internal/ui/desktop", goos, imp)
				}
			}
		}
	}
}
