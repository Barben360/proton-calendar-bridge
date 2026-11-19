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
		a.newVersion(), a.newDoctor(), a.newUpdateCheck(), a.newCrashReports(), a.newLogin(), a.newLogout(), a.newStatus(), a.newAccount(), a.newCalendar(), a.newSync(), a.newEvent(), a.newClient(), a.newMCP(), a.newServe(),
		a.newInstall(), a.newUninstall(), a.newService())
	root.PersistentFlags().Bool("non-interactive", false, "never prompt: exit 10 naming what is missing")
	addSelector(root)
	return root
}

// Execute runs pcb with args on the operating system's environment and
// terminal and returns the process exit code. SIGINT and SIGTERM cancel it.
// It alone keeps crash reports (startReports), first thing: Main, which
// tests run, never does.
func Execute(args []string, stdout, stderr io.Writer) exitcode.Code {
	a := &app{IO: OSIO(stdout, stderr).withDefaults()}
	root := newRoot(a)
	stopReports := a.startReports(root, args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := a.run(ctx, root, args)
	// Not deferred: a panic must leave the crash file for the next start.
	stopReports()
	return code
}

// Main runs pcb with args on io (nil fields take the operating system's
// values) and returns the process exit code.
func Main(ctx context.Context, args []string, io IO) exitcode.Code {
	a := &app{IO: io.withDefaults()}
	return a.run(ctx, newRoot(a), args)
}

// run executes root with args, then says once a day that crash reports
// wait (reportNoticeAfter).
func (a *app) run(ctx context.Context, root *cobra.Command, args []string) exitcode.Code {
	code := RunContext(ctx, root, args, a.Stdout, a.Stderr)
	a.reportNoticeAfter(root, args)
	return code
}
