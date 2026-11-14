//go:build linux && cgo

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// noDisplayLine is what pcb-ui says when no graphical session is there:
// GLFW would abort without a readable message.
const noDisplayLine = "pcb-ui needs a graphical session (no DISPLAY or WAYLAND_DISPLAY)"

// displayHint is what to do without a usable display.
const displayHint = "run pcb-ui from the desktop session (its terminal, or the applications menu); on a headless host run pcb serve instead"

type options struct {
	background bool
}

// parseArgs reads the command line. done is true when the run is over
// (--version, --help or a bad flag) with exit code code.
func parseArgs(args []string, stdout, stderr io.Writer) (o options, done bool, code int) {
	fs := flag.NewFlagSet("pcb-ui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage: pcb-ui [--background] [--version]\n\n"+
			"The desktop app of pcb: the bridge runs inside it, with a tray icon.\n\n")
		fs.PrintDefaults()
	}
	fs.BoolVar(&o.background, "background", false, "start in the tray only (the window opens when no tray host answers)")
	version := fs.Bool("version", false, "print the version and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, true, 0
		}
		return o, true, 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "pcb-ui: unexpected argument %q\n", fs.Arg(0))
		fs.Usage()
		return o, true, 2
	}
	if *version {
		fmt.Fprintf(stdout, "pcb-ui %s (commit %s, built %s)\n", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
		return o, true, 0
	}
	return o, false, 0
}

// checkDisplay refuses to start without a graphical session, or with one
// open cannot open (live 2026-11-13, F8: DISPLAY set over SSH without the X
// authority; Fyne printed its raw GLFW error, then panicked): exit 10, the
// person must start it elsewhere. It runs before any state is opened.
func checkDisplay(env func(string) string, open func() error) error {
	if env("DISPLAY") == "" && env("WAYLAND_DISPLAY") == "" {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New(noDisplayLine), Hint: displayHint}
	}
	if err := open(); err != nil {
		var which []string
		for _, k := range []string{"WAYLAND_DISPLAY", "DISPLAY"} {
			if v := env(k); v != "" {
				which = append(which, k+"="+v)
			}
		}
		return &exitcode.Error{Code: exitcode.MustAct,
			Err:  fmt.Errorf("pcb-ui cannot open the display (%s): %w", strings.Join(which, ", "), err),
			Hint: displayHint}
	}
	return nil
}
