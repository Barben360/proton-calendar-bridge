//go:build (linux || windows) && cgo

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/report"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

func TestParseArgs(t *testing.T) {
	t.Run("background", func(t *testing.T) {
		o, done, _ := parseArgs([]string{"--background"}, new(bytes.Buffer), new(bytes.Buffer))
		if done || !o.background {
			t.Fatalf("o=%+v done=%v, want background and not done", o, done)
		}
	})
	t.Run("default is not background", func(t *testing.T) {
		o, done, _ := parseArgs(nil, new(bytes.Buffer), new(bytes.Buffer))
		if done || o.background {
			t.Fatalf("o=%+v done=%v", o, done)
		}
	})
	t.Run("version", func(t *testing.T) {
		var out bytes.Buffer
		_, done, code := parseArgs([]string{"--version"}, &out, new(bytes.Buffer))
		if !done || code != 0 || !strings.Contains(out.String(), buildinfo.Version) {
			t.Fatalf("done=%v code=%d out=%q", done, code, out.String())
		}
	})
	t.Run("help exits 0 and names the flags", func(t *testing.T) {
		var errb bytes.Buffer
		_, done, code := parseArgs([]string{"--help"}, new(bytes.Buffer), &errb)
		if !done || code != 0 || !strings.Contains(errb.String(), "--background") {
			t.Fatalf("done=%v code=%d out=%q", done, code, errb.String())
		}
	})
	t.Run("unknown flag and stray argument are exit 2", func(t *testing.T) {
		for _, args := range [][]string{{"--nope"}, {"serve"}} {
			_, done, code := parseArgs(args, new(bytes.Buffer), new(bytes.Buffer))
			if !done || code != 2 {
				t.Errorf("%v: done=%v code=%d, want exit 2", args, done, code)
			}
		}
	})
}

// childHook, set by a platform's test file, runs the child of a test that
// needs the main thread (Linux: classChild, of TestWindowClassIsTheAppID).
var childHook func() int

// TestMain runs pcb-ui's run, with the arguments in PCB_UI_TEST_ARGS, in
// the child of TestUnusableDisplayIsAPcbMessage and
// TestVersionGoesToTheInheritedHandle (on the main thread, which GLFW needs).
func TestMain(m *testing.M) {
	if os.Getenv("PCB_UI_TEST_RUN") == "1" {
		os.Exit(run(strings.Fields(os.Getenv("PCB_UI_TEST_ARGS"))))
	}
	if how := os.Getenv(reportsChildEnv); how != "" {
		os.Exit(reportsChild(how))
	}
	if childHook != nil && os.Getenv("PCB_UI_TEST_CLASS") == "1" {
		os.Exit(childHook())
	}
	os.Exit(m.Run())
}

// runChild runs pcb-ui's run with args in a child whose stdout is the file
// at out (pcb-ui --version > v.txt) and whose stderr is a pipe; it returns
// the exit code and stderr. No display, config or state is reached.
func runChild(t *testing.T, args, out string) (int, string) {
	t.Helper()
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "PCB_UI_TEST_RUN=1", "PCB_UI_TEST_ARGS="+args,
		"PCB_CONFIG_DIR="+filepath.Join(tmp, "cfg"), "PCB_STATE_DIR="+filepath.Join(tmp, "state"))
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = f, &stderr
	err = cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, stderr.String()
	case errors.As(err, &ee):
		return ee.ExitCode(), stderr.String()
	default:
		t.Fatal(err)
		return 0, ""
	}
}

// pcb-ui --version redirected to a file, --help and a usage error on a
// pipe: what run prints reaches the inherited handles (on Windows, never
// the parent's console instead).
func TestVersionGoesToTheInheritedHandle(t *testing.T) {
	out := filepath.Join(t.TempDir(), "v.txt")
	code, stderr := runChild(t, "--version", out)
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := "pcb-ui " + buildinfo.Version + " (commit "; code != 0 || !strings.HasPrefix(string(b), want) || stderr != "" {
		t.Errorf("--version: exit %d, v.txt %q (want %q…), stderr %q", code, b, want, stderr)
	}
	code, stderr = runChild(t, "--help", out)
	if code != 0 || !strings.Contains(stderr, "Usage: pcb-ui") {
		t.Errorf("--help: exit %d, stderr %q", code, stderr)
	}
	code, stderr = runChild(t, "--nope", out)
	if code != 2 || !strings.Contains(stderr, "flag provided but not defined: -nope") {
		t.Errorf("--nope: exit %d, stderr %q", code, stderr)
	}
}

func newSettings(t *testing.T) (*settings, *[]string) {
	t.Helper()
	var opened []string
	return &settings{
		level:      new(slog.LevelVar),
		logsDir:    "/state/logs",
		configPath: "/cfg/config.toml",
		open:       func(p string) error { opened = append(opened, p); return nil },
		exe:        func() (string, error) { return "/opt/pcb/bin/pcb-ui", nil },
	}, &opened
}

