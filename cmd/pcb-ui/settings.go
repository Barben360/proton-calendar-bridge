//go:build (linux || windows) && cgo

package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
	"github.com/Barben360/proton-calendar-bridge/internal/ui/desktop"
)

// settings is what the window's Settings pane asks of the application.
type settings struct {
	level      *slog.LevelVar
	logsDir    string
	configPath string
	open       func(path string) error // openPath
	exe        func() (string, error)
	updates    *ui.Updates   // the update check's switch
	poke       chan struct{} // wakes the update loop (buffered, 1)
}

var _ desktop.Settings = (*settings)(nil)

// executable is the absolute path of this binary with symlinks resolved, so
// the autostart entry survives a moved symlink farm.
func executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func (s *settings) Autostart() bool { return desktop.Autostart() }

func (s *settings) SetAutostart(on bool) error {
	exe, err := s.exe()
	if err != nil {
		return err
	}
	return desktop.SetAutostart(on, exe)
}

func (s *settings) UpdateCheck() bool { return s.updates.Enabled() }

// SetUpdateCheck flips the switch in update.json, then wakes the update
// loop so that the panel and the tray line follow at once.
func (s *settings) SetUpdateCheck(on bool) error {
	if err := s.updates.SetEnabled(context.Background(), on); err != nil {
		return err
	}
	pokeLoop(s.poke)
	return nil
}

// pokeLoop wakes the update loop without blocking (one wake is pending at
// most).
func pokeLoop(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (s *settings) Level() slog.Level     { return s.level.Level() }
func (s *settings) SetLevel(l slog.Level) { s.level.Set(l) }

func (s *settings) OpenLogs() error { return s.open(s.logsDir) }

// OpenConfig opens config.toml, made first when there is none: an empty
// file (every key keeps its default) in a private config directory.
// Opening a missing file only failed ("The system cannot find the file
// specified" on Windows, live 2026-11-15), and having no config.toml is
// the usual case.
func (s *settings) OpenConfig() error {
	if err := makeEmpty(s.configPath); err != nil {
		return err
	}
	return s.open(s.configPath)
}

// makeEmpty creates path, empty and private to the user, with its
// directory, unless it exists.
func makeEmpty(path string) error {
	if err := acl.MkdirAll(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}
