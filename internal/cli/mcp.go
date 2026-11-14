package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/mcp"
)

// newMCP is pcb mcp: the stdio MCP server of one agent (design §5.3), and
// the group of the remote tokens' commands.
func (a *app) newMCP() *cobra.Command {
	var (
		profile, allow string
		accounts       []string
	)
	c := selecting(&cobra.Command{
		Use:   "mcp",
		Short: "Serve MCP to an agent over stdin and stdout; manage the remote agents' tokens",
		Long: "Speak MCP on stdin and stdout for the agent that runs this command; logs and warnings go to stderr. " +
			"When a pcb daemon runs (pcb serve, pcb-ui), the session is relayed to it over its control socket; " +
			"otherwise pcb mcp serves the accounts itself until the agent ends the session (kind mcp: no network " +
			"listener, and pcb serve or pcb-ui cannot start meanwhile). --profile read-only (the default) allows " +
			"reading and starting a login, full allows everything; --allow replaces the profile with a " +
			"comma-separated list among freebusy, read, write, delete, login, manage. --account limits the session " +
			"to accounts (address or local ID, repeatable; none: every account). With a system-scope pcb service " +
			"installed, use its remote /mcp with a token instead (pcb mcp token).",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if JSONMode(cmd) {
				return &exitcode.Error{Code: exitcode.Usage,
					Err:  errors.New("--json cannot be combined with pcb mcp: stdout carries the MCP session"),
					Hint: "drop --json"}
			}
			g, err := stdioGrant(cmd, profile, allow, accounts)
			if err != nil {
				return err
			}
			return a.runMCP(cmd, g)
		},
	})
	c.Flags().StringVar(&profile, "profile", "read-only", "what the agent may do: `PROFILE` read-only or full")
	c.Flags().StringVar(&allow, "allow", "", "what the agent may do, instead of a profile: `CAPS` comma-separated among freebusy, read, write, delete, login, manage")
	// --account here is the command's own (repeatable: the session's
	// scope), in place of the root's selector; selecting lets it past the
	// root's check.
	c.Flags().StringArrayVar(&accounts, "account", nil, "an `ACCOUNT` the agent may reach, by address or local ID (repeatable; none: every account)")
	// Help offers --json on the token commands only.
	c.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if f := cmd.InheritedFlags().Lookup("json"); f != nil && cmd == c {
			f.Hidden = true
			defer func() { f.Hidden = false }()
		}
		cmd.Root().HelpFunc()(cmd, args)
	})
	c.AddCommand(a.newMCPToken())
	return c
}

// stdioGrant is the session's grant: the profile, or --allow's
// capabilities (implications applied), scoped to --account.
func stdioGrant(cmd *cobra.Command, profile, allow string, accounts []string) (mcp.Grant, error) {
	if cmd.Flags().Changed("allow") && cmd.Flags().Changed("profile") {
		return mcp.Grant{}, &exitcode.Error{Code: exitcode.Usage, Err: errors.New("--allow replaces the profile: give --profile or --allow, not both"),
			Hint: "drop --profile"}
	}
	var g mcp.Grant
	if cmd.Flags().Changed("allow") {
		caps, err := mcp.ParseCaps(allow)
		if err != nil {
			return mcp.Grant{}, &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("--allow: %w", err), Hint: "run pcb mcp --help"}
		}
		g = mcp.Grant{Name: "stdio:custom", Caps: caps}
	} else {
		p, err := mcp.Profile(profile)
		if err != nil {
			return mcp.Grant{}, &exitcode.Error{Code: exitcode.Usage, Err: fmt.Errorf("--profile: %w", err), Hint: "run pcb mcp --help"}
		}
		g = p
		g.Name = "stdio:" + profile
	}
	g.Accounts = accounts
	return g, nil
}
