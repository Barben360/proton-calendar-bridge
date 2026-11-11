package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
)

// Root builds a binary's root command: the persistent --json flag, usage
// errors for bad flags and unknown commands, and subs as its commands.
// Every command of the tree is a Group, or a leaf using NoArgs.
func Root(use, short string, subs ...*cobra.Command) *cobra.Command {
	root := Group(use, short, subs...)
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.PersistentFlags().Bool("json", false, "print one JSON envelope instead of text")
	root.CompletionOptions.DisableDefaultCmd = true // not part of the usage contract
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return UsageError(err) })
	return root
}

// groupKey marks the commands Group builds, which have no behaviour of their own.
const groupKey = "pcb/group"

// IsGroup reports whether c was built by Group.
func IsGroup(c *cobra.Command) bool { return c.Annotations[groupKey] == "true" }

// Group builds a command that only holds subcommands. An unknown
// subcommand is a usage error (exit 2); with none given it prints its help,
// or in JSON mode fails with a usage error so stdout stays one envelope.
func Group(use, short string, subs ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{
		Use:         use,
		Short:       short,
		Annotations: map[string]string{groupKey: "true"},
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return UsageError(fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if JSONMode(cmd) {
				return UsageError(fmt.Errorf("%q needs a command", cmd.CommandPath()))
			}
			return cmd.Help()
		},
	}
	c.AddCommand(subs...)
	return c
}

// UsageError marks err as a bad invocation (exit 2).
func UsageError(err error) error { return &exitcode.Error{Code: exitcode.Usage, Err: err} }

// NoArgs is the Args of a leaf command: positional arguments are a usage error.
func NoArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return UsageError(err)
	}
	return nil
}

// CommandName is the name envelopes carry: the command path without the
// binary, such as "probe refresh". The root itself is named by the binary.
func CommandName(cmd *cobra.Command) string {
	if !cmd.HasParent() {
		return cmd.Name()
	}
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

// OperandsKey is the annotation naming how many operands a command takes
// (set by OneOperand), so the usage contract can tell a missing or an extra one.
const OperandsKey = "pcb/operands"

// OneOperand makes c take exactly one operand, named what in its error
// messages. Any other count is a usage error (exit 2) that names the extra
// operand or the missing one.
func OneOperand(c *cobra.Command, what string) *cobra.Command {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[OperandsKey] = "1"
	c.Args = func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return UsageError(fmt.Errorf("%q needs %s", cmd.CommandPath(), what))
		case len(args) > 1:
			return UsageError(fmt.Errorf("unexpected operand %q for %q: it takes one %s", args[1], cmd.CommandPath(), what))
		}
		return nil
	}
	return c
}

type jsonKey struct{}

// JSONMode reports whether the running command was asked for --json. Run
// decides it once, from the raw arguments.
func JSONMode(cmd *cobra.Command) bool {
	if ctx := cmd.Context(); ctx != nil {
		on, _ := ctx.Value(jsonKey{}).(bool)
		return on
	}
	return false
}

// jsonRequested scans args for --json, the only way to know it when a flag
// error stops cobra before it parses the flags. It stops at "--", takes the
// last occurrence, and reads --json=VALUE as a Go boolean; an unreadable
// value counts as asking for JSON, so the caller gets an envelope.
func jsonRequested(args []string) bool {
	on := false
	for _, a := range args {
		switch {
		case a == "--":
			return on
		case a == "--json":
			on = true
		case strings.HasPrefix(a, "--json="):
			v, err := strconv.ParseBool(strings.TrimPrefix(a, "--json="))
			on = err != nil || v
		}
	}
	return on
}

// Finish prints a command's result the way its mode wants: with --json the
// one envelope (data, or the failure), else text on stdout (printed even
// when err is set, so a failing measurement keeps what it measured). It
// returns err, marked as already reported once its envelope is out.
func Finish(cmd *cobra.Command, data any, text string, err error) error {
	if JSONMode(cmd) {
		if werr := output.Write(cmd.OutOrStdout(), CommandName(cmd), data, err); werr != nil {
			return werr
		}
		if err != nil {
			return &reportedError{err}
		}
		return nil
	}
	if text != "" {
		fmt.Fprintln(cmd.OutOrStdout(), text)
	}
	return err
}

// reportedError marks a failure whose JSON envelope Finish already printed,
// so Run must not print a second one.
type reportedError struct{ error }

func (e *reportedError) Unwrap() error { return e.error }

// Run executes root with args and returns the process exit code. A failure
// is printed on stderr as text, or as the one envelope on stdout with --json.
func Run(root *cobra.Command, args []string, stdout, stderr io.Writer) exitcode.Code {
	return RunContext(context.Background(), root, args, stdout, stderr)
}

// RunContext is Run with ctx, which commands see through cmd.Context().
func RunContext(ctx context.Context, root *cobra.Command, args []string, stdout, stderr io.Writer) exitcode.Code {
	asJSON := jsonRequested(args)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteContextC(context.WithValue(ctx, jsonKey{}, asJSON))
	if err == nil {
		return exitcode.OK
	}
	var reported *reportedError
	switch {
	case errors.As(err, &reported):
	case asJSON:
		// A failed write has nowhere left to be reported.
		_ = output.Write(stdout, CommandName(cmd), nil, err)
	default:
		fmt.Fprintf(stderr, "Error: %s\n", err)
		if hint := exitcode.HintOf(err); hint != "" {
			fmt.Fprintf(stderr, "Hint: %s\n", hint)
		}
	}
	return exitcode.From(err)
}
