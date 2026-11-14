package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pcb is a pure-Go static binary: the desktop app's GUI toolkit (cgo, GL)
// must never reach it, however an internal package is wired.
func TestPcbHasNoGUIDependency(t *testing.T) {
	deps := exec.Command("go", "list", "-deps", ".")
	deps.Env = append(os.Environ(), "CGO_ENABLED=1") // the larger set: cgo-only imports count too
	out, err := deps.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	lines := strings.Fields(string(out))
	if len(lines) < 50 {
		t.Fatalf("go list -deps returned %d packages: the check would pass vacuously", len(lines))
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "fyne.io/") {
			t.Errorf("pcb depends on %s", l)
		}
	}

	if testing.Short() {
		t.Skip("short: skipping the CGO_ENABLED=0 build")
	}
	b := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "pcb"), ".")
	b.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("CGO_ENABLED=0 go build: %v\n%s", err, out)
	}
}
