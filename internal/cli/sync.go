package cli

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (a *app) newSync() *cobra.Command {
	var full, watch bool
	var interval time.Duration
	c := &cobra.Command{
		Use:   "sync",
		Short: "Bring the event cache up to date with Proton",
		Long: "Sync every calendar into the local event cache: the first pass lists each calendar, later passes " +
			"read Proton's change feed. With --watch it repeats every --interval until Ctrl-C or until you must log in again.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if interval < service.MinWatchInterval {
				return &exitcode.Error{Code: exitcode.Usage,
					Err:  fmt.Errorf("--interval %s is below the minimum of %s", interval, service.MinWatchInterval),
					Hint: "use --interval " + service.MinWatchInterval.String() + " or more"}
			}
			if watch && JSONMode(cmd) {
				return &exitcode.Error{Code: exitcode.Usage,
					Err:  errors.New("--json cannot be combined with --watch: a command prints one envelope"),
					Hint: "drop --json, or run pcb sync --json without --watch"}
			}
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			req := service.SyncRequest{Full: full}
			if watch {
				err := svc.WatchSync(cmd.Context(), req, interval, func(p service.WatchPass) {
					a.printPass(cmd, p)
				})
				return Finish(cmd, nil, "", err)
			}
			res, err := svc.Sync(cmd.Context(), req)
			if res.Calendars == nil {
				res.Calendars = []service.CalendarSync{}
			}
			if err != nil && len(res.Calendars) == 0 {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, res, syncText(res, ""), err)
		},
	}
	c.Flags().BoolVar(&full, "full", false, "list every calendar in full instead of reading the change feed")
	c.Flags().BoolVar(&watch, "watch", false, "keep syncing until interrupted (not with --json)")
	c.Flags().DurationVar(&interval, "interval", service.DefaultWatchInterval, "time between passes with --watch (at least 10s)")
	return c
}

// printPass writes one watch pass: a timestamped line per calendar on
// stdout, or the failure of a pass that got nowhere on stderr with when
// the next pass runs. The failure the watch stops on (Next 0) is left to
// the final report, so it is printed once.
func (a *app) printPass(cmd *cobra.Command, p service.WatchPass) {
	stamp := time.Now().Format(time.RFC3339)
	if len(p.Result.Calendars) > 0 {
		fmt.Fprintln(cmd.OutOrStdout(), syncText(p.Result, stamp+" "))
		return
	}
	if p.Err == nil || p.Next == 0 {
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s sync failed: %s; next try in %s\n", stamp, p.Err, p.Next.Round(time.Second))
	if h := exitcode.HintOf(p.Err); h != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s hint: %s\n", stamp, h)
	}
}

// syncText is one line per calendar, each starting with prefix.
func syncText(res service.SyncResult, prefix string) string {
	lines := make([]string, 0, len(res.Calendars))
	for _, c := range res.Calendars {
		if c.Error != "" {
			lines = append(lines, fmt.Sprintf("%s%s: %s sync failed: %s (%d events cached)", prefix, c.Name, c.Mode, c.Error, c.Events))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s%s: %s sync, +%d ~%d -%d, %d events", prefix, c.Name, c.Mode, c.Added, c.Updated, c.Deleted, c.Events))
	}
	return strings.Join(lines, "\n")
}
