package caldav_test

import (
	"go/build"
	"io/fs"
	"maps"
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

// go-webdav and go-ical are test-only dependencies (ruling M3-R5): no
// package of the module imports them outside its tests.
func TestGoWebdavAndGoICalAreTestOnly(t *testing.T) {
	testOnly := []string{"github.com/emersion/go-webdav", "github.com/emersion/go-ical"}
	is := func(imp, mod string) bool { return imp == mod || strings.HasPrefix(imp, mod+"/") }
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	pkgs, usedInTests := 0, map[string]bool{}
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
		for _, mod := range testOnly {
			for _, imp := range pkg.Imports {
				if is(imp, mod) {
					t.Errorf("%s imports %s outside its tests", path, imp)
				}
			}
			for _, imp := range slices.Concat(pkg.TestImports, pkg.XTestImports) {
				usedInTests[mod] = usedInTests[mod] || is(imp, mod)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pkgs < 20 || len(usedInTests) != len(testOnly) || slices.Contains(slices.Collect(maps.Values(usedInTests)), false) {
		t.Fatalf("walked %d packages, seen in tests: %v: the check would pass vacuously", pkgs, usedInTests)
	}
}
