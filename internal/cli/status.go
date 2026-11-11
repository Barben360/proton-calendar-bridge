package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (a *app) newStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the login state (no network call)",
		Args:  NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			svc, err := a.service(cmd)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			defer a.closeService(svc)
			st, err := svc.Status(cmd.Context())
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, st, statusText(st), nil)
		},
	}
}

// statusText is one "key: value" line per field; optional fields are left
// out when unset.
func statusText(s service.Status) string {
	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s: %s\n", k, v) }
	optional := func(k, v string) {
		if v != "" {
			line(k, v)
		}
	}
	at := func(t *time.Time) string {
		if t == nil {
			return ""
		}
		return t.Format(time.RFC3339)
	}
	line("phase", s.Phase)
	optional("reason", s.Reason)
	optional("username", s.Username)
	optional("account_id", s.AccountID)
	line("secrets", s.Secrets)
	line("key_file", s.KeyFile)
	line("unattended", fmt.Sprint(s.Unattended))
	optional("captcha_url", s.CaptchaURL)
	optional("last_login", at(s.LastLogin))
	optional("last_relogin", at(s.LastRelogin))
	if s.Sync != nil {
		if s.Sync.Error != "" {
			line("sync", "cache unreadable: "+s.Sync.Error)
		}
		if len(s.Sync.Calendars) == 0 && s.Sync.Error == "" {
			line("sync", "no calendars cached")
		}
		for _, c := range s.Sync.Calendars {
			v := fmt.Sprintf("%s (%s): %d events, synced %s, full %s", c.Name, c.ID, c.Events, orNever(at(c.SyncedAt)), orNever(at(c.FullAt)))
			if c.Error != "" {
				v += ", last error: " + c.Error
			}
			line("sync", v)
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func orNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}
