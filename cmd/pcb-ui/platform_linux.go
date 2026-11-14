//go:build linux && cgo

package main

import (
	"net/url"
	"os"
)

// preflight checks the display before any state is opened.
func preflight() error { return checkDisplay(os.Getenv, openDisplay) }

// showPreflightError prints a failed preflight on stderr: a pcb-ui started
// without a display has nothing else to show it with.
func showPreflightError(err error) { printError(os.Stderr, err) }

// attachConsole does nothing: a Linux binary has its terminal already.
func attachConsole() {}

// watchSessionEnd does nothing: at logout the session sends SIGTERM, which
// run handles.
func watchSessionEnd(func()) (stop func(), err error) { return func() {}, nil }

// openPath opens a file or a folder ("Open logs", "Open config") through
// openURL (fyne.App.OpenURL, xdg-open) as a file:// URL.
func openPath(openURL func(*url.URL) error) func(string) error {
	return func(p string) error { return openURL(fileURL(p)) }
}

func fileURL(path string) *url.URL { return &url.URL{Scheme: "file", Path: path} }
