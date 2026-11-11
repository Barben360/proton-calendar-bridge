package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

var dirs = paths.Dirs{Config: "/x/cfg/pcb", State: "/x/state/pcb"}

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// refused loads body and requires a MustAct error with a "fix <path>" hint.
func refused(t *testing.T, body string) string {
	t.Helper()
	p := write(t, body)
	_, err := config.Load(p, dirs)
	if err == nil {
		t.Fatalf("%q: want an error", body)
	}
	if exitcode.From(err) != exitcode.MustAct || exitcode.HintOf(err) != "fix "+p {
		t.Fatalf("%q: code=%d hint=%q", body, exitcode.From(err), exitcode.HintOf(err))
	}
	return err.Error()
}

func TestPathAndDefaults(t *testing.T) {
	if got := config.Path(dirs); got != "/x/cfg/pcb/config.toml" {
		t.Fatalf("path = %q", got)
	}
	d := config.Defaults(dirs)
	if d.Proton.BaseURL != "https://mail-api.proton.me" || d.Proton.AppVersion != "Other" ||
		d.Secrets.KeyFile != "/x/cfg/pcb/secrets.key" {
		t.Fatalf("defaults = %+v", d)
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "nope.toml"), dirs)
	if err != nil {
		t.Fatal(err)
	}
	if got != config.Defaults(dirs) || got.Proton.BaseURL != config.DefaultBaseURL || got.Proton.AppVersion != config.DefaultAppVersion {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadOverridesEachKey(t *testing.T) {
	p := write(t, "[proton]\nbase_url = \"https://example.org\"\napp_version = \"web-calendar@1.2.3\"\n[secrets]\nkey_file = \"/k/my.key\"\n")
	got, err := config.Load(p, dirs)
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		Proton:  config.Proton{BaseURL: "https://example.org", AppVersion: "web-calendar@1.2.3"},
		Secrets: config.Secrets{KeyFile: "/k/my.key"},
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadKeepsDefaultsForAbsentKeys(t *testing.T) {
	got, err := config.Load(write(t, "[proton]\napp_version = \"x\"\n"), dirs)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proton.BaseURL != config.DefaultBaseURL || got.Secrets.KeyFile != "/x/cfg/pcb/secrets.key" || got.Proton.AppVersion != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestLoadRefusesUnknownKey(t *testing.T) {
	msg := refused(t, "[proton]\nappversion = \"x\"\n")
	if !strings.Contains(msg, "appversion") {
		t.Fatalf("error does not name the key: %q", msg)
	}
	// A key in the wrong table is unknown there too.
	if msg := refused(t, "[secrets]\napp_version = \"x\"\n"); !strings.Contains(msg, "app_version") {
		t.Fatalf("error does not name the key: %q", msg)
	}
	if msg := refused(t, "[proton]\nbase_url = \"https://a.b\"\n[bridge]\nport = 1\n"); !strings.Contains(msg, "bridge") {
		t.Fatalf("error does not name the table: %q", msg)
	}
}

func TestLoadRefusesMalformedTOMLWithoutEchoingValues(t *testing.T) {
	msg := refused(t, "[proton]\napp_version = \"hunter2-secret\n")
	if strings.Contains(msg, "hunter2") {
		t.Fatalf("error echoes a value: %q", msg)
	}
	if !strings.Contains(msg, "line 2") {
		t.Fatalf("error does not locate the fault: %q", msg)
	}
}

func TestLoadRefusesWrongType(t *testing.T) {
	msg := refused(t, "[proton]\napp_version = 3\n")
	if !strings.Contains(msg, "app_version") {
		t.Fatalf("error does not name the key: %q", msg)
	}
}

func TestLoadRefusesEmptyAppVersion(t *testing.T) {
	if msg := refused(t, "[proton]\napp_version = \"\"\n"); !strings.Contains(msg, "app_version") {
		t.Fatalf("error does not name the key: %q", msg)
	}
	refused(t, "[proton]\napp_version = \"a\\nb\"\n")
	refused(t, "[proton]\napp_version = \"a\\u007fb\"\n")
}

func TestLoadRefusesPlainHTTPRemote(t *testing.T) {
	for _, u := range []string{"http://example.com", "ftp://example.com", "example.com", "https://", "https://a b", ""} {
		if msg := refused(t, "[proton]\nbase_url = \""+u+"\"\n"); !strings.Contains(msg, "base_url") {
			t.Errorf("%q: error does not name the key: %q", u, msg)
		}
	}
	for _, u := range []string{"http://127.0.0.1:8080", "http://localhost:9", "http://[::1]:8080", "https://mail-api.proton.me"} {
		if _, err := config.Load(write(t, "[proton]\nbase_url = \""+u+"\"\n"), dirs); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
}

func TestLoadRefusesUserinfoInBaseURL(t *testing.T) {
	msg := refused(t, "[proton]\nbase_url = \"https://user:pw-secret@example.org\"\n")
	if strings.Contains(msg, "pw-secret") {
		t.Fatalf("error echoes the credential: %q", msg)
	}
}

func TestLoadRefusesRelativeKeyFile(t *testing.T) {
	for _, v := range []string{"secrets.key", "../k", ""} {
		if msg := refused(t, "[secrets]\nkey_file = \""+v+"\"\n"); !strings.Contains(msg, "key_file") {
			t.Errorf("%q: error does not name the key: %q", v, msg)
		}
	}
}

func TestLoadUnreadableFileIsAnError(t *testing.T) {
	dir := t.TempDir() // a directory in place of the file
	_, err := config.Load(dir, dirs)
	var e *exitcode.Error
	if !errors.As(err, &e) || e.Code != exitcode.MustAct {
		t.Fatalf("got %v", err)
	}
}
