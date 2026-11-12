package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/cli/clitest"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// run is pcb on the process's environment and terminal, as cli.Execute
// runs it, but on a host with no system to touch (noSystemHost).
func run(args ...string) (exitcode.Code, string, string) {
	var out, errb bytes.Buffer
	code := cli.Main(context.Background(), args, cli.IO{Stdout: &out, Stderr: &errb, Host: noSystemHost()})
	return code, out.String(), errb.String()
}

func TestVersionText(t *testing.T) {
	code, out, errs := run("version")
	if code != exitcode.OK || errs != "" {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	if want := "pcb dev (commit dev, built dev)\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, errs := run("version", "--json")
	if code != exitcode.OK || errs != "" {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	var env struct {
		Command string            `json:"command"`
		OK      bool              `json:"ok"`
		Data    map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Command != "version" || !env.OK {
		t.Fatalf("bad envelope: %+v", env)
	}
	for _, k := range []string{"version", "commit", "date", "go", "os", "arch"} {
		if env.Data[k] == "" {
			t.Errorf("data.%s missing or empty: %v", k, env.Data)
		}
	}
	if len(env.Data) != 6 {
		t.Errorf("unexpected keys: %v", env.Data)
	}
}

func TestJSONEnvelopeOnError(t *testing.T) {
	for _, args := range [][]string{
		{"--json", "nope"},
		{"--json", "--bogus"},
		{"--bogus", "--json"},
		{"--json", "version", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, errs := run(args...)
			if code != exitcode.Usage {
				t.Fatalf("code = %d, want %d", code, exitcode.Usage)
			}
			if errs != "" {
				t.Fatalf("stderr not empty in JSON mode: %q", errs)
			}
			dec := json.NewDecoder(strings.NewReader(out))
			var env struct {
				OK    bool `json:"ok"`
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := dec.Decode(&env); err != nil {
				t.Fatal(err)
			}
			if env.OK || env.Error.Code != 2 {
				t.Fatalf("bad envelope: %+v", env)
			}
			if _, err := dec.Token(); err != io.EOF {
				t.Fatalf("more than one JSON value on stdout: %q", out)
			}
		})
	}
}

func TestTextErrorGoesToStderr(t *testing.T) {
	code, out, errs := run("nope")
	if code != exitcode.Usage || out != "" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	if !strings.HasPrefix(errs, "Error: ") {
		t.Fatalf("stderr = %q", errs)
	}
}

func TestBareRootShowsHelp(t *testing.T) {
	code, out, _ := run()
	if code != exitcode.OK || !strings.Contains(out, "version") {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}

// decodeOne decodes the single envelope in out, failing on a second value.
func decodeOne(t *testing.T, out string) (env struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("stdout is not an envelope: %v\n%q", err, out)
	}
	if _, err := dec.Token(); err != io.EOF {
		t.Fatalf("more than one JSON value on stdout: %q", out)
	}
	return env
}

func TestJSONFlagValuesAgreeOnSuccessAndFailure(t *testing.T) {
	for _, tc := range []struct {
		flag string
		json bool
	}{
		{"--json", true}, {"--json=true", true}, {"--json=1", true}, {"--json=T", true},
		{"--json=false", false}, {"--json=0", false},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			code, out, _ := run("version", tc.flag)
			if code != exitcode.OK || strings.HasPrefix(out, "{") != tc.json {
				t.Errorf("success: code=%d json=%v stdout=%q", code, tc.json, out)
			}
			code, out, errs := run("version", tc.flag, "extra")
			if code != exitcode.Usage {
				t.Fatalf("failure code = %d", code)
			}
			if tc.json {
				if env := decodeOne(t, out); env.OK || env.Error.Code != 2 || errs != "" {
					t.Errorf("envelope = %+v stderr=%q", env, errs)
				}
			}
			if got := strings.HasPrefix(out, "{"); got != tc.json {
				t.Errorf("failure printed json=%v, want %v (stdout %q, stderr %q)", got, tc.json, out, errs)
			}
		})
	}
}

func TestJSONAfterDoubleDashIsNotTheFlag(t *testing.T) {
	code, out, errs := run("version", "--", "--json")
	if code != exitcode.Usage || out != "" || errs == "" {
		t.Fatalf("code=%d stdout=%q stderr=%q: --json after -- is a positional, text mode", code, out, errs)
	}
}

func TestBareRootInJSONModeIsAUsageEnvelope(t *testing.T) {
	code, out, errs := run("--json")
	if code != exitcode.Usage || errs != "" {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	if env := decodeOne(t, out); env.OK || env.Error.Code != 2 {
		t.Fatalf("envelope = %+v", env)
	}
}

// The contract runs real commands on the operating system's environment: it
// is pointed at a temporary home so a command that works instead of failing
// can never touch (or create) the developer's own state.
func TestUsageContractOfEveryPcbCommand(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PCB_CONFIG_DIR", "")
	t.Setenv("PCB_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	clitest.CheckUsageContract(t, cli.NewRoot)
	if _, err := os.Stat(filepath.Join(home, "state", "pcb", "clients.json")); err == nil {
		t.Error("the usage contract created a client: a bad invocation was accepted")
	}
}
