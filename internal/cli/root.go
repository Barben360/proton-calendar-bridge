// Package cli wires the pcb command tree.
package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
)

// NewRoot builds the pcb command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "pcb",
		Short:         "Proton Calendar to CalDAV bridge",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError(fmt.Errorf("unknown command %q for pcb", args[0]))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.PersistentFlags().Bool("json", false, "print one JSON envelope instead of text")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	root.AddCommand(newVersion())
	return root
}

// Execute runs pcb with args and returns the process exit code. Failures
// are printed on stderr as text, or as the one envelope on stdout with --json.
func Execute(args []string, stdout, stderr io.Writer) exitcode.Code {
	root := NewRoot()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteC()
	if err == nil {
		return exitcode.OK
	}
	if wantsJSON(args) {
		// A failed write has nowhere left to be reported.
		_ = output.Write(stdout, cmd.Name(), nil, err)
	} else {
		fmt.Fprintf(stderr, "Error: %s\n", err)
		var e *exitcode.Error
		if errors.As(err, &e) && e.Hint != "" {
			fmt.Fprintf(stderr, "Hint: %s\n", e.Hint)
		}
	}
	return exitcode.From(err)
}

// wantsJSON reports whether --json was given. It reads the raw arguments
// because a flag error can stop cobra's parsing before --json is seen.
func wantsJSON(args []string) bool {
	if i := slices.Index(args, "--"); i >= 0 {
		args = args[:i]
	}
	return slices.Contains(args, "--json") || slices.Contains(args, "--json=true")
}

func usageError(err error) error { return &exitcode.Error{Code: exitcode.Usage, Err: err} }

// noArgs rejects positional arguments as a usage error.
func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return usageError(err)
	}
	return nil
}
