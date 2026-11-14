package ctl_test

import (
	"go/build"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// windowsOnly are the modules internal/ctl may import for Windows alone:
// the named pipe and the Win32 calls behind it (ruling 10).
var windowsOnly = map[string]bool{
	"github.com/Microsoft/go-winio": true,
	"golang.org/x/sys/windows":      true,
}

// internal/ctl is a thin adapter: the standard library and the service's
// plain types only, on every platform it builds for, plus the named pipe's
// libraries on Windows.
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
		var ours, pipe int
		for _, imp := range pkg.Imports {
			first, _, _ := strings.Cut(imp, "/")
			switch {
			case allowed[imp]:
				ours++
			case goos == "windows" && windowsOnly[imp]:
				pipe++
			case !strings.Contains(first, "."):
				// the standard library
			default:
				t.Errorf("GOOS=%s: internal/ctl imports %s", goos, imp)
			}
		}
		if ours == 0 {
			t.Errorf("GOOS=%s: no import of this module seen: the check would pass vacuously", goos)
		}
		if goos == "windows" && pipe != len(windowsOnly) {
			t.Errorf("GOOS=windows: %d of the pipe's %d libraries imported: the allowance is stale", pipe, len(windowsOnly))
		}
	}
}
