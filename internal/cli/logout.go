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
	c := &cobra.Command{
		Use:   "logout",
		Short: "Revoke the Proton session and forget the stored credentials (through pcb serve when it runs)",
		Args:  NoArgs,
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
	}
	c.Flags().BoolVar(&yes, "yes", false, "discard a stored session this pcb cannot read without asking (it cannot be revoked)")
	return c
}

// logout logs out through the daemon when one runs (its Serve then waits
// for a login, or stops), else in process.
func (a *app) logout(cmd *cobra.Command, yes bool) (service.LogoutResult, error) {
	c, err := a.daemon(cmd)
	if err != nil {
		return service.LogoutResult{}, err
	}
	if c != nil {
		defer func() { _ = c.Close() }()
		status := func(ctx context.Context) (service.Status, error) {
			st, err := c.Status(ctx)
			return st.Status, err
		}
		return a.logoutWith(cmd, yes, status, c.Logout)
	}
	svc, err := a.service(cmd)
	if err != nil {
		return service.LogoutResult{}, err
	}
	defer a.closeService(svc)
	a.noDaemonNote(cmd)
	return a.logoutWith(cmd, yes, svc.Status, svc.Logout)
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
