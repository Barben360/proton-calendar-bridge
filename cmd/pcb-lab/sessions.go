package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/probe"
	"github.com/Barben360/proton-calendar-bridge/internal/lab/session"
)

// explicitDir refuses a command that would fall back to the default lab
// dir: the live probes run only on a run root's session (scripts/live/run.sh
// passes --state-dir @LAB@), never on ~/.local/state/pcb-lab by accident.
func explicitDir(cmd *cobra.Command) error {
	if d, _ := cmd.Flags().GetString("state-dir"); d == "" {
		return cli.UsageError(errors.New("--state-dir is required (the run root's lab dir, @LAB@ under scripts/live/run.sh)"))
	}
	if d, _ := cmd.Flags().GetString("record-dir"); d != "" {
		return cli.UsageError(errors.New("--record-dir is not supported here: this probe records nothing"))
	}
	return nil
}

func sessionsCmd() *cobra.Command {
	var revoke, yes bool
	c := &cobra.Command{
		Use:   "sessions",
		Short: "List the test account's sessions; --revoke-others --yes revokes every one but this session",
		Long: "Lists every session of the test account (redacted UID, creation time, client, revocable), marking the\n" +
			"saved one. With --revoke-others and --yes it revokes every revocable session except its own (other lab\n" +
			"sessions, pcb's, proton-cal's and the web app's included), prints Proton's answer to each, then lists again.",
		Args: cli.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := explicitDir(cmd); err != nil {
				return cli.Finish(cmd, nil, "", err)
			}
			if revoke && !yes {
				return cli.Finish(cmd, nil, "", cli.UsageError(errors.New("--revoke-others needs --yes: it revokes every other session of the account")))
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
			res, err := probe.Sessions(cmd.Context(), s, revoke)
			if err != nil && len(res.Sessions) == 0 {
				return cli.Finish(cmd, nil, "", err)
			}
			var b strings.Builder
			for i, si := range res.Sessions {
				if i > 0 {
					b.WriteString("\n")
				}
				b.WriteString(si.Line())
			}
			if res.After != nil {
				fmt.Fprintf(&b, "\nafter: %d session(s)", *res.After)
			}
			if err == nil && res.Failed > 0 {
				err = &exitcode.Error{Code: exitcode.Blocked, Err: fmt.Errorf("%d revoke(s) not confirmed by Proton (see each line)", res.Failed)}
			}
			return cli.Finish(cmd, res, b.String(), err)
		},
	}
	c.Flags().BoolVar(&revoke, "revoke-others", false, "revoke every revocable session but this one")
	c.Flags().BoolVar(&yes, "yes", false, "confirm --revoke-others")
	return c
}
