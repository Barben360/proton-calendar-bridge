package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

type doctorData struct {
	Results []doctor.Result `json:"results"`
}

func newDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine can run pcb",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			var stream func(doctor.Result)
			if !asJSON {
				stream = func(r doctor.Result) { printResult(cmd.OutOrStdout(), r) }
			}
			results := doctor.Run(cmd.Context(), doctorChecks(), stream)
			var failure error
			if doctor.Worst(results) == doctor.StatusError {
				failure = &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("doctor found errors")}
			}
			if asJSON {
				if err := output.Write(cmd.OutOrStdout(), "doctor", doctorData{results}, failure); err != nil {
					return err
				}
				if failure != nil {
					return &reportedError{failure}
				}
			}
			return failure
		},
	}
}

// doctorChecks lists the checks, with one failing check per directory when
// the user directories cannot be located.
func doctorChecks() []doctor.Check {
	checks := []doctor.Check{doctor.PlatformCheck{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}}
	dirs, err := paths.Resolve(os.Getenv)
	if err != nil {
		fix := "set HOME or XDG_CONFIG_HOME and XDG_STATE_HOME to absolute paths"
		return append(checks,
			doctor.FailedCheck{Label: "config dir", Message: err.Error(), Fix: fix},
			doctor.FailedCheck{Label: "state dir", Message: err.Error(), Fix: fix})
	}
	return append(checks,
		doctor.DirModeCheck{Label: "config dir", Path: dirs.Config, Want: 0o700},
		doctor.DirModeCheck{Label: "state dir", Path: dirs.State, Want: 0o700})
}

var marks = map[doctor.Status]string{
	doctor.StatusOK: "✓", doctor.StatusWarn: "!", doctor.StatusError: "✗", doctor.StatusSkip: "–",
}

// printResult writes one check's line, and its fix line. A failed write has
// nowhere left to be reported.
func printResult(w io.Writer, r doctor.Result) {
	_, _ = fmt.Fprintf(w, "%s  %s: %s\n", marks[r.Status], r.Name, r.Message)
	if r.Fix != "" {
		_, _ = fmt.Fprintf(w, "   fix: %s\n", r.Fix)
	}
}

// reportedError marks a failure whose JSON envelope the command already
// printed, so Execute must not print a second one.
type reportedError struct{ error }

func (e *reportedError) Unwrap() error { return e.error }
