package seal_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

func TestDPAPIName(t *testing.T) {
	if got := (&seal.DPAPI{}).Name(); got != "dpapi" {
		t.Fatalf("Name = %q, want dpapi", got)
	}
	if seal.DPAPIKeyName != "secrets.key.dpapi" {
		t.Fatalf("DPAPIKeyName = %q", seal.DPAPIKeyName)
	}
}

// Off Windows there is no DPAPI: every use fails with ErrNoDPAPI and
// nothing is written, not even the key's directory.
func TestDPAPIOffWindowsFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("DPAPI works here: TestDPAPIRoundTrip")
	}
	dir := filepath.Join(t.TempDir(), "state")
	d := &seal.DPAPI{Path: filepath.Join(dir, seal.DPAPIKeyName)}
	if _, err := d.Seal([]byte("x"), nil); !errors.Is(err, seal.ErrNoDPAPI) {
		t.Fatalf("Seal = %v, want ErrNoDPAPI", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Seal created %s (%v)", dir, err)
	}
	// A key copied from Windows is not opened either.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.Path, make([]byte, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Open(make([]byte, 64), nil); !errors.Is(err, seal.ErrNoDPAPI) || errors.Is(err, seal.ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrNoDPAPI (not ErrCorrupt)", err)
	}
	if err := d.Check(); !errors.Is(err, seal.ErrNoDPAPI) {
		t.Fatalf("Check = %v, want ErrNoDPAPI", err)
	}
}
