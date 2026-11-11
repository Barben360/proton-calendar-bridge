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
	c.AddCommand(calendarAccessCmd(), refreshCmd(), aliveCmd())
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

func refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "E01: refresh the saved session once, then retry once with the old refresh token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			const name = "probe refresh"
			o, err := options(cmd)
			if err != nil {
				return finish(cmd, name, nil, "", err)
			}
			o.Recorder = recorder(cmd)
			res, err := probe.Refresh(cmd.Context(), o)
			if err != nil {
				return finish(cmd, name, nil, "", err)
			}
			data := struct {
				RefreshStatus  int  `json:"refresh_status"`
				RefreshCode    int  `json:"refresh_code"`
				ExpiresIn      int  `json:"expires_in"`
				RefreshRotated bool `json:"refresh_rotated"`
				AccessRotated  bool `json:"access_rotated"`
				OldRetried     bool `json:"old_token_retried"`
				OldTokenWorks  bool `json:"old_token_works"`
				OldStatus      int  `json:"old_token_status"`
				OldCode        int  `json:"old_token_code"`
			}{res.First.Status, res.First.Code, res.ExpiresIn, res.RefreshRotated, res.AccessRotated,
				res.Retried, res.OldTokenWorks, res.Retry.Status, res.Retry.Code}
			text := fmt.Sprintf("refresh: HTTP %d, Proton code %d, ExpiresIn %ds, refresh token rotated %v, access token rotated %v",
				data.RefreshStatus, data.RefreshCode, data.ExpiresIn, data.RefreshRotated, data.AccessRotated)
			if res.Retried {
				text += fmt.Sprintf("\nold refresh token: HTTP %d, Proton code %d, still works %v", data.OldStatus, data.OldCode, data.OldTokenWorks)
			}
			return finish(cmd, name, data, text, nil)
		},
	}
}

func aliveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "alive",
		Short: "E01: one authenticated GET with the saved access token (no refresh), appended to e01.jsonl",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			const name = "probe alive"
			o, err := options(cmd)
			if err != nil {
				return finish(cmd, name, nil, "", err)
			}
			res, err := probe.Alive(cmd.Context(), o)
			if err != nil {
				return finish(cmd, name, nil, "", err)
			}
			text := fmt.Sprintf("%s ok=%v code=%d", res.At, res.OK, res.Code)
			if res.Over {
				text = "the 14-day window is over; nothing sampled. Stop it: systemctl --user disable --now pcb-lab-e01.timer"
			}
			return finish(cmd, name, struct {
				At   string `json:"at"`
				OK   bool   `json:"ok"`
				Code int    `json:"code"`
				Over bool   `json:"over"`
			}{res.At, res.OK, res.Code, res.Over}, text, nil)
		},
	}
}
