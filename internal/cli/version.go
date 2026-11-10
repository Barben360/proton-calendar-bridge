package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/buildinfo"
	"github.com/Barben360/proton-calendar-bridge/internal/output"
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
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return output.Write(cmd.OutOrStdout(), "version", versionData{
					Version: buildinfo.Version, Commit: buildinfo.Commit, Date: buildinfo.Date,
					Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
				}, nil)
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "pcb %s (commit %s, built %s)\n",
				buildinfo.Version, buildinfo.Commit, buildinfo.Date)
			return err
		},
	}
}
