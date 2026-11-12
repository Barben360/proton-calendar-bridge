package ctl_test

import (
	"go/build"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// internal/ctl is a thin adapter: the standard library and the service's
// plain types only, on every platform it builds for.
func TestCTLImportsOnlyWhatItMay(t *testing.T) {
	allowed := map[string]bool{
		module + "/internal/service":   true,
		module + "/internal/output":    true,
		module + "/internal/exitcode":  true,
		module + "/internal/buildinfo": true,
	}
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
		var ours int
		for _, imp := range pkg.Imports {
			first, _, _ := strings.Cut(imp, "/")
			switch {
			case allowed[imp]:
				ours++
			case !strings.Contains(first, "."):
				// the standard library
			default:
				t.Errorf("GOOS=%s: internal/ctl imports %s", goos, imp)
			}
		}
		if ours == 0 {
			t.Errorf("GOOS=%s: no import of this module seen: the check would pass vacuously", goos)
		}
	}
}
