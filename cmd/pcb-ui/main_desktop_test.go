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

// TestMain runs pcb-ui's run, with the arguments in PCB_UI_TEST_ARGS, in
// the child of TestUnusableDisplayIsAPcbMessage and
// TestVersionGoesToTheInheritedHandle (on the main thread, which GLFW needs).
func TestMain(m *testing.M) {
	if os.Getenv("PCB_UI_TEST_RUN") == "1" {
		os.Exit(run(strings.Fields(os.Getenv("PCB_UI_TEST_ARGS"))))
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
	if err := s.OpenLogs(); err != nil {
		t.Fatal(err)
	}
	if err := s.OpenConfig(); err != nil {
		t.Fatal(err)
	}
	if got := *opened; len(got) != 2 || got[0] != "/state/logs" || got[1] != "/cfg/config.toml" {
		t.Errorf("opened %q, want the logs folder then the config file", got)
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
