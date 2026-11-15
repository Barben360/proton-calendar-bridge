//go:build windows

package cli_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// On Windows, a supported platform since the live check (spec/experiments/
// W1-windows-live.md): no state yet is exit 0, a ✓ platform line and
// Windows' own lines. An elevated run (CI's runner) gets the elevated
// warning and its fix, a normal one the ok line; zone and Start at login
// depend on the machine: only their presence is checked.
func TestDoctorTextOKOnWindows(t *testing.T) {
	cfg := isolate(t)
	state := filepath.Join(filepath.Dir(filepath.Dir(cfg)), "state", "pcb")
	code, out, errs := run("doctor")
	if code != exitcode.OK || errs != "" {
		t.Fatalf("code=%d stderr=%q\n%s", code, errs, out)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	want := []string{
		"✓  platform: windows/" + runtime.GOARCH,
		"–  config dir: " + cfg + " does not exist yet",
		"–  state dir: " + state + " does not exist yet",
		"✓  config file: defaults, no file",
		"✓  sealer: not chosen yet: the first login picks it ([secrets] backend = auto)",
		"–  dpapi key: no key yet: the first login creates it",
		"✓  caldav listener: 127.0.0.1:1080, plain HTTP (loopback only)",
		"–  tls certificate: plain HTTP on loopback: no certificate needed",
		"!  accounts: no account: nothing can sign in to pcb serve",
		"   fix: pcb login (it creates the client default), then pcb client show",
		"✓  elevated: not elevated",
		"time zone: ",
		"start at login: ",
		"–  service: no service on Windows: pcb-ui's Start at login runs it",
		"–  daemon: not running: offline checks only",
	}
	if acl.Elevated() {
		i := 10 // the elevated line
		want = append(want[:i], append([]string{
			"!  elevated: pcb runs elevated (as administrator): the files it creates are owned by Administrators",
			"   fix: run pcb from a normal (not elevated) terminal",
		}, want[i+1:]...)...)
	}
	if runtime.GOARCH != "amd64" {
		want[0] = "!  platform: windows/" + runtime.GOARCH
	}
	if len(lines) != len(want) {
		t.Fatalf("stdout = %q, want %d lines (elevated: %v)", out, len(want), acl.Elevated())
	}
	for i, w := range want {
		if strings.HasSuffix(w, ": ") { // a machine-dependent line: its name only
			if _, rest, _ := strings.Cut(lines[i], "  "); !strings.HasPrefix(rest, w) {
				t.Errorf("line %d = %q, want the %q line", i, lines[i], strings.TrimSuffix(w, ": "))
			}
			continue
		}
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i, lines[i], w)
		}
	}
}
