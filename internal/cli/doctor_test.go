package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// isolate points the XDG dirs at a temp tree and returns the config dir.
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	return filepath.Join(root, "cfg", "pcb")
}

func TestDoctorExitCode(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("doctor", "--json")
	if code != exitcode.Blocked || errs != "" {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Results []struct {
				Name, Status, Fix string
			} `json:"results"`
		} `json:"data"`
		Error struct{ Code int } `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	if dec.More() {
		t.Fatalf("more than one JSON value: %q", out)
	}
	if env.OK || env.Error.Code != 20 {
		t.Fatalf("bad envelope: %+v", env)
	}
	var names, statuses []string
	for _, r := range env.Data.Results {
		names = append(names, r.Name)
		statuses = append(statuses, r.Status)
	}
	if got, want := strings.Join(names, ","), "platform,config dir,state dir"; got != want {
		t.Fatalf("names = %s, want %s", got, want)
	}
	if got, want := strings.Join(statuses, ","), "ok,error,skip"; got != want {
		// platform may be warn on an unsupported test host
		if !strings.HasSuffix(got, ",error,skip") {
			t.Fatalf("statuses = %s, want %s", got, want)
		}
	}
	if !strings.Contains(env.Data.Results[1].Fix, "chmod 700 "+cfg) {
		t.Fatalf("fix = %q", env.Data.Results[1].Fix)
	}
}

func TestDoctorTextOK(t *testing.T) {
	cfg := isolate(t)
	state := filepath.Join(filepath.Dir(filepath.Dir(cfg)), "state", "pcb")
	code, out, errs := run("doctor")
	if code != exitcode.OK || errs != "" {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout = %q, want 3 lines", out)
	}
	if want := "ok  platform: " + runtime.GOOS + "/" + runtime.GOARCH; !strings.HasPrefix(lines[0], "✓  platform: "+runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("line 0 = %q, want %q (or a warning on an unsupported host)", lines[0], want)
	}
	if want := "–  config dir: " + cfg + " does not exist yet"; lines[1] != want {
		t.Errorf("line 1 = %q, want %q", lines[1], want)
	}
	if want := "–  state dir: " + state + " does not exist yet"; lines[2] != want {
		t.Errorf("line 2 = %q, want %q", lines[2], want)
	}
}

func TestDoctorTextShowsFix(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("doctor")
	if code != exitcode.Blocked || !strings.Contains(out, "✗  config dir") || !strings.Contains(out, "   fix: chmod 700 "+cfg) {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}

func TestDoctorReportsUnresolvableDirs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	code, out, _ := run("doctor")
	if code != exitcode.Blocked || !strings.Contains(out, "✗  config dir") || !strings.Contains(out, "✗  state dir") ||
		!strings.Contains(out, "fix: set HOME or XDG_CONFIG_HOME to an absolute path") ||
		!strings.Contains(out, "fix: set HOME or XDG_STATE_HOME to an absolute path") {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}
