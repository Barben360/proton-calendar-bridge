// Package cli wires the pcb command tree and holds the runner, group and
// output helpers every binary of this repository (pcb, pcb-lab) shares.
package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// NewRoot builds the pcb command tree on the operating system's streams,
// environment and terminal.
func NewRoot() *cobra.Command {
	return newRoot(&app{IO: OSIO(os.Stdout, os.Stderr)})
}

func newRoot(a *app) *cobra.Command {
	root := Root("pcb", "Proton Calendar to CalDAV bridge",
		newVersion(), newDoctor(), a.newLogin(), a.newLogout(), a.newStatus(), a.newCalendar(), a.newSync(), a.newEvent())
	root.PersistentFlags().Bool("non-interactive", false, "never prompt: exit 10 naming what is missing")
	return root
}

// Execute runs pcb with args on the operating system's environment and
// terminal and returns the process exit code. SIGINT and SIGTERM cancel it.
func Execute(args []string, stdout, stderr io.Writer) exitcode.Code {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Main(ctx, args, OSIO(stdout, stderr))
}

// Main runs pcb with args on io (nil fields take the operating system's
// values) and returns the process exit code.
func Main(ctx context.Context, args []string, io IO) exitcode.Code {
	a := &app{IO: io.withDefaults()}
	return RunContext(ctx, newRoot(a), args, a.Stdout, a.Stderr)
}
