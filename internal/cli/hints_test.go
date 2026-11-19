package cli_test

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// hintEnvelope decodes the failure envelope of out, hint included.
func hintEnvelope(t *testing.T, out string) (message, hint string) {
	t.Helper()
	var e struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("stdout is not an envelope: %v\n%q", err, out)
	}
	if e.OK || e.Error.Code != 2 {
		t.Fatalf("envelope = %+v, want a usage failure (2)", e)
	}
	return e.Error.Message, e.Error.Hint
}

func TestUnknownSubcommandHintsTheNextStep(t *testing.T) {
	isolate(t)
	for _, c := range []struct {
		args       []string
		help, subs string
	}{
		{[]string{"--json", "event", "get"}, "pcb event --help", "list"},
		{[]string{"--json", "client", "frobnicate"}, "pcb client --help", "add, list, remove, rotate, show"},
		{[]string{"--json", "frobnicate"}, "pcb --help", "login"},
	} {
		code, out, errs := run(c.args...)
		if code != exitcode.Usage || errs != "" {
			t.Fatalf("%v: code=%d stderr=%q", c.args, code, errs)
		}
		msg, hint := hintEnvelope(t, out)
		if !strings.Contains(msg, "unknown command") {
			t.Errorf("%v: message %q", c.args, msg)
		}
		if !strings.Contains(hint, "run "+c.help) || !strings.Contains(hint, c.subs) {
			t.Errorf("%v: hint %q, want %q and the subcommands %q", c.args, hint, "run "+c.help, c.subs)
		}
	}
	// Without --json the hint is on stderr, as for every failure.
	code, out, errs := run("event", "get")
	if code != exitcode.Usage || out != "" || !strings.Contains(errs, "Hint: run pcb event --help") {
		t.Errorf("text mode: code=%d stdout=%q stderr=%q", code, out, errs)
	}
}

func TestGroupWithoutACommandHintsTheNextStep(t *testing.T) {
	isolate(t)
	code, out, _ := run("--json", "event")
	if code != exitcode.Usage {
		t.Fatalf("code=%d", code)
	}
	if msg, hint := hintEnvelope(t, out); !strings.Contains(msg, "needs a command") || !strings.Contains(hint, "run pcb event --help") {
		t.Errorf("message %q, hint %q", msg, hint)
	}
}

// --help is the one output --json does not wrap: usage text on stdout, exit
// 0 (docs/commands.md, CONTRIBUTING.md).
func TestHelpStaysTextWithJSON(t *testing.T) {
	for _, args := range [][]string{{"--json", "--help"}, {"--json", "help"}, {"--json", "event", "--help"}} {
		code, out, errs := run(args...)
		if code != exitcode.OK || errs != "" || !strings.Contains(out, "Usage:") || json.Valid([]byte(out)) {
			t.Errorf("pcb %v: code=%d stdout=%q stderr=%q, want usage text and exit 0", args, code, out, errs)
		}
	}
}

// pcb doctor tries to bind the CalDAV port when no daemon answers: a port
// another program holds is an error that names the port.
func TestDoctorErrorsOnATakenCalDAVPort(t *testing.T) {
	cfg := isolate(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "[caldav]\nlisten = \"" + ln.Addr().String() + "\"\n"
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("doctor", "--json")
	if code != exitcode.Blocked { // a taken port may free itself: exit 20, as pcb serve's
		t.Fatalf("code=%d, want 20; stdout=%s", code, out)
	}
	var env struct {
		Data struct {
			Results []struct{ Name, Status, Message, Fix string } `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	for _, r := range env.Data.Results {
		if r.Name == "caldav listener" {
			if r.Status != "error" || !strings.Contains(r.Message, "port "+port) || !strings.Contains(r.Fix, "[caldav] listen") {
				t.Errorf("caldav listener = %+v", r)
			}
			return
		}
	}
	t.Fatalf("no caldav listener result in %s", out)
}

func TestConfigWithPortZeroIsRefused(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("[caldav]\nlisten = \"127.0.0.1:0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("doctor", "--json")
	if code != exitcode.MustAct || !strings.Contains(out, "port 0") {
		t.Errorf("doctor with port 0 in config.toml: code=%d stdout=%s", code, out)
	}
}

// With no daemon there is no listener: pcb status --json omits the key, as
// docs/commands.md says.
func TestStatusJSONHasNoListenerWithoutADaemon(t *testing.T) {
	isolate(t)
	code, out, _ := run("--json", "status")
	if code != exitcode.OK {
		t.Fatalf("code=%d stdout=%s", code, out)
	}
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.Data["listener"]; ok {
		t.Errorf("offline status has a listener: %s", out)
	}
	for _, key := range []string{"daemon", "accounts", "alerts"} {
		if _, ok := env.Data[key]; !ok {
			t.Errorf("offline status lacks %q: %s", key, out)
		}
	}
}
