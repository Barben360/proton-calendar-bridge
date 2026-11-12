package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

type clientListData struct {
	Clients []service.Client `json:"clients"`
}

func (a *app) newClient() *cobra.Command {
	return Group("client", "CalDAV app passwords (who may read the bridge)",
		a.newClientAdd(), a.newClientList(), a.newClientShow(), a.newClientRotate(), a.newClientRemove())
}

func (a *app) newClientAdd() *cobra.Command {
	return OneOperand(&cobra.Command{
		Use:   "add NAME",
		Short: "Create a CalDAV app password for one client (a device, separately revocable)",
		Long: "Create an app password for a CalDAV client such as Thunderbird. The state directory keeps a salted hash " +
			"of it for the check and a copy sealed with the key of the Proton session, so pcb client show prints it " +
			"again. Works logged out. NAME is 1 to 64 characters, unique (case-insensitive); it is also the username " +
			"to give (any username is accepted).",
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

// clientAddText prints the password, then how to use it.
func clientAddText(s service.ClientSecret) string {
	return fmt.Sprintf(`Client %q added (id %s).

Password: %s

In Thunderbird: New Calendar > On the Network, format CalDAV, then:
  Username  %s
  Password  the password above
  Location  the URL printed by pcb client show
Show it all again: pcb client show %s`, s.Name, s.ID, s.Password, s.Name, shellquote.Quote(s.Name))
}

func (a *app) newClientShow() *cobra.Command {
	var reveal bool
	c := &cobra.Command{
		Use:   "show [NAME|ID]",
		Short: "Print what a CalDAV client needs: URL, username, password and the TLS fingerprint",
		Long: "Print the URL, the username (the client's name; any username is accepted), the password and, over " +
			"TLS, the certificate's SHA-256 fingerprint. Without NAME: the client named default, else the only " +
			"client. The URL and fingerprint are the running daemon's, else what pcb serve would use for the " +
			"configuration. --json leaves the password out unless --reveal is given.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return UsageError(fmt.Errorf("unexpected operand %q for %q: it takes at most one NAME or ID", args[1], cmd.CommandPath()))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			cr, err := svc.ShowClient(cmd.Context(), name)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			var urlErr error
			cr.URL, cr.Fingerprint, urlErr = a.listenerFacts(cmd)
			text := clientShowText(cr, urlErr)
			if !reveal {
				cr.Password = "" // the JSON form only
			}
			return Finish(cmd, cr, text, nil)
		},
	}
	c.Flags().BoolVar(&reveal, "reveal", false, "include the password in --json")
	return c
}

// listenerFacts is the URL and, over TLS, the fingerprint a CalDAV client
// needs: the running daemon's listener when one answers, else what pcb
// serve would use for the configuration (offlineListener). Neither is
// secret: a daemon that cannot be asked is passed over. err says why the
// URL is not known (a configuration that does not load).
func (a *app) listenerFacts(cmd *cobra.Command) (url, fingerprint string, err error) {
	ctx := cmd.Context()
	if c, err := a.daemon(cmd); err == nil && c != nil {
		st, err := c.Status(ctx)
		_ = c.Close()
		if err == nil && st.Listener != nil {
			return st.Listener.URL, st.Listener.Fingerprint, nil
		}
	}
	dirs, cfg, err := loadConfig(a.Env)
	if err != nil {
		return "", "", err
	}
	url, fingerprint = offlineListener(ctx, cfg.CalDAV, filepath.Join(dirs.State, "tls"))
	if url == "" {
		return "", "", errors.New("caldav.listen is not HOST:PORT")
	}
	return url, fingerprint, nil
}

// clientShowText is one "Label: value" line per thing to enter; urlErr
// says why the URL is not known.
func clientShowText(cr service.ClientCredentials, urlErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CalDAV client %q (id %s)\n", cr.Name, cr.ID)
	if urlErr != nil {
		fmt.Fprintf(&b, "URL: not known (%s): check config.toml with pcb doctor\n", urlErr)
	} else {
		fmt.Fprintf(&b, "URL: %s\n", cr.URL)
	}
	fmt.Fprintf(&b, "Username: %s\n", cr.Username)
	if cr.Recoverable {
		fmt.Fprintf(&b, "Password: %s", cr.Password)
	} else {
		fmt.Fprintf(&b, "Password: not recoverable (made before pcb could show passwords): run pcb client rotate %s", shellquote.Quote(cr.Name))
	}
	if strings.HasPrefix(cr.URL, "https://") {
		fp := cr.Fingerprint
		if fp == "" {
			fp = "not known yet: pcb serve creates the certificate at its first start"
		}
		fmt.Fprintf(&b, "\nTLS certificate SHA-256 fingerprint: %s", fp)
	}
	return b.String()
}

func (a *app) newClientRotate() *cobra.Command {
	return OneOperand(&cobra.Command{
		Use:   "rotate NAME|ID",
		Short: "Give a CalDAV client a new password: the old one stops working at once",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			sec, err := svc.RotateClient(cmd.Context(), args[0])
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, sec, fmt.Sprintf("Client %q has a new password (id %s).\n\nPassword: %s\n\nHint: update the password in your CalDAV client",
				sec.Name, sec.ID, sec.Password), nil)
		},
	}, "NAME or ID")
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
