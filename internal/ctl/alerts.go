package ctl

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The alert codes (stable) and severities.
const (
	CodeNeedsLogin         = "needs-login"
	CodeAccountStopped     = "account-stopped"
	CodeAccountUnreadable  = "account-unreadable"
	CodeNeedsCaptcha       = "needs-captcha"
	CodeHumanVerification  = "human-verification"
	CodeSyncStalled        = "sync-stalled"
	CodeCalendarKeysFailed = "calendar-keys-failed"
	CodeWriteGateUndecided = "write-gate-undecided"
	CodeClientsUnreadable  = "clients-file-unreadable"
	CodeTLSExpiring        = "tls-certificate-expiring"
	CodeTLSExpired         = "tls-certificate-expired"
	CodeKeyringLocked      = "keyring-locked"
	CodeKeyringUnavailable = "keyring-unavailable"

	SeverityError   = "error"
	SeverityWarning = "warning"
)

// stalledAfter is how many sync intervals without a successful sync of a
// calendar make it stalled; certWarn how early an expiring certificate is.
const (
	stalledAfter = 5
	certWarn     = 30 * 24 * time.Hour
)

const certFix = "renew caldav.tls_cert; a generated one is renewed by restarting pcb"

// AlertInput is what one account's alerts are computed from: its local ID
// (empty offline), its login, its serve loop's stop and, when a daemon
// answers, Serve's health (nil offline).
type AlertInput struct {
	Account string
	Login   service.Status
	Stopped *service.StopInfo
	Health  *service.Health
	Now     time.Time
}

// Alerts lists the problems of one account, errors first, then by code,
// then by calendar ID; never nil. Each carries in.Account.
func Alerts(in AlertInput) []Alert {
	out := []Alert{}
	add := func(code, sev, msg, fix, cal string) {
		out = append(out, Alert{Account: in.Account, Code: code, Severity: sev, Message: msg, Fix: fix, Calendar: cal})
	}
	if s := in.Stopped; s != nil {
		add(CodeAccountStopped, SeverityError, s.Message, s.Hint, "")
	}
	switch in.Login.Phase {
	case "logged-out", "needs-login":
		msg := in.Login.Reason
		if msg == "" {
			msg = "not logged in to Proton"
		}
		fix := "run pcb login"
		if in.Login.Secrets == service.SecretsUnsupported {
			fix = "upgrade pcb" // a login fails the same way (state-I1)
		}
		add(CodeNeedsLogin, SeverityError, msg, fix, "")
	case "needs-captcha":
		add(CodeNeedsCaptcha, SeverityError, "Proton asks for a CAPTCHA: solve it at "+in.Login.CaptchaURL,
			"pcb login resume --captcha-token TOKEN (the token parameter of that URL)", "")
	}
	if in.Login.Secrets == "locked" {
		add(CodeKeyringLocked, SeverityError, "the desktop keyring is locked", service.KeyringFix, "")
	}
	if in.Login.Secrets == service.SecretsKeyringUnavailable {
		add(CodeKeyringUnavailable, SeverityError, "the desktop keyring is not available", service.KeyringUnavailableFix, "")
	}
	if f := in.Login.ClientsFile; f != "" {
		add(CodeClientsUnreadable, SeverityError, "the CalDAV clients file cannot be loaded: every CalDAV request answers 503",
			"fix or restore "+f+" (pcb client list shows the error)", "")
	}
	if h := in.Health; h != nil {
		healthAlerts(h, in.Now, add)
	}
	SortAlerts(out)
	return out
}

