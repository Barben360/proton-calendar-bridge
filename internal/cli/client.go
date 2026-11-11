package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

type clientListData struct {
	Clients []service.Client `json:"clients"`
}

func (a *app) newClient() *cobra.Command {
	return Group("client", "CalDAV app passwords (who may read the bridge)", a.newClientAdd(), a.newClientList(), a.newClientRemove())
}

func (a *app) newClientAdd() *cobra.Command {
	return OneOperand(&cobra.Command{
		Use:   "add NAME",
		Short: "Create a CalDAV app password for one client (shown once)",
		Long: "Create an app password for a CalDAV client such as Thunderbird. The password is printed once and never " +
			"stored: only a salted hash is kept in the state directory, so a lost password is replaced by removing the " +
			"client and adding it again. Works logged out. NAME is 1 to 64 characters, unique (case-insensitive).",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			sec, err := svc.AddClient(cmd.Context(), args[0])
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, sec, clientAddText(sec), nil)
		},
	}, "NAME")
}

// clientAddText prints the password exactly once, then how to use it.
func clientAddText(s service.ClientSecret) string {
	return fmt.Sprintf(`Client %q added (id %s).

Password (shown once, not stored): %s

In Thunderbird: New Calendar > On the Network, format CalDAV, then:
  Username  anything (for example your account email)
  Password  the password above
  Location  the URL printed by pcb serve
Lost it? pcb client remove %q, then add the client again.`, s.Name, s.ID, s.Password, s.Name)
}

func (a *app) newClientList() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the CalDAV clients (never their passwords)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			list, err := svc.Clients(cmd.Context())
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			if list == nil {
				list = []service.Client{}
			}
			return Finish(cmd, clientListData{list}, clientTable(list), nil)
		},
	}
}

func clientTable(list []service.Client) string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tCREATED\tLAST USED")
	for _, c := range list {
		used := "never"
		if c.LastUsed != nil {
			used = c.LastUsed.Format(time.RFC3339)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", c.Name, c.ID, c.Created.Format(time.RFC3339), used)
	}
	_ = w.Flush() // a strings.Builder does not fail
	return strings.TrimSuffix(b.String(), "\n")
}

type clientRemoveData struct {
	Removed service.Client `json:"removed"`
}

func (a *app) newClientRemove() *cobra.Command {
	return OneOperand(&cobra.Command{
		Use:   "remove NAME|ID",
		Short: "Remove a CalDAV client: its password stops working at once",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			c, err := svc.RemoveClient(cmd.Context(), args[0])
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, clientRemoveData{c}, fmt.Sprintf("Client %q removed (id %s).", c.Name, c.ID), nil)
		},
	}, "NAME or ID")
}
