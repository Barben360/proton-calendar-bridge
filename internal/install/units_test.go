package install_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

// The golden files are rewritten only by `go test ./internal/install -update`.
var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// golden compares got with testdata/<name>.golden byte for byte.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from %s\n--- got ---\n%s\n--- want ---\n%s", name, p, got, want)
	}
}

func TestUserUnitGolden(t *testing.T) {
	golden(t, "pcb-user.service", install.UserUnit(install.UnitParams{
		Exec:      "/home/alice/go/bin/pcb",
		ConfigDir: "/home/alice/.config/pcb",
		StateDir:  "/home/alice/.local/state/pcb",
	}))
}

func TestSystemUnitGolden(t *testing.T) {
	golden(t, "pcb-system.service", install.SystemUnit(install.UnitParams{
		Exec:      "/usr/local/bin/pcb",
		ConfigDir: "/etc/pcb",
		StateDir:  "/var/lib/pcb",
	}))
}

func TestDefaultConfigGolden(t *testing.T) {
	golden(t, "config.toml", install.DefaultConfig())
}

func TestDefaultConfigLoadsAsDefaults(t *testing.T) {
	dir := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(dir, "config"), State: filepath.Join(dir, "state")}
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, install.DefaultConfig(), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(p, dirs)
	if err != nil {
		t.Fatal(err)
	}
	if want := config.Defaults(dirs); !reflect.DeepEqual(got, want) {
		t.Fatalf("the default config loads as %+v, want config.Defaults %+v", got, want)
	}
}
