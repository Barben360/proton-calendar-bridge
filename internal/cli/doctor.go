package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/doctor"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

type doctorData struct {
	Results []doctor.Result `json:"results"`
}

func newDoctor() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that this machine can run pcb",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var stream func(doctor.Result)
			if !JSONMode(cmd) {
				stream = func(r doctor.Result) { printResult(cmd.OutOrStdout(), r) }
			}
			results := doctor.Run(cmd.Context(), doctorChecks(), stream)
			var failure error
			if doctor.Worst(results) == doctor.StatusError {
				failure = &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("doctor found errors")}
			}
			return Finish(cmd, doctorData{results}, "", failure)
		},
	}
}

// doctorChecks lists the checks, with one failing check per directory when
// the user directories cannot be located.
func doctorChecks() []doctor.Check {
	checks := []doctor.Check{doctor.PlatformCheck{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}}
	dirs, err := paths.Resolve(os.Getenv)
	if err != nil {
		fix := func(env string) string { return "set HOME or " + env + " to an absolute path" }
		return append(checks,
			doctor.FailedCheck{Label: "config dir", Message: err.Error(), Fix: fix("XDG_CONFIG_HOME")},
			doctor.FailedCheck{Label: "state dir", Message: err.Error(), Fix: fix("XDG_STATE_HOME")})
	}
	checks = append(checks,
		doctor.DirModeCheck{Label: "config dir", Path: dirs.Config, Want: 0o700},
		doctor.DirModeCheck{Label: "state dir", Path: dirs.State, Want: 0o700},
		doctor.ConfigCheck{Path: config.Path(dirs), Dirs: dirs})
	cfg, err := config.Load(config.Path(dirs), dirs)
	if err != nil { // ConfigCheck already reports it
		return checks
	}
	bind := &doctor.Bind{Listen: cfg.CalDAV.Listen} // resolved once for both CalDAV checks
	return append(checks,
		doctor.FileModeCheck{Label: "key file", Path: cfg.Secrets.KeyFile, Want: 0o600},
		doctor.FileModeCheck{Label: "sealed secrets", Path: filepath.Join(dirs.State, "secrets.sealed"), Want: 0o600},
		doctor.SecretsCheck{StateDir: dirs.State, KeyFile: cfg.Secrets.KeyFile},
		doctor.CacheCheck{StateDir: dirs.State},
		doctor.ListenerCheck{CalDAV: cfg.CalDAV, Bind: bind},
		doctor.CertificateCheck{CalDAV: cfg.CalDAV, TLSDir: filepath.Join(dirs.State, "tls"), Bind: bind},
		doctor.ClientsCheck{StateDir: dirs.State})
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
