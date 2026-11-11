package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

type calendarData struct {
	Calendars []service.Calendar `json:"calendars"`
}

func (a *app) newCalendar() *cobra.Command {
	return Group("calendar", "Proton calendars", a.newCalendarList())
}

func (a *app) newCalendarList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the account's calendars",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			cals, err := svc.Calendars(cmd.Context())
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			if cals == nil {
				cals = []service.Calendar{}
			}
			return Finish(cmd, calendarData{cals}, calendarTable(cals), nil)
		},
	}
}

// calendarTable renders the calendars as aligned columns. ACCESS is rw or ro,
// DEFAULT is * for the default calendar and - otherwise.
func calendarTable(cals []service.Calendar) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tACCESS\tDEFAULT\tKEYS\tID")
	for _, c := range cals {
		access, def := "rw", "-"
		if c.ReadOnly {
			access = "ro"
		}
		if c.Default {
			def = "*"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Name, c.Type, access, def, c.Keys, c.ID)
	}
	_ = w.Flush() // a strings.Builder does not fail
	return strings.TrimSuffix(b.String(), "\n")
}
