package logfile_test

import (
	"go/build"
	"strings"
	"testing"
)

// internal/logfile imports only the standard library, on every platform.
func TestLogfileImportsOnlyTheStandardLibrary(t *testing.T) {
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
			if first, _, _ := strings.Cut(imp, "/"); strings.Contains(first, ".") {
				t.Errorf("GOOS=%s: internal/logfile imports %s", goos, imp)
			}
		}
	}
}
