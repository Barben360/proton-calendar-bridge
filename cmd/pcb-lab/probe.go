package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/record"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

func probeCmd() *cobra.Command {
	c := cli.Group("probe", "Live measurements on the saved session (never log in again)",
		calendarAccessCmd(), refreshCmd(), aliveCmd())
	c.PersistentFlags().String("record-dir", "", "write redacted answers here (default: none)")
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
		Args:  cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := options(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			rec := recorder(cmd)
			o.Recorder = rec
			s, err := session.Resume(cmd.Context(), o)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
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
			if err == nil {
				var failed []string
				for _, r := range res {
					if r.Failed() {
						failed = append(failed, r.Path)
					}
				}
				if len(failed) > 0 {
					err = &exitcode.Error{
						Code: exitcode.Blocked,
						Err:  fmt.Errorf("no calendar access with x-pm-appversion %s: failed %s", app, strings.Join(failed, ", ")),
					}
				}
			}
			return cli.Finish(cmd, data, b.String(), err)
		},
	}
}

func refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "E01: refresh the saved session once, then retry once with the old refresh token",
		Args:  cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := options(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			o.Recorder = recorder(cmd)
			res, err := probe.Refresh(cmd.Context(), o)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			if res.First.Status != http.StatusOK {
				return cli.Finish(cmd, nil, "", &exitcode.Error{
					Code: exitcode.Blocked,
					Err:  fmt.Errorf("refresh refused: HTTP %d, Proton code %d", res.First.Status, res.First.Code),
					Hint: "stop here; the session needs pcb-lab login (guard and E02 protocol apply)",
				})
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
			return cli.Finish(cmd, data, text, nil)
		},
	}
}

func aliveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "alive",
		Short: "E01: one authenticated GET with the saved access token (no refresh), appended to e01.jsonl",
		Args:  cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o, err := options(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			res, err := probe.Alive(cmd.Context(), o)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			text := fmt.Sprintf("%s ok=%v code=%d refreshed=%v rotated=%v expires_in=%d", res.At, res.OK, res.Code, res.Refreshed, res.Rotated, res.ExpiresIn)
			if res.Over {
				text = "the 14-day window is over; nothing sampled. Stop it: systemctl --user disable --now pcb-lab-e01.timer"
			}
			return cli.Finish(cmd, struct {
				probe.AliveResult
				Over bool `json:"over"`
			}{res, res.Over}, text, nil)
		},
	}
}
