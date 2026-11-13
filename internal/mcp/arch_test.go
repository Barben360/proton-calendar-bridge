package mcp_test

import (
	"go/build"
	"strings"
	"testing"
)

const module = "github.com/Barben360/proton-calendar-bridge"

// internal/mcp is an adapter on the service (Global Constraints): the
// standard library, the service, exit codes, the UI's wizard, the token
// types and the MCP SDK; never Proton, the store, the iCalendar codec or the
// CLI. Test files are not checked (sdk_e19_test.go).
func TestMCPArch(t *testing.T) {
	allowed := map[string]bool{
		module + "/internal/service":  true,
		module + "/internal/exitcode": true,
		module + "/internal/ui":       true,
		module + "/internal/access":   true,
	}
	const sdk = "github.com/modelcontextprotocol/go-sdk/"
	for _, goos := range []string{"linux", "darwin", "windows"} {
		ctx := build.Default
		ctx.GOOS = goos
		pkg, err := ctx.ImportDir(".", 0)
		if err != nil {
			t.Fatal(err)
		}
		var ours, sdkSeen int
		for _, imp := range pkg.Imports {
			first, _, _ := strings.Cut(imp, "/")
			switch {
			case allowed[imp]:
				ours++
			case strings.HasPrefix(imp, sdk):
				sdkSeen++
			case !strings.Contains(first, "."):
				// the standard library
			default:
				t.Errorf("GOOS=%s: internal/mcp imports %s", goos, imp)
			}
		}
		if ours == 0 || sdkSeen == 0 {
			t.Errorf("GOOS=%s: %d imports of this module, %d of the SDK: the check would pass vacuously", goos, ours, sdkSeen)
		}
	}
}
