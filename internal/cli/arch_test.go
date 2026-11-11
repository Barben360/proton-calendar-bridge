package cli_test

import (
	"go/build"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// internal/service is the boundary: the CLI never reaches into the Proton
// adapters or the store (SPEC §4). Test files may use protontest.
func TestCLIImportsNeitherProtonNorStore(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Imports) == 0 {
		t.Fatal("no imports found: the check would pass vacuously")
	}
	for _, imp := range pkg.Imports {
		for _, banned := range []string{module + "/internal/proton", module + "/internal/store", "github.com/ProtonMail/go-proton-api"} {
			if imp == banned || strings.HasPrefix(imp, banned+"/") {
				t.Errorf("internal/cli imports %s; go through internal/service", imp)
			}
		}
	}
}
