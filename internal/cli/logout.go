package cli

import (
	"github.com/spf13/cobra"
)

func (a *app) newLogout() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the Proton session and forget the stored credentials",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			res, err := svc.Logout(cmd.Context())
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
