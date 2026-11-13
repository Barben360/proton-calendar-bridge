package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (a *app) newAccount() *cobra.Command {
	return Group("account", "The Proton accounts pcb holds (add one with pcb login)",
		a.newAccountList(), a.newAccountCalDAV(), a.newAccountMCP(), a.newAccountRestart(), a.newAccountRemove())
}

// accountEntry is one account of pcb account list: what the Hub says of
// it and when its cache last synced (nil: never, or not readable).
type accountEntry struct {
	service.AccountInfo
	LastSync *time.Time `json:"last_sync,omitempty"`
}

type accountListData struct {
	Accounts []accountEntry `json:"accounts"`
}

func (a *app) newAccountList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the accounts: local ID, address, phase, CalDAV and MCP on or off, last sync",
		Long: "List the accounts, pending adds included, sorted by address: the running daemon's view when one " +
			"answers (a stopped account shows stopped), else the files'.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, err := a.accountEntries(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, accountListData{list}, accountTable(list), nil)
		},
	}
}

// accountEntries lists the accounts from the daemon when one answers, else
// from the in-process accounts.
func (a *app) accountEntries(cmd *cobra.Command) ([]accountEntry, error) {
	c, err := a.daemon(cmd)
	if err != nil {
		return nil, err
	}
	if c != nil {
		defer func() { _ = c.Close() }()
		st, err := c.Status(cmd.Context())
		if err != nil {
			return nil, err
		}
		out := make([]accountEntry, 0, len(st.Accounts))
		for _, as := range st.Accounts {
			out = append(out, accountEntry{AccountInfo: as.AccountInfo, LastSync: lastSync(as.Login)})
		}
		return out, nil
	}
	h, err := a.hub(cmd)
	if err != nil {
		return nil, err
	}
	defer a.closeHub(h)
	infos := h.Accounts()
	out := make([]accountEntry, 0, len(infos))
	for _, info := range infos {
		e := accountEntry{AccountInfo: info}
		if api, _, err := h.Account(info.ID); err == nil {
			if st, err := api.Status(cmd.Context()); err == nil {
				e.LastSync = lastSync(st)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// lastSync is the latest sync of st's calendars, nil if none synced.
func lastSync(st service.Status) *time.Time {
	var last *time.Time
	if st.Sync == nil {
		return nil
	}
	for _, c := range st.Sync.Calendars {
		if c.SyncedAt != nil && (last == nil || c.SyncedAt.After(*last)) {
			t := *c.SyncedAt
			last = &t
		}
	}
	return last
}

// noAccountText is pcb account list with no account.
const noAccountText = "No account yet.\nHint: run pcb login"

func accountTable(list []accountEntry) string {
	if len(list) == 0 {
		return noAccountText
	}
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tADDRESS\tPHASE\tCALDAV\tMCP\tLAST SYNC")
	for _, e := range list {
		phase := e.Phase
		switch {
		case e.Stopped != nil:
			phase = "stopped"
		case e.Pending:
			phase += " (pending)"
		}
		synced := "never"
		if e.LastSync != nil {
			synced = e.LastSync.Format(time.RFC3339)
		}
		address := e.Address
		if address == "" {
			address = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, address, phase, onOff(e.CalDAV), onOff(e.MCP), synced)
	}
	_ = w.Flush() // a strings.Builder does not fail
	return strings.TrimSuffix(b.String(), "\n")
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// accountChange is an account-changing call (ruling R14: through the Hub,
// never the account's own API): over the control socket when a daemon
// answers (ruling R15: a running Hub does not rescan the accounts), else
// on the in-process accounts. Both receive the account's local ID.
type accountChange struct {
	daemon func(ctx context.Context, c *ctl.Client, id string) (service.AccountInfo, error)
	local  func(ctx context.Context, h Hub, id string) (service.AccountInfo, error)
	// confirm, when set, runs once the account is known, before the change.
	confirm func(info service.AccountInfo) error
}

// changeAccount runs ch on cmd's account (selector).
func (a *app) changeAccount(cmd *cobra.Command, ch accountChange) (service.AccountInfo, error) {
	ctx := cmd.Context()
	sel := a.selector(cmd)
	c, err := a.daemon(cmd)
	if err != nil {
		return service.AccountInfo{}, err
	}
	if c != nil {
		defer func() { _ = c.Close() }()
		info, err := daemonAccount(ctx, c, sel)
		if err != nil {
			return service.AccountInfo{}, err
		}
		if ch.confirm != nil {
			if err := ch.confirm(info); err != nil {
				return service.AccountInfo{}, err
			}
		}
		return ch.daemon(ctx, c, info.ID)
	}
	h, err := a.hub(cmd)
	if err != nil {
		return service.AccountInfo{}, err
	}
	defer a.closeHub(h)
	_, info, err := h.Account(sel)
	if err != nil {
		return service.AccountInfo{}, err
	}
	if ch.confirm != nil {
		if err := ch.confirm(info); err != nil {
			return service.AccountInfo{}, err
		}
	}
	return ch.local(ctx, h, info.ID)
}

func (a *app) newAccountCalDAV() *cobra.Command {
	return a.adapterSwitch("caldav", "CalDAV",
		"Turn the account's CalDAV on or off (off: its clients are refused; its sync pauses only when MCP is off too)",
		func(ctx context.Context, c *ctl.Client, id string, on bool) (service.AccountInfo, error) {
			return c.SetCalDAV(ctx, id, on)
		},
		func(ctx context.Context, h Hub, id string, on bool) (service.AccountInfo, error) {
			return h.SetCalDAV(ctx, id, on)
		})
}

func (a *app) newAccountMCP() *cobra.Command {
	return a.adapterSwitch("mcp", "MCP",
		"Turn the account's MCP on or off (off: agents do not see it; its sync pauses only when CalDAV is off too)",
		func(ctx context.Context, c *ctl.Client, id string, on bool) (service.AccountInfo, error) {
			return c.SetMCP(ctx, id, on)
		},
		func(ctx context.Context, h Hub, id string, on bool) (service.AccountInfo, error) {
			return h.SetMCP(ctx, id, on)
		})
}

// adapterSwitch is pcb account <use> on|off: an adapter switch (named
// adapter in its output), set through the daemon when one answers, else on
// the in-process accounts.
func (a *app) adapterSwitch(use, adapter, short string,
	daemon func(ctx context.Context, c *ctl.Client, id string, on bool) (service.AccountInfo, error),
	local func(ctx context.Context, h Hub, id string, on bool) (service.AccountInfo, error),
) *cobra.Command {
	return OneOperand(selecting(&cobra.Command{
		Use:   use + " on|off",
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			var on bool
			switch args[0] {
			case "on":
				on = true
			case "off":
			default:
				return UsageError(fmt.Errorf("%q is neither on nor off", args[0]))
			}
			info, err := a.changeAccount(cmd, accountChange{
				daemon: func(ctx context.Context, c *ctl.Client, id string) (service.AccountInfo, error) {
					return daemon(ctx, c, id, on)
				},
				local: func(ctx context.Context, h Hub, id string) (service.AccountInfo, error) {
					return local(ctx, h, id, on)
				},
			})
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, info, fmt.Sprintf("%s of %s: %s.", adapter, accountName(info), args[0]), nil)
		},
	}), "on or off")
}

func (a *app) newAccountRestart() *cobra.Command {
	return selecting(&cobra.Command{
		Use:   "restart",
		Short: "Restart the account's serve loop after it stopped on an error (see pcb status)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			served := false
			info, err := a.changeAccount(cmd, accountChange{
				daemon: func(ctx context.Context, c *ctl.Client, id string) (service.AccountInfo, error) {
					served = true
					return c.Restart(ctx, id)
				},
				local: func(ctx context.Context, h Hub, id string) (service.AccountInfo, error) {
					return h.Restart(ctx, id)
				},
			})
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			text := "Account " + accountName(info) + " restarted."
			if !served {
				text = "No pcb daemon is running: " + accountName(info) + " is served again at the next pcb serve."
			}
			return Finish(cmd, info, text, nil)
		},
	})
}

func (a *app) newAccountRemove() *cobra.Command {
	var yes bool
	c := selecting(&cobra.Command{
		Use:   "remove",
		Short: "Sign the account out, then forget it: its directory, cache and CalDAV passwords are deleted",
		Long: "Sign the account out (its Proton session revoked), then delete its directory: its event cache and its " +
			"CalDAV passwords go with it. Through pcb serve when it runs. It asks for a confirmation; --yes gives " +
			"it (without a terminal, or with --non-interactive, --yes is required: exit 10).",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info, err := a.changeAccount(cmd, accountChange{
				confirm: func(info service.AccountInfo) error {
					if yes {
						return nil
					}
					return a.confirmRemove(cmd, info)
				},
				daemon: func(ctx context.Context, c *ctl.Client, id string) (service.AccountInfo, error) {
					return c.Remove(ctx, id, service.LogoutRequest{})
				},
				local: func(ctx context.Context, h Hub, id string) (service.AccountInfo, error) {
					return h.Remove(ctx, id, service.LogoutRequest{})
				},
			})
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, info, "Account "+accountName(info)+" removed.", nil)
		},
	})
	c.Flags().BoolVar(&yes, "yes", false, "remove without asking")
	return c
}

// confirmRemove asks the person to type "remove" before info goes; without
// a person it fails naming --yes (exit 10).
func (a *app) confirmRemove(cmd *cobra.Command, info service.AccountInfo) error {
	if !a.interactive(cmd) {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("removing " + accountName(info) + " deletes its CalDAV passwords: confirm with --yes"),
			Hint: "pcb account remove --account " + info.ID + " --yes"}
	}
	q := "Remove " + accountName(info) + "? It is signed out and its directory deleted, CalDAV passwords included. Type remove to confirm: "
	answer, err := a.Term.ReadLine(cmd.Context(), q)
	if err != nil {
		return noAnswer(cmd.Context(), "confirmation", err)
	}
	if strings.TrimSpace(answer) != "remove" {
		return &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("not confirmed: nothing was changed"),
			Hint: "type remove at the prompt, or pass --yes"}
	}
	return nil
}
