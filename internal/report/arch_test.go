package report_test

import (
	"go/build"
	"strings"
	"testing"
)

// internal/report imports only the standard library and pcb's file helpers,
// on every platform: what it sends is built field by field, by itself.
func TestReportImportsOnlyTheStandardLibrary(t *testing.T) {
	allowed := map[string]bool{
		"github.com/Barben360/proton-calendar-bridge/internal/atomicfile": true,
		"github.com/Barben360/proton-calendar-bridge/internal/filelock":   true,
		"github.com/Barben360/proton-calendar-bridge/internal/acl":        true,
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
		for _, imp := range pkg.Imports {
			if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") && !allowed[imp] {
				t.Errorf("GOOS=%s: internal/report imports %s", goos, imp)
			}
		}
	}
}
