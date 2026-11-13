package daemon_test

import (
	"go/build"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// internal/daemon follows the CLI's rule: it goes through internal/service
// and never reaches into the Proton adapters or the store.
func TestDaemonArch(t *testing.T) {
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
			for _, banned := range []string{module + "/internal/proton", module + "/internal/store", "github.com/ProtonMail/go-proton-api"} {
				if imp == banned || strings.HasPrefix(imp, banned+"/") {
					t.Errorf("GOOS=%s: internal/daemon imports %s; go through internal/service", goos, imp)
				}
			}
		}
	}
}
