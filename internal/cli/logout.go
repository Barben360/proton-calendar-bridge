package cli

import (
	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (a *app) newLogout() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the Proton session and forget the stored credentials (through pcb serve when it runs)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := a.logout(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			text := "Logged out."
			switch {
			case !res.WasLoggedIn:
				text = "Not logged in."
			case !res.Revoked:
				text = "Logged out locally; Proton could not be told, revoke the session in the Proton account settings."
			}
			return Finish(cmd, res, text, nil)
		},
	}
}

// logout logs out through the daemon when one runs (its Serve then waits
// for a login, or stops), else in process.
func (a *app) logout(cmd *cobra.Command) (service.LogoutResult, error) {
	c, err := a.daemon(cmd)
	if err != nil {
		return service.LogoutResult{}, err
	}
	if c != nil {
		defer func() { _ = c.Close() }()
		return c.Logout(cmd.Context())
	}
	svc, err := a.service(cmd)
	if err != nil {
		return service.LogoutResult{}, err
	}
	defer a.closeService(svc)
	a.noDaemonNote(cmd)
	return svc.Logout(cmd.Context())
}
