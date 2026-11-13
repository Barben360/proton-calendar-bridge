//go:build linux && cgo

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
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

func TestCheckDisplay(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if err := checkDisplay(env(nil)); err == nil || err.Error() != noDisplayLine {
		t.Errorf("no display: err=%v", err)
	}
	for _, k := range []string{"DISPLAY", "WAYLAND_DISPLAY"} {
		if err := checkDisplay(env(map[string]string{k: ":0"})); err != nil {
			t.Errorf("%s set: %v", k, err)
		}
	}
}

func newSettings(t *testing.T) (*settings, *[]*url.URL) {
	t.Helper()
	var opened []*url.URL
	return &settings{
		level:      new(slog.LevelVar),
		logsDir:    "/state/logs",
		configPath: "/cfg/config.toml",
		open:       func(u *url.URL) error { opened = append(opened, u); return nil },
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
	got := []string{(*opened)[0].String(), (*opened)[1].String()}
	if got[0] != "file:///state/logs" || got[1] != "file:///cfg/config.toml" {
		t.Errorf("opened %v", got)
	}
}

func TestSettingsLevelIsTheLevelVar(t *testing.T) {
	s, _ := newSettings(t)
	s.SetLevel(slog.LevelDebug)
	if s.level.Level() != slog.LevelDebug || s.Level() != slog.LevelDebug {
		t.Errorf("level not applied: %v", s.level.Level())
	}
}

func TestSettingsAutostartWritesTheResolvedExecutable(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	s, _ := newSettings(t)
	if s.Autostart() {
		t.Fatal("autostart on in a fresh config dir")
	}
	if err := s.SetAutostart(true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(cfg, "autostart", "pcb-ui.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "Exec=/opt/pcb/bin/pcb-ui --background\n") || !s.Autostart() {
		t.Errorf("entry %q, Autostart=%v", b, s.Autostart())
	}
	if err := s.SetAutostart(false); err != nil {
		t.Fatal(err)
	}
	if s.Autostart() {
		t.Error("still on after SetAutostart(false)")
	}
}

func TestSettingsAutostartFailsWithoutExecutable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, _ := newSettings(t)
	s.exe = func() (string, error) { return "", errors.New("no exe") }
	if err := s.SetAutostart(true); err == nil || s.Autostart() {
		t.Errorf("err=%v autostart=%v, want an error and no entry", err, s.Autostart())
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

func TestFileURLEscapesSpacesAndSpecials(t *testing.T) {
	u := fileURL("/home/my user/.local/state/pcb #1/logs")
	if got, want := u.String(), "file:///home/my%20user/.local/state/pcb%20%231/logs"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	back, err := url.Parse(u.String())
	if err != nil || back.Path != "/home/my user/.local/state/pcb #1/logs" {
		t.Errorf("round trip: %q, %v", back.Path, err)
	}
}
