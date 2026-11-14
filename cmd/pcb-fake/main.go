// Command pcb-fake serves the fake Proton of internal/proton/protontest on
// the loopback interface, seeded with the test data set, so the real pcb
// binary can be rehearsed end to end offline (scripts/live/rehearse.sh).
// Developer tooling: never built into the image, never installed by make
// install, and it never contacts Proton.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(int(code))
}

func newRoot() *cobra.Command {
	return cli.Root("pcb-fake", "Fake Proton on the loopback interface, for rehearsals (developer only, never shipped)", serveCmd())
}

func execute(ctx context.Context, args []string, stdout, stderr io.Writer) exitcode.Code {
	return cli.RunContext(ctx, newRoot(), args, stdout, stderr)
}
