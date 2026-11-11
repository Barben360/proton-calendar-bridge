package caldav_test

import (
	"go/build"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// The adapter talks to the service only (SPEC §4): the standard library,
// internal/service and internal/exitcode, nothing else.
func TestCalDAVImportsOnlyTheServiceAndTheStandardLibrary(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pkg.Imports, module+"/internal/service") {
		t.Fatalf("imports %v lack internal/service: the check would pass vacuously", pkg.Imports)
	}
	for _, imp := range pkg.Imports {
		first, _, _ := strings.Cut(imp, "/")
		std := !strings.Contains(first, ".")
		if !std && imp != module+"/internal/service" && imp != module+"/internal/exitcode" {
			t.Errorf("internal/caldav imports %s; it may import only the standard library, internal/service and internal/exitcode", imp)
		}
	}
}

// go-webdav is a test-only dependency (ruling M3-R5): no package of the
// module imports it outside its tests.
func TestGoWebdavIsTestOnly(t *testing.T) {
	const webdav = "github.com/emersion/go-webdav"
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pkgs, usedInTests := 0, false
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "bin") {
			return filepath.SkipDir
		}
		pkg, err := build.ImportDir(path, 0)
		if err != nil {
			return nil // no Go files, or build-constrained away
		}
		pkgs++
		for _, imp := range pkg.Imports {
			if imp == webdav || strings.HasPrefix(imp, webdav+"/") {
				t.Errorf("%s imports %s outside its tests", path, imp)
			}
		}
		for _, imp := range slices.Concat(pkg.TestImports, pkg.XTestImports) {
			usedInTests = usedInTests || strings.HasPrefix(imp, webdav)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pkgs < 20 || !usedInTests {
		t.Fatalf("walked %d packages, go-webdav seen in tests: %v: the check would pass vacuously", pkgs, usedInTests)
	}
}
