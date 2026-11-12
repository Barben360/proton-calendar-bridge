package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (a *app) newLogout() *cobra.Command {
	var yes bool
	c := selecting(&cobra.Command{
		Use:   "logout",
		Short: "Sign the account out: revoke its Proton session and forget its stored credentials (through pcb serve when it runs)",
		Long: "Sign the account out: revoke its Proton session, delete its stored credentials and event cache. The " +
			"account stays listed (pcb account list), with its address and its CalDAV passwords; pcb login signs " +
			"it in again, pcb account remove forgets it.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := a.logout(cmd, yes)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			text := "Logged out."
			switch {
			case !res.WasLoggedIn:
				text = "Not logged in."
			case !res.Revoked:
				text = "Logged out locally; Proton could not be told: end the session in " + service.SessionsPage + "."
			}
			if res.SyncRunning {
				text += "\nAnother pcb process is syncing (pcb sync or pcb serve): once it stops, run pcb logout again so it leaves no event cache."
			}
			return Finish(cmd, res, text, nil)
		},
	})
	c.Flags().BoolVar(&yes, "yes", false, "discard a stored session this pcb cannot read without asking (it cannot be revoked)")
	return c
}

// logout signs the selected account out through the daemon when one runs
// (its loop then waits for a login), else in process (ruling R14: through
// the Hub, never the account's own API).
func (a *app) logout(cmd *cobra.Command, yes bool) (service.LogoutResult, error) {
	c, err := a.daemon(cmd)
	if err != nil {
		return service.LogoutResult{}, err
	}
	sel := a.selector(cmd)
	if c != nil {
		defer func() { _ = c.Close() }()
		list, err := c.Accounts(cmd.Context())
		if err != nil || (sel == "" && len(list) == 0) {
			return service.LogoutResult{}, err // no account: not logged in
		}
		info, err := chooseAccount(list, sel, anyAccount)
		if err != nil {
			return service.LogoutResult{}, err
		}
		status := func(ctx context.Context) (service.Status, error) {
			st, err := c.Status(ctx)
			for _, as := range st.Accounts {
				if as.ID == info.ID {
					return as.Login, err
				}
			}
			return service.Status{}, err
		}
		logout := func(ctx context.Context, req service.LogoutRequest) (service.LogoutResult, error) {
			return c.Logout(ctx, info.ID, req)
		}
		return a.logoutWith(cmd, yes, status, logout)
	}
	h, err := a.hub(cmd)
	if err != nil {
		return service.LogoutResult{}, err
	}
	defer a.closeHub(h)
	a.noDaemonNote(cmd)
	if sel == "" && len(h.Accounts()) == 0 {
		return service.LogoutResult{}, nil // no account: not logged in
	}
	api, info, err := h.Account(sel)
	if err != nil {
		return service.LogoutResult{}, err
	}
	logout := func(ctx context.Context, req service.LogoutRequest) (service.LogoutResult, error) {
		return h.SignOut(ctx, info.ID, req)
	}
	return a.logoutWith(cmd, yes, api.Status, logout)
}

// logoutWith runs the logout on status and logout. A stored session this
// pcb cannot read (state-I1) cannot be revoked: a person is told to end it
// in Proton's web app and asked to confirm the local wipe; --yes confirms
// it. Without a person nor --yes, the service refuses it (exit 10).
func (a *app) logoutWith(cmd *cobra.Command, yes bool, status func(context.Context) (service.Status, error),
	logout func(context.Context, service.LogoutRequest) (service.LogoutResult, error)) (service.LogoutResult, error) {
	ctx := cmd.Context()
	discard := yes
	if !yes && a.interactive(cmd) {
		if st, err := status(ctx); err == nil && st.Secrets == service.SecretsUnsupported {
			fmt.Fprintln(a.Stderr, st.Reason+", so pcb cannot revoke the Proton session: end it in "+service.SessionsPage+".")
			answer, err := a.Term.ReadLine(ctx, "Discard the stored session from this machine? Type discard to confirm: ")
			if err != nil {
				return service.LogoutResult{}, err
			}
			if strings.TrimSpace(answer) != "discard" {
				return service.LogoutResult{}, &exitcode.Error{Code: exitcode.MustAct, Err: errors.New("not confirmed: nothing was changed"),
					Hint: "type discard at the prompt, or pass --yes"}
			}
			discard = true
		}
	}
	return logout(ctx, service.LogoutRequest{Discard: discard})
}
