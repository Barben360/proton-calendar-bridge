package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

func probeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "probe",
		Short: "Live measurements on the saved session (never log in again)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	c.PersistentFlags().String("record-dir", "", "write redacted answers here (default: none)")
	c.AddCommand(calendarAccessCmd())
	return c
}

// recorder is the --record-dir recorder, or nil.
func recorder(cmd *cobra.Command) *record.Recorder {
	if d, _ := cmd.Flags().GetString("record-dir"); d != "" {
		return &record.Recorder{Dir: d}
	}
	return nil
}

func calendarAccessCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "calendar-access",
		Short: "E03: do calendar endpoints answer with the current x-pm-appversion (PCB_LAB_APPVERSION)?",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			const name = "probe calendar-access"
			o, err := options(cmd)
			if err != nil {
				return finish(cmd, name, nil, "", err)
			}
			rec := recorder(cmd)
			o.Recorder = rec
			s, err := session.Resume(cmd.Context(), o)
			if err != nil {
				return finish(cmd, name, nil, "", err)
			}
			defer s.Close()
			app := o.AppVersion
			if app == "" {
				app = session.DefaultAppVersion
			}
			res, err := probe.CalendarAccess(cmd.Context(), s, rec, app)
			data := struct {
				AppVersion string         `json:"app_version"`
				Scopes     []string       `json:"scopes"`
				Results    []probe.Access `json:"results"`
			}{app, s.Scopes, res}
			var b strings.Builder
			fmt.Fprintf(&b, "x-pm-appversion %s; scopes %v", app, s.Scopes)
			for _, r := range res {
				if r.Skipped != "" {
					fmt.Fprintf(&b, "\n  %s: skipped (%s)", r.Path, r.Skipped)
				} else {
					fmt.Fprintf(&b, "\n  %s: HTTP %d, Proton code %d", r.Path, r.Status, r.Code)
				}
			}
			return finish(cmd, name, data, b.String(), err)
		},
	}
}
