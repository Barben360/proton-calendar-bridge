//go:build linux && cgo

package main

import (
	"log/slog"
	"net/url"
	"os"
	"path/filepath"

	"github.com/Barben360/proton-calendar-bridge/internal/ui/desktop"
)

// settings is what the window's Settings pane asks of the application.
type settings struct {
	level      *slog.LevelVar
	logsDir    string
	configPath string
	open       func(*url.URL) error // fyne.App.OpenURL
	exe        func() (string, error)
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

func (s *settings) Level() slog.Level     { return s.level.Level() }
func (s *settings) SetLevel(l slog.Level) { s.level.Set(l) }

func (s *settings) OpenLogs() error   { return s.open(fileURL(s.logsDir)) }
func (s *settings) OpenConfig() error { return s.open(fileURL(s.configPath)) }

func fileURL(path string) *url.URL { return &url.URL{Scheme: "file", Path: path} }
