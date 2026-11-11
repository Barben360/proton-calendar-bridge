package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

type eventData struct {
	Events []service.Event `json:"events"`
}

func (a *app) newEvent() *cobra.Command {
	return Group("event", "Cached events", a.newEventList())
}

func (a *app) newEventList() *cobra.Command {
	var calendar, from, to string
	c := &cobra.Command{
		Use:   "list",
		Short: "List a calendar's cached events (no network call)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(calendar) == "" {
				return &exitcode.Error{Code: exitcode.Usage, Err: errors.New("--calendar is required"),
					Hint: "pass the calendar's ID or exact name (pcb calendar list)"}
			}
			req := service.EventsRequest{Calendar: calendar}
			var err error
			if req.From, err = parseWhen("--from", from); err != nil {
				return err
			}
			if req.To, err = parseWhen("--to", to); err != nil {
				return err
			}
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			evs, err := svc.Events(cmd.Context(), req)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			if evs == nil {
				evs = []service.Event{}
			}
			return Finish(cmd, eventData{evs}, eventTable(evs), nil)
		},
	}
	c.Flags().StringVar(&calendar, "calendar", "", "the calendar's ID or exact name (required)")
	c.Flags().StringVar(&from, "from", "", "only events ending after this time: RFC 3339, or YYYY-MM-DD for local midnight")
	c.Flags().StringVar(&to, "to", "", "only events starting before this time (recurring events always kept): RFC 3339, or YYYY-MM-DD for local midnight")
	return c
}

// parseWhen reads an RFC 3339 time or a YYYY-MM-DD date (midnight in the
// local time zone); "" is no bound.
func parseWhen(flag, v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t, err = time.ParseInLocation(time.DateOnly, v, time.Local)
	}
	if err != nil {
		return nil, &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("%s %q is neither RFC 3339 nor YYYY-MM-DD", flag, v),
			Hint: "write 2026-11-11T09:00:00Z, or 2026-11-11 for local midnight"}
	}
	return &t, nil
}

// eventTable renders the events as aligned columns, times in the local zone.
func eventTable(evs []service.Event) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "START\tEND\tALL-DAY\tRECURRING\tID")
	yn := func(v bool) string {
		if v {
			return "yes"
		}
		return "no"
	}
	for _, e := range evs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Start.Local().Format(time.RFC3339), e.End.Local().Format(time.RFC3339), yn(e.AllDay), yn(e.Recurring), e.ID)
	}
	_ = w.Flush() // a strings.Builder does not fail
	return strings.TrimSuffix(b.String(), "\n")
}
