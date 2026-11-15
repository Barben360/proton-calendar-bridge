package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
)

type versionData struct {
	Version string      `json:"version"`
	Commit  string      `json:"commit"`
	Date    string      `json:"date"`
	Go      string      `json:"go"`
	OS      string      `json:"os"`
	Arch    string      `json:"arch"`
	Update  *updateData `json:"update,omitempty"`
}

// newVersion is pcb version: offline (CI and the release run it to check
// the tag it prints), unless --check asks github.com for a newer release.
func (a *app) newVersion() *cobra.Command {
	c := &cobra.Command{
		Use:   "version",
		Short: "Print the pcb version",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := versionData{
				Version: buildinfo.Version, Commit: buildinfo.Commit, Date: buildinfo.Date,
				Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
			}
			text := fmt.Sprintf("pcb %s (commit %s, built %s)", buildinfo.Version, buildinfo.Commit, buildinfo.Date)
			if check, _ := cmd.Flags().GetBool("check"); check {
				var line string
				d.Update, line = a.checkNow(cmd.Context())
				text += "\n" + line
			}
			return Finish(cmd, d, text, nil)
		},
	}
	c.Flags().Bool("check", false, "also ask github.com whether a newer stable release exists")
	return c
}
