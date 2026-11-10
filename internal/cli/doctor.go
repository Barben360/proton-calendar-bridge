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
			dirs := paths.Resolve(os.Getenv)
			results := doctor.Run(cmd.Context(), []doctor.Check{
				doctor.PlatformCheck{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
				doctor.DirModeCheck{Label: "config dir", Path: dirs.Config, Want: 0o700},
				doctor.DirModeCheck{Label: "state dir", Path: dirs.State, Want: 0o700},
			})
			var failure error
			if doctor.Worst(results) == doctor.StatusError {
				failure = &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("doctor found errors")}
			}
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				if err := output.Write(cmd.OutOrStdout(), "doctor", doctorData{results}, failure); err != nil {
					return err
				}
				if failure != nil {
					return &reportedError{failure}
				}
				return nil
			}
			return printResults(cmd.OutOrStdout(), results, failure)
		},
	}
}

var marks = map[doctor.Status]string{
	doctor.StatusOK: "✓", doctor.StatusWarn: "!", doctor.StatusError: "✗", doctor.StatusSkip: "–",
}

func printResults(w io.Writer, results []doctor.Result, failure error) error {
	for _, r := range results {
		if _, err := fmt.Fprintf(w, "%s  %s: %s\n", marks[r.Status], r.Name, r.Message); err != nil {
			return err
		}
		if r.Fix != "" {
			if _, err := fmt.Fprintf(w, "   fix: %s\n", r.Fix); err != nil {
				return err
			}
		}
	}
	return failure
}

// reportedError marks a failure whose JSON envelope the command already
// printed, so Execute must not print a second one.
type reportedError struct{ error }

func (e *reportedError) Unwrap() error { return e.error }
