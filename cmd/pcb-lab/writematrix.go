package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

func writeMatrixCmd() *cobra.Command {
	var calendarID string
	var keep bool
	var interval time.Duration
	c := &cobra.Command{
		Use:   "write-matrix",
		Short: "L.4 #3/#5/#6/#8/#10, R4: write what pcb never sends into a pcb-lab calendar and report what Proton did",
		Long: "Writes, straight to the sync endpoint and without pcb's refusals, one case per line: new key packets on an\n" +
			"update, a second master of a UID, RRULEs beyond pcb's limits, STATUS:TENTATIVE, a floating time, a TZID\n" +
			"outside the tz database, DURATION, an unbumped time edit, an update without Notifications, an update that\n" +
			"leaves a personal part out. Each line is Proton's answer and what the rows read back show against what was\n" +
			"sent. Only into --calendar, whose name must start with \"pcb-lab\"; every UID starts with pcb-lab-wm-<run>-;\n" +
			"everything written is deleted at the end unless --keep. The password (PCB_LAB_PASSWORD, with\n" +
			"PCB_LAB_USERNAME) unlocks the keys in memory; the saved session is resumed, never a new login.",
		Args: cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := explicitDir(cmd); err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			if calendarID == "" {
				return cli.Finish(cmd, nil, "", cli.UsageError(errors.New("--calendar is required (the ID of the test account's pcb-lab calendar)")))
			}
			user, pass := os.Getenv("PCB_LAB_USERNAME"), os.Getenv("PCB_LAB_PASSWORD")
			if user == "" || pass == "" {
				return cli.Finish(cmd, nil, "", &exitcode.Error{Code: exitcode.Usage, Err: errors.New("PCB_LAB_USERNAME and PCB_LAB_PASSWORD are required (the keys are unlocked with the password)"),
					Hint: "pass-cli run --env-file scripts/lab.env.example -- pcb-lab probe write-matrix ..."})
			}
			o, err := options(cmd)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			s, err := session.Resume(cmd.Context(), o)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			defer s.Close()
			password := []byte(pass)
			defer clear(password)
			mo := probe.MatrixOptions{CalendarID: calendarID, Username: user, Password: password, Keep: keep, Interval: interval}
			if loopback(o.BaseURL) {
				mo.Limiter = rate.NewLimiter(rate.Inf, 1) // a local fake, never Proton: no pacing of reads
			}
			res, err := probe.WriteMatrix(cmd.Context(), s, mo)
			if err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "write matrix run %s, UIDs %s*, key salts %s", res.RunID, res.UIDPrefix, res.Scope)
			for _, cs := range res.Cases {
				b.WriteString("\n" + cs.Line)
			}
			fmt.Fprintf(&b, "\ncleanup: %s\n%d write(s)", res.Cleanup, res.Writes)
			hint := "wait (honour Retry-After), then delete the events whose UID starts with " + res.UIDPrefix + " in the web app (or the whole pcb-lab calendar)"
			switch {
			case res.Stopped != "":
				fmt.Fprintf(&b, "; stopped: %s", res.Stopped)
				err = &exitcode.Error{Code: exitcode.Partial, Err: fmt.Errorf("the write matrix stopped: %s", res.Stopped), Hint: hint}
			case res.CleanupFailed:
				err = &exitcode.Error{Code: exitcode.Partial, Err: errors.New("the cleanup left rows of this run (see the cleanup line)"), Hint: hint}
			}
			return cli.Finish(cmd, res, b.String(), err)
		},
	}
	c.Flags().StringVar(&calendarID, "calendar", "", "ID of the test account's pcb-lab calendar (required)")
	c.Flags().BoolVar(&keep, "keep", false, "leave the events written (default: delete them at the end)")
	c.Flags().DurationVar(&interval, "interval", 2*time.Second, "pause before each write but the first")
	return c
}

// loopback reports a base URL on the loopback (a test fake, never Proton).
func loopback(base string) bool {
	u, err := url.Parse(base)
	if err != nil || base == "" {
		return false
	}
	h := u.Hostname()
	ip := net.ParseIP(h)
	return h == "localhost" || (ip != nil && ip.IsLoopback())
}
