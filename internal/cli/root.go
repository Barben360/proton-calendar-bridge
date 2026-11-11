// Package cli wires the pcb command tree and holds the runner, group and
// output helpers every binary of this repository (pcb, pcb-lab) shares.
package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// NewRoot builds the pcb command tree.
func NewRoot() *cobra.Command {
	return Root("pcb", "Proton Calendar to CalDAV bridge", newVersion(), newDoctor())
}

// Execute runs pcb with args and returns the process exit code.
func Execute(args []string, stdout, stderr io.Writer) exitcode.Code {
	return Run(NewRoot(), args, stdout, stderr)
}