func TestSettingsOpenFiles(t *testing.T) {
	s, opened := newSettings(t)
	s.configPath = filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(s.configPath, []byte("[log]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenLogs(); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenConfig(); err != nil {
		t.Fatal(err)
	}
	if got := *opened; len(got) != 2 || got[0] != "/state/logs" || got[1] != s.configPath {
		t.Errorf("opened %q, want the logs folder then the config file", got)
	}
	if b, _ := os.ReadFile(s.configPath); string(b) != "[log]\nlevel = \"debug\"\n" {
		t.Errorf("config.toml is now %q: an existing file must be left as it is", b)
	}
}

// With no config.toml (every key at its default, the usual case), Open
// config.toml first makes an empty one in a private config directory:
// opening a missing file failed with "The system cannot find the file
// specified" (live check on Windows, 2026-11-15).
func TestSettingsOpenConfigMakesAMissingFile(t *testing.T) {
	s, opened := newSettings(t)
	dir := filepath.Join(t.TempDir(), "cfg", "pcb")
	s.configPath = filepath.Join(dir, "config.toml")
	if err := s.OpenConfig(); err != nil {
		t.Fatal(err)
	}
	if got := *opened; len(got) != 1 || got[0] != s.configPath {
		t.Errorf("opened %q, want %q", got, s.configPath)
	}
	b, err := os.ReadFile(s.configPath)
	if err != nil || len(b) != 0 {
		t.Fatalf("config.toml: %q, %v; want an empty file", b, err)
	}
	acltest.WantPrivate(t, dir, 0o700)
	acltest.WantPrivate(t, s.configPath, 0o600)
}

func TestSettingsOpenConfigFailsWithoutOpening(t *testing.T) {
	s, opened := newSettings(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.configPath = filepath.Join(file, "pcb", "config.toml") // its directory cannot be made
	if err := s.OpenConfig(); err == nil {
		t.Fatal("no error for a config directory that cannot be made")
	}
	if len(*opened) != 0 {
		t.Errorf("opened %q after the failure", *opened)
	}
}

func TestSettingsLevelIsTheLevelVar(t *testing.T) {
	s, _ := newSettings(t)
	s.SetLevel(slog.LevelDebug)
	if s.level.Level() != slog.LevelDebug || s.Level() != slog.LevelDebug {
		t.Errorf("level not applied: %v", s.level.Level())
	}
}

func TestExecutableIsAbsoluteAndResolved(t *testing.T) {
	p, err := executable()
	if err != nil || !filepath.IsAbs(p) {
		t.Fatalf("%q, %v", p, err)
	}
	if r, _ := filepath.EvalSymlinks(p); r != p {
		t.Errorf("%q still has a symlink (%q)", p, r)
	}
}

// Without OpenGL 2.0 (a VM without 3D acceleration, a remote desktop
// session: Windows' own OpenGL is 1.1) Fyne cannot draw: exit 10, a message
// naming OpenGL 2.0 with the driver's own words, and the workaround.
func TestCheckGL(t *testing.T) {
	err := checkGL(func() error { return errors.New("WGL: The driver does not appear to support OpenGL") })
	if exitcode.From(err) != exitcode.MustAct {
		t.Errorf("exit %d, want %d", exitcode.From(err), exitcode.MustAct)
	}
	if err == nil || !strings.Contains(err.Error(), "OpenGL 2.0") || !strings.Contains(err.Error(), "WGL: The driver") {
		t.Errorf("err = %v, want OpenGL 2.0 and the driver's message", err)
	}
	for _, want := range []string{"opengl32.dll", "docs/troubleshooting.md", `"pcb-ui needs OpenGL 2.0"`} {
		if h := exitcode.HintOf(err); !strings.Contains(h, want) {
			t.Errorf("hint %q lacks %q", h, want)
		}
	}
	if err := checkGL(func() error { return nil }); err != nil {
		t.Errorf("a usable OpenGL: %v", err)
	}
}

// The log's directories are private as every directory pcb makes: mode
// 0700 on Unix; on Windows a protected access list owned by the user, not
// the state directory's inherited one (an elevated pcb-ui would otherwise
// leave <state>\logs owned by Administrators).
func TestOpenLogMakesPrivateDirectories(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	lf, err := openLog(state)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lf.Close() }()
	if _, err := lf.Write([]byte("line\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "logs", "pcb-ui.log")); err != nil {
		t.Fatalf("the log is not at <state>/logs/pcb-ui.log: %v", err)
	}
	acltest.WantPrivate(t, state, 0o700)
	acltest.WantPrivate(t, filepath.Join(state, "logs"), 0o700)
}

// The Settings switch writes update.json and wakes the update loop, so the
// panel and the tray line go at once.
func TestSettingsUpdateCheckWritesTheSwitchAndPokes(t *testing.T) {
	s, _ := newSettings(t)
	path := update.StorePath(t.TempDir())
	s.updates = &ui.Updates{Path: path, Env: func(string) string { return "" }}
	s.poke = make(chan struct{}, 1)
	if !s.UpdateCheck() {
		t.Fatal("UpdateCheck() = false on a fresh state")
	}
	if err := s.SetUpdateCheck(false); err != nil {
		t.Fatal(err)
	}
	if !update.Load(path).Off || s.UpdateCheck() {
		t.Errorf("after SetUpdateCheck(false): %+v, UpdateCheck=%v", update.Load(path), s.UpdateCheck())
	}
	select {
	case <-s.poke:
	default:
		t.Error("no poke after the switch")
	}
}

func TestSettingsCrashReportsSwitch(t *testing.T) {
	s, _ := newSettings(t)
	if on, available := s.CrashReports(); on || available {
		t.Errorf("without reports: on %v, available %v", on, available)
	}
	if err := s.SetCrashReports(true); err == nil {
		t.Error("the switch worked in a build without a report address")
	}
	store := report.Store{StateDir: t.TempDir(), Env: func(string) string { return "" }}
	s.reports = &ui.Reports{Store: store}
	if on, available := s.CrashReports(); on || !available {
		t.Errorf("mode ask: on %v, available %v", on, available)
	}
	if err := s.SetCrashReports(true); err != nil {
		t.Fatal(err)
	}
	if store.Mode() != report.Always {
		t.Errorf("mode %s after switching on, want always", store.Mode())
	}
	if on, _ := s.CrashReports(); !on {
		t.Error("the switch is off in mode always")
	}
}
