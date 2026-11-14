//go:build (linux || windows) && cgo

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
)

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
