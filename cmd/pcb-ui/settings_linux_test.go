//go:build linux && cgo

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The desktop entry under XDG_CONFIG_HOME: Linux only. On Windows the same
// switch writes the Run key, tested on a test subkey in internal/ui/desktop
// (never the real Run value of the user running the tests).
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
