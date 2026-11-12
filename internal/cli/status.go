package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

func (a *app) newStatus() *cobra.Command {
	var daemonOnly bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show the login and, from a running pcb serve, its live state",
		Long: "Show the login and, when pcb serve runs, its live state over the control socket: the listener, each " +
			"calendar's sync and write gate, the render counters and the alerts. Without a daemon, the login as the " +
			"files describe it (the offline view, no network call). Exit 0 whatever the alerts.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.status(cmd, daemonOnly)
			if err != nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, st, liveStatusText(st), nil)
		},
	}
	c.Flags().BoolVar(&daemonOnly, "daemon", false, "exit 20 when no pcb daemon answers, instead of the offline view")
	return c
}

// status is the daemon's status, else (unless daemonOnly) the offline view:
// the login as the files describe it and its alerts.
func (a *app) status(cmd *cobra.Command, daemonOnly bool) (liveStatus, error) {
	ctx := cmd.Context()
	dirs, err := resolveDirs(a.Env)
	if err != nil && daemonOnly {
		return liveStatus{}, err
	}
	if err == nil {
		st, ok, err := a.daemonStatus(ctx, dirs.Socket())
		switch {
		case err != nil:
			return liveStatus{}, err
		case ok:
			return st, nil
		case daemonOnly:
			return liveStatus{}, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("no pcb daemon answers on " + dirs.Socket()),
				Hint: startHint(a.Host)}
		}
	}
	svc, err := a.service(cmd)
	if err != nil {
		return liveStatus{}, err
	}
	defer a.closeService(svc)
	local, err := svc.Status(ctx)
	if err != nil {
		return liveStatus{}, err
	}
	return liveStatus{Status: local, Alerts: ctl.Alerts(ctl.AlertInput{Login: local, Now: time.Now()})}, nil
}

// daemonStatus asks the daemon on socket for its status; ok is false when
// none answers there.
func (a *app) daemonStatus(ctx context.Context, socket string) (st liveStatus, ok bool, err error) {
	c, err := a.dialDaemon(ctx, socket)
	if c == nil || err != nil {
		return liveStatus{}, false, err
	}
	defer func() { _ = c.Close() }()
	cs, err := c.Status(ctx)
	if err != nil {
		return liveStatus{}, false, err
	}
	return flatStatus(cs), true, nil
}

// liveStatusText is statusText, then the daemon, its listener, each
// calendar's live state, the render counters and one line per alert.
func liveStatusText(s liveStatus) string {
	var b strings.Builder
	b.WriteString(statusText(s.Status))
	line := func(format string, args ...any) { fmt.Fprintf(&b, "\n"+format, args...) }
	if s.Daemon == nil {
		line("daemon: not running (offline view)")
	} else {
		line("daemon: %s", daemonState(s))
	}
	if l := s.Listener; l != nil {
		var tls []string
		if l.TLS {
			tls = append(tls, "TLS")
			if l.CertNotAfter != nil {
				tls = append(tls, "certificate until "+l.CertNotAfter.Format(time.DateOnly))
			}
			if l.Fingerprint != "" {
				tls = append(tls, "fingerprint "+l.Fingerprint)
			}
		} else {
			tls = append(tls, "plain HTTP")
		}
		line("listener: %s (%s)", l.URL, strings.Join(tls, ", "))
	}
	if h := s.Health; h != nil {
		for _, c := range h.Calendars {
			line("calendar: %s", calendarHealthText(c))
		}
		line("render: undecryptable %d, unverified %d, duplicate masters %d, dropped alarms %d", h.Render.Undecryptable, h.Render.Unverified, h.Render.DuplicateMasters, h.Render.DroppedAlarms)
	}
	for _, al := range s.Alerts {
		line("alert: %s %s: %s (fix: %s)", al.Severity, al.Code, al.Message, al.Fix)
	}
	return b.String()
}

// daemonState is "serving since T (pid N, version V)": since Serve began
// serving, else since the daemon started; or "waiting for a login since T"
// while Serve waits for one.
func daemonState(s liveStatus) string {
	since := s.Daemon.StartedAt
	if s.Health != nil && s.Health.Since != nil {
		since = *s.Health.Since
	}
	if s.Health != nil && s.Health.Serve == service.ServeWaitingForLogin {
		return "waiting for a login since " + since.Format(time.RFC3339)
	}
	return fmt.Sprintf("serving since %s (pid %d, version %s)", since.Format(time.RFC3339), s.Daemon.PID, s.Daemon.Version)
}

// calendarHealthText is one calendar's live state; a calendar never synced
// has no lag.
func calendarHealthText(c service.CalendarHealth) string {
	v := fmt.Sprintf("%s (%s): synced ", c.Name, c.ID)
	if c.SyncedAt == nil {
		v += "never"
	} else {
		v += c.SyncedAt.Format(time.RFC3339)
		if c.LagSeconds != nil {
			v += ", lag " + (time.Duration(*c.LagSeconds) * time.Second).String()
		}
	}
	v += ", write gate " + c.Gate
	if c.GateReason != "" {
		v += " (" + c.GateReason + ")"
	}
	if c.Error != "" {
		v += ", last error " + c.ErrorClass + ": " + c.Error
	}
	return v
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
	optional("login_expires", at(s.LoginExpires))
	optional("last_login", at(s.LastLogin))
	optional("last_relogin", at(s.LastRelogin))
	if s.Sync == nil && s.Phase == string(service.StateReady) {
		line("sync", "never (run pcb sync)")
	}
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

// startHint is how to start a daemon that does not answer, on this host
// (final review M2): the service where a unit is installed, the
// container's restart in a container (no systemd there), else pcb serve.
func startHint(h install.Host) string {
	switch {
	case install.SystemdRunning(h):
		if _, _, err := install.Detect(h); err == nil {
			return "start it: pcb service start (pcb service logs says why it stopped)"
		}
		return "start it: pcb serve, or pcb install to run it as a service"
	case install.InContainer(h):
		return "restart the container (docker restart NAME, or docker compose up -d); docker logs NAME says why it stopped"
	}
	return "start it: pcb serve"
}
