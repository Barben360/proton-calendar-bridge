package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The account selector (design §4.6): --account A, else PCB_ACCOUNT, where
// A is the account's address (case-insensitive) or its local ID. Empty, a
// command acts on the only account that is not pending.
const (
	accountFlag = "account"
	accountEnv  = "PCB_ACCOUNT"
	// selectorKey marks the commands that take the selector; every other
	// one refuses --account (exit 2).
	selectorKey = "pcb/account"
)

// addSelector declares --account on the root, for every command.
func addSelector(root *cobra.Command) {
	root.PersistentFlags().String(accountFlag, "", "the account to act on: its address or its local ID (pcb account list); default $"+accountEnv+", else the only one")
	help := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		// The help offers --account only where it is taken (ux Minor).
		if f := cmd.InheritedFlags().Lookup(accountFlag); f != nil && !takesSelector(cmd) {
			f.Hidden = true
			defer func() { f.Hidden = false }()
		}
		help(cmd, args)
	})
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if !cmd.Flags().Changed(accountFlag) || takesSelector(cmd) {
			return nil
		}
		if cmd.Name() == "login" && cmd.Parent() == cmd.Root() {
			return UsageError(errors.New("pcb login takes no --account: the username decides (a new one adds an account)"))
		}
		return UsageError(fmt.Errorf("%s takes no --account", cmd.CommandPath()))
	}
}

// selecting marks c as taking the selector and returns it.
func selecting(c *cobra.Command) *cobra.Command {
	if c.Annotations == nil {
		c.Annotations = map[string]string{}
	}
	c.Annotations[selectorKey] = "true"
	return c
}

func takesSelector(c *cobra.Command) bool { return c.Annotations[selectorKey] == "true" }

// selector is cmd's account selector: --account, else PCB_ACCOUNT ("":
// none named).
func (a *app) selector(cmd *cobra.Command) string {
	if v, _ := cmd.Flags().GetString(accountFlag); v != "" {
		return v
	}
	return a.Env(accountEnv)
}

// daemonAccount is the local ID of the daemon's account selector names,
// chosen as Hub.Account does: by local ID, else by address
// (case-insensitive), pending accounts included; empty, the only account
// that is not pending. The control socket's account routes take an ID.
func daemonAccount(ctx context.Context, c *ctl.Client, selector string) (service.AccountInfo, error) {
	list, err := c.Accounts(ctx)
	if err != nil {
		return service.AccountInfo{}, err
	}
	return chooseAccount(list, selector, anyAccount)
}

// anyAccount makes every account eligible for chooseAccount.
func anyAccount(service.AccountInfo) bool { return true }

// chooseAccount is the account of list selector names; empty, the only
// one of list that is not pending, among those eligible (the others are
// passed over).
func chooseAccount(list []service.AccountInfo, selector string, eligible func(service.AccountInfo) bool) (service.AccountInfo, error) {
	if selector != "" {
		if info, ok := findAccount(list, selector); ok {
			return info, nil
		}
		return service.AccountInfo{}, &exitcode.Error{Code: exitcode.Usage, Err: errors.New("no account " + selector), Hint: "see pcb account list"}
	}
	var ready []service.AccountInfo
	pending := 0
	for _, info := range list {
		switch {
		case !eligible(info):
		case info.Pending:
			pending++
		default:
			ready = append(ready, info)
		}
	}
	switch len(ready) {
	case 1:
		return ready[0], nil
	case 0:
		hint := "add one with pcb login"
		if pending > 0 {
			hint = "finish the pending login (pcb login resume), or add an account with pcb login"
		}
		return service.AccountInfo{}, &exitcode.Error{Code: exitcode.Usage, Err: errors.New("no account"), Hint: hint}
	}
	return service.AccountInfo{}, severalAccounts(ready)
}

// findAccount is the account of list whose local ID is selector, else
// whose address is (case-insensitively).
func findAccount(list []service.AccountInfo, selector string) (service.AccountInfo, bool) {
	for _, info := range list {
		if info.ID == selector {
			return info, true
		}
	}
	for _, info := range list {
		if strings.EqualFold(info.Address, selector) {
			return info, true
		}
	}
	return service.AccountInfo{}, false
}

// severalAccounts is the refusal to choose among list, as the Hub words it.
func severalAccounts(list []service.AccountInfo) error {
	return &exitcode.Error{Code: exitcode.Usage, Err: &service.SeveralAccountsError{Accounts: list},
		Hint: "name one with --account or PCB_ACCOUNT (pcb account list)"}
}

// accountName is how a line names an account: its address and local ID,
// or the ID alone while the address is unknown.
func accountName(info service.AccountInfo) string {
	if info.Address == "" {
		return info.ID
	}
	return info.Address + " (" + info.ID + ")"
}
