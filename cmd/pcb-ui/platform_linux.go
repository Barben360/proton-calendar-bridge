//go:build linux && cgo

package main

import "os"

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
