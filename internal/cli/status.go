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
	c := selecting(&cobra.Command{
		Use:   "status",
		Short: "Show every account's login and, from a running pcb serve, its live state",
		Long: "Show the process and every account (--account: that one): its login and, when pcb serve runs, its " +
			"live state over the control socket: the listener, each calendar's sync and write gate, the render " +
			"counters and the alerts. Without a daemon, each login as the files describe it (the offline view, no " +
			"network call). Exit 0 whatever the alerts.",
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := a.status(cmd, daemonOnly)
			if st == nil {
				return Finish(cmd, nil, "", err)
			}
			return Finish(cmd, st, statusText(*st), err)
		},
	})
	c.Flags().BoolVar(&daemonOnly, "daemon", false, "exit 20 when no pcb daemon answers, instead of the offline view")
	return c
}

// status is the daemon's status, else (unless daemonOnly) the offline
// view: each login as the files describe it and its alerts. An offline
// account that cannot be read keeps its entry, with an account-unreadable
// alert, and its error is the command's (st is then set too).
func (a *app) status(cmd *cobra.Command, daemonOnly bool) (*ctl.Status, error) {
	ctx := cmd.Context()
	sel := a.selector(cmd)
	dirs, err := resolveDirs(a.Env)
	if err != nil && daemonOnly {
		return nil, err
	}
	if err == nil {
		st, ok, err := a.daemonStatus(ctx, dirs.Socket())
		switch {
		case err != nil:
			return nil, err
		case ok:
			return narrow(st, sel)
		case daemonOnly:
			return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("no pcb daemon answers on " + dirs.Socket()),
				Hint: startHint(a.Host)}
		}
	}
	h, err := a.hub(cmd)
	if err != nil {
		return nil, err
	}
	defer a.closeHub(h)
	return offlineStatus(ctx, h, sel)
}

// narrow keeps the account of st that selector names, all of them without one.
func narrow(st ctl.Status, selector string) (*ctl.Status, error) {
	if selector == "" {
		return &st, nil
	}
	infos := make([]service.AccountInfo, 0, len(st.Accounts))
	for _, as := range st.Accounts {
		infos = append(infos, as.AccountInfo)
	}
	info, err := service.SelectAccount(infos, selector)
	if err != nil {
		return nil, err
	}
	for _, as := range st.Accounts {
		if as.ID == info.ID {
			st.Accounts = []ctl.AccountStatus{as}
		}
	}
	return &st, nil
}

// offlineStatus is the status of h's accounts (the one selector names, when
// set) with no daemon: no listener, no live state; the process alerts are
// the daemon's would be (with no account, needs-login: ruling R44).
func offlineStatus(ctx context.Context, h Hub, selector string) (*ctl.Status, error) {
	infos := h.Accounts()
	if selector != "" {
		_, info, err := h.Account(selector)
		if err != nil {
			return nil, err
		}
		infos = []service.AccountInfo{info}
	}
	st := &ctl.Status{Accounts: make([]ctl.AccountStatus, 0, len(infos))}
	now := time.Now()
	var failure error
	for _, info := range infos {
		as := ctl.AccountStatus{AccountInfo: info}
		api, _, err := h.Account(info.ID)
		if err == nil {
			as.Login, err = api.Status(ctx)
		}
		switch {
		case err == nil:
			as.Alerts = ctl.Alerts(ctl.AlertInput{Account: info.ID, Login: as.Login, Stopped: info.Stopped, Now: now})
		case errors.Is(err, service.ErrNoAccount):
			continue // removed since Accounts listed it
		default:
			as.Alerts = []ctl.Alert{{Account: info.ID, Code: ctl.CodeAccountUnreadable, Severity: ctl.SeverityError,
				Message: err.Error(), Fix: exitcode.HintOf(err)}}
			if failure == nil {
				failure = err
			}
		}
		st.Accounts = append(st.Accounts, as)
	}
	st.Alerts = ctl.ProcessAlerts(nil, st.Accounts, now)
	return st, failure
}

// daemonStatus asks the daemon on socket for its status; ok is false when
// none answers there.
func (a *app) daemonStatus(ctx context.Context, socket string) (st ctl.Status, ok bool, err error) {
	c, err := a.dialDaemon(ctx, socket)
	if c == nil || err != nil {
		return ctl.Status{}, false, err
	}
	defer func() { _ = c.Close() }()
	st, err = c.Status(ctx)
	if err != nil {
		return ctl.Status{}, false, err
	}
	return st, true, nil
}

// statusText is the process (the daemon, its listener and its alerts),
// then a block per account: its name, its login (loginText), its serve
// loop, each calendar's live state, the render counters and its alerts.
func statusText(s ctl.Status) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	if d := s.Daemon; d == nil {
		line("daemon: not running (offline view)")
	} else {
		line("daemon: %s", daemonText(d))
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
	alerts := func(list []ctl.Alert) {
		for _, al := range list {
			line("alert: %s %s: %s (fix: %s)", al.Severity, al.Code, al.Message, al.Fix)
		}
	}
	alerts(s.Alerts)
	if len(s.Accounts) == 0 {
		line("accounts: none (add one with pcb login)")
	}
	for _, as := range s.Accounts {
		line("")
		line("account: %s%s", accountName(as.AccountInfo), accountMarks(as.AccountInfo))
		if as.URL != "" {
			line("url: %s", as.URL)
		}
		b.WriteString(loginText(as.Login))
		if h := as.Health; h != nil {
			if st := serveState(*h); st != "" {
				line("serve: %s", st)
			}
			for _, c := range h.Calendars {
				line("calendar: %s", calendarHealthText(c))
			}
			line("render: undecryptable %d, unverified %d, duplicate masters %d, dropped alarms %d", h.Render.Undecryptable, h.Render.Unverified, h.Render.DuplicateMasters, h.Render.DroppedAlarms)
		}
		alerts(as.Alerts)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// accountMarks says, after an account's name, what sets it apart: a
// pending add, CalDAV off, MCP off, a stopped loop.
func accountMarks(info service.AccountInfo) string {
	var marks []string
	if info.Pending {
		marks = append(marks, "pending")
	}
	if !info.CalDAV {
		marks = append(marks, "CalDAV off")
	}
	if !info.MCP {
		marks = append(marks, "MCP off")
	}
	if info.Stopped != nil {
		marks = append(marks, "stopped")
	}
	if len(marks) == 0 {
		return ""
	}
	return ", " + strings.Join(marks, ", ")
}

// serveState is "serving since T", or "waiting for a login since T" while
// the account's loop waits for one; "" when Serve says nothing of it.
func serveState(h service.Health) string {
	what := map[service.ServeState]string{
		service.ServeServing: "serving", service.ServeWaitingForLogin: "waiting for a login", service.ServeStopped: "stopped",
	}[h.Serve]
	if what == "" {
		return ""
	}
	if h.Since != nil {
		what += " since " + h.Since.Format(time.RFC3339)
	}
	return what
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

// loginText is one "key: value" line per field of a login, each ending in
// a newline; optional fields are left out when unset.
func loginText(s service.Status) string {
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
	return b.String()
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
