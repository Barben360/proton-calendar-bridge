package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
)

type versionData struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func newVersion() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the pcb version",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return Finish(cmd, versionData{
				Version: buildinfo.Version, Commit: buildinfo.Commit, Date: buildinfo.Date,
				Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
			}, fmt.Sprintf("pcb %s (commit %s, built %s)", buildinfo.Version, buildinfo.Commit, buildinfo.Date), nil)
		},
	}
}