// ProcessAlerts lists the alerts of the process, not of an account, sorted
// as SortAlerts; never nil. With no account at all (a fresh install) it is
// needs-login, as M5's one account was (ruling R44): an account that exists
// says what it needs itself (its own needs-login, needs-captcha or
// account-unreadable; a login waiting for its 2FA code had no alert in M5
// either), so the process never repeats it. Then the CalDAV listener's
// certificate nearing its end or past it.
func ProcessAlerts(l *Listener, accounts []AccountStatus, now time.Time) []Alert {
	out := []Alert{}
	if len(accounts) == 0 {
		out = append(out, Alert{Code: CodeNeedsLogin, Severity: SeverityError, Message: "not logged in to Proton", Fix: "run pcb login"})
	}
	if l != nil && l.TLS && l.CertNotAfter != nil {
		left, date := l.CertNotAfter.Sub(now), l.CertNotAfter.UTC().Format(time.DateOnly)
		switch {
		case left <= 0:
			out = append(out, Alert{Code: CodeTLSExpired, Severity: SeverityError, Message: "the CalDAV certificate expired on " + date, Fix: certFix})
		case left <= certWarn:
			out = append(out, Alert{Code: CodeTLSExpiring, Severity: SeverityWarning, Message: "the CalDAV certificate expires on " + date, Fix: certFix})
		}
	}
	SortAlerts(out)
	return out
}

// SortAlerts orders alerts: errors first, then by account, code and
// calendar ID.
func SortAlerts(a []Alert) {
	slices.SortStableFunc(a, func(a, b Alert) int {
		return cmp.Or(cmp.Compare(rank(a.Severity), rank(b.Severity)), strings.Compare(a.Account, b.Account),
			strings.Compare(a.Code, b.Code), strings.Compare(a.Calendar, b.Calendar))
	})
}

func healthAlerts(h *service.Health, now time.Time, add func(code, sev, msg, fix, cal string)) {
	if len(h.HumanVerification) > 0 {
		first := h.HumanVerification[0].Since
		sources := make([]string, 0, len(h.HumanVerification))
		for _, v := range h.HumanVerification {
			if v.Since.Before(first) {
				first = v.Since
			}
			sources = append(sources, v.Source)
		}
		add(CodeHumanVerification, SeverityError,
			fmt.Sprintf("Proton asks for a human verification (code 9001) since %s (%s): writes answer 503",
				first.UTC().Format(time.RFC3339), strings.Join(sources, ", ")),
			"open the Proton web app from this machine's network and complete the verification", "")
	}
	interval := time.Duration(h.IntervalSeconds) * time.Second
	if interval <= 0 {
		interval = service.ActiveInterval
	}
	for _, c := range h.Calendars {
		if h.Serve == service.ServeServing {
			if ref, ok := lastGood(h.Since, c.SyncedAt); ok {
				if d := now.Sub(ref); d > stalledAfter*interval {
					add(CodeSyncStalled, SeverityWarning, fmt.Sprintf("no successful sync of %s for %s", c.Name, d.Round(time.Second)),
						"see last_pass and the logs (pcb service logs)", c.ID)
				}
			}
		}
		if c.KeysError != "" {
			add(CodeCalendarKeysFailed, SeverityError,
				fmt.Sprintf("the keys of %s did not unlock (%s): its events are placeholders", c.Name, c.KeysError),
				"run pcb login again; if it stays, check the calendar in the Proton web app", c.ID)
		}
		if c.Gate == "undecided" {
			add(CodeWriteGateUndecided, SeverityWarning, fmt.Sprintf("writes to %s answer 503: %s", c.Name, c.GateReason),
				"check the calendar's keys in the Proton web app, then restart pcb (pcb service restart, or restart its container)", c.ID)
		}
	}
}

// lastGood is the later of Serve's start and the last successful sync,
// whichever are known (ruling M5-P7).
func lastGood(since, synced *time.Time) (time.Time, bool) {
	switch {
	case since == nil && synced == nil:
		return time.Time{}, false
	case since == nil:
		return *synced, true
	case synced == nil || since.After(*synced):
		return *since, true
	}
	return *synced, true
}

func rank(severity string) int {
	if severity == SeverityError {
		return 0
	}
	return 1
}
