package config_test

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
		d.Secrets.KeyFile != "/x/cfg/pcb/secrets.key" || d.CalDAV.Listen != "127.0.0.1:1080" ||
		d.CalDAV.Hostnames != nil || d.CalDAV.TLSCert != "" || d.CalDAV.TLSKey != "" {
		t.Fatalf("defaults = %+v", d)
	}
}

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	got, err := config.Load(filepath.Join(t.TempDir(), "nope.toml"), dirs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, config.Defaults(dirs)) || got.Proton.BaseURL != config.DefaultBaseURL || got.Proton.AppVersion != config.DefaultAppVersion {
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
		CalDAV:  config.CalDAV{Listen: "127.0.0.1:1080"},
		Log:     config.Log{Level: "info"},
	}
	if !reflect.DeepEqual(got, want) {
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

func TestLoadCalDAVSection(t *testing.T) {
	p := write(t, "[caldav]\nlisten = \"0.0.0.0:8443\"\nhostnames = [\"pcb.lan\", \"192.168.1.5\"]\n"+
		"tls_cert = \"/c/cert.pem\"\ntls_key = \"/c/key.pem\"\n")
	got, err := config.Load(p, dirs)
	if err != nil {
		t.Fatal(err)
	}
	want := config.CalDAV{Listen: "0.0.0.0:8443", Hostnames: []string{"pcb.lan", "192.168.1.5"}, TLSCert: "/c/cert.pem", TLSKey: "/c/key.pem"}
	if !reflect.DeepEqual(got.CalDAV, want) {
		t.Fatalf("caldav = %+v, want %+v", got.CalDAV, want)
	}
	// Absent keys keep their defaults.
	got, err = config.Load(write(t, "[caldav]\nhostnames = [\"pcb.lan\"]\n"), dirs)
	if err != nil {
		t.Fatal(err)
	}
	if got.CalDAV.Listen != "127.0.0.1:1080" {
		t.Fatalf("listen = %q, want the default", got.CalDAV.Listen)
	}
}

func TestLoadCalDAVIsStrict(t *testing.T) {
	for body, key := range map[string]string{
		"[caldav]\nport = 1080\n":                            "caldav.port",
		"[caldav]\nlisten = 1080\n":                          "listen",
		"[caldav]\nlisten = \"1080\"\n":                      "caldav.listen",
		"[caldav]\nlisten = \":1080\"\n":                     "caldav.listen",
		"[caldav]\nlisten = \"127.0.0.1\"\n":                 "caldav.listen",
		"[caldav]\nlisten = \"127.0.0.1:65536\"\n":           "caldav.listen",
		"[caldav]\nlisten = \"127.0.0.1:-1\"\n":              "caldav.listen",
		"[caldav]\nlisten = \"127.0.0.1:http\"\n":            "caldav.listen",
		"[caldav]\nlisten = \"bad host:1080\"\n":             "caldav.listen",
		"[caldav]\nhostnames = []\n":                         "caldav.hostnames",
		"[caldav]\nhostnames = [\"\"]\n":                     "caldav.hostnames",
		"[caldav]\nhostnames = [\"a b\"]\n":                  "caldav.hostnames",
		"[caldav]\nhostnames = [\"-a.lan\"]\n":               "caldav.hostnames",
		"[caldav]\nhostnames = [\"https://a.lan\"]\n":        "caldav.hostnames",
		"[caldav]\ntls_cert = \"/c/cert.pem\"\n":             "caldav.tls_key",
		"[caldav]\ntls_key = \"/c/key.pem\"\n":               "caldav.tls_cert",
		"[caldav]\ntls_cert = \"c.pem\"\ntls_key = \"/k\"\n": "caldav.tls_cert",
		"[caldav]\ntls_cert = \"/c\"\ntls_key = \"k.pem\"\n": "caldav.tls_key",
	} {
		if msg := refused(t, body); !strings.Contains(msg, key) {
			t.Errorf("%q: error %q does not name %s", body, msg, key)
		}
	}
	for _, ok := range []string{"127.0.0.1:1080", "[::1]:1080", "0.0.0.0:443", "[::]:8443", "localhost:1080", "pcb.lan:1", "192.168.1.5:0"} {
		if _, err := config.Load(write(t, "[caldav]\nlisten = \""+ok+"\"\n"), dirs); err != nil {
			t.Errorf("listen %q refused: %v", ok, err)
		}
	}
}

func TestValidateListenIsTheConfigRule(t *testing.T) {
	for _, bad := range []string{"", "1080", ":1080", "127.0.0.1", "127.0.0.1:99999", "a b:1"} {
		if err := config.ValidateListen(bad); err == nil {
			t.Errorf("ValidateListen(%q) = nil, want an error", bad)
		}
	}
	// An empty host is not "every interface" by accident: the message says how to ask for it.
	if err := config.ValidateListen(":1080"); err == nil || !strings.Contains(err.Error(), "0.0.0.0") {
		t.Errorf("ValidateListen(:1080) = %v, want it to name 0.0.0.0", err)
	}
	for _, good := range []string{"127.0.0.1:1080", "[::]:0", "localhost:1080"} {
		if err := config.ValidateListen(good); err != nil {
			t.Errorf("ValidateListen(%q) = %v", good, err)
		}
	}
}

func TestLogLevelDefaultsToInfo(t *testing.T) {
	if got := config.Defaults(dirs).Log.Level; got != "info" {
		t.Fatalf("default level = %q", got)
	}
	cfg, err := config.Load(write(t, "[proton]\napp_version = \"x\"\n"), dirs)
	if err != nil || cfg.Log.Level != "info" {
		t.Fatalf("level = %q, %v", cfg.Log.Level, err)
	}
	lvl, err := config.ParseLevel(cfg.Log.Level)
	if err != nil || lvl != slog.LevelInfo {
		t.Fatalf("ParseLevel = %v, %v", lvl, err)
	}
	cfg, err = config.Load(write(t, "[log]\nlevel = \"warn\"\n"), dirs)
	if err != nil || cfg.Log.Level != "warn" {
		t.Fatalf("level = %q, %v", cfg.Log.Level, err)
	}
	for name, want := range map[string]slog.Level{"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := config.ParseLevel(name); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v", name, got, err)
		}
	}
}

func TestUnknownLogLevelIsRefused(t *testing.T) {
	const secret = "VerboseSecretValue"
	msg := refused(t, "[log]\nlevel = \""+secret+"\"\n")
	if !strings.Contains(msg, "log.level must be one of debug, info, warn, error") || strings.Contains(msg, secret) {
		t.Fatalf("message = %q", msg)
	}
	for _, bad := range []string{"INFO", "Warn", "", "warning", "trace"} {
		if _, err := config.ParseLevel(bad); err == nil || (bad != "" && strings.Contains(err.Error(), bad)) {
			t.Errorf("ParseLevel(%q) = %v, want a refusal that does not echo it", bad, err)
		}
	}
	refused(t, "[log]\nlevel = \"\"\n")
}
