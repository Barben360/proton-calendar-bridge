package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

func run(args ...string) (exitcode.Code, string, string) {
	var out, errb bytes.Buffer
	code := cli.Execute(args, &out, &errb)
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
