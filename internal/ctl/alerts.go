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
	CodeNeedsCaptcha       = "needs-captcha"
	CodeHumanVerification  = "human-verification"
	CodeSyncStalled        = "sync-stalled"
	CodeCalendarKeysFailed = "calendar-keys-failed"
	CodeWriteGateUndecided = "write-gate-undecided"
	CodeClientsUnreadable  = "clients-file-unreadable"
	CodeTLSExpiring        = "tls-certificate-expiring"
	CodeTLSExpired         = "tls-certificate-expired"

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

// AlertInput is what the alerts are computed from: the login, and Serve's
// health and listener when a daemon answers (nil offline).
type AlertInput struct {
	Login    service.Status
	Health   *service.Health
	Listener *Listener
	Now      time.Time
}

// Alerts lists the problems in, errors first, then by code, then by
// calendar ID; never nil.
func Alerts(in AlertInput) []Alert {
	out := []Alert{}
	add := func(code, sev, msg, fix, cal string) {
		out = append(out, Alert{Code: code, Severity: sev, Message: msg, Fix: fix, Calendar: cal})
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
	if f := in.Login.ClientsFile; f != "" {
		add(CodeClientsUnreadable, SeverityError, "the CalDAV clients file cannot be loaded: every CalDAV request answers 503",
			"fix or restore "+f+" (pcb client list shows the error)", "")
	}
	if h := in.Health; h != nil {
		healthAlerts(h, in.Now, add)
	}
	if l := in.Listener; l != nil && l.TLS && l.CertNotAfter != nil {
		left, date := l.CertNotAfter.Sub(in.Now), l.CertNotAfter.UTC().Format(time.DateOnly)
		switch {
		case left <= 0:
			add(CodeTLSExpired, SeverityError, "the CalDAV certificate expired on "+date, certFix, "")
		case left <= certWarn:
			add(CodeTLSExpiring, SeverityWarning, "the CalDAV certificate expires on "+date, certFix, "")
		}
	}
	slices.SortStableFunc(out, func(a, b Alert) int {
		return cmp.Or(cmp.Compare(rank(a.Severity), rank(b.Severity)), strings.Compare(a.Code, b.Code), strings.Compare(a.Calendar, b.Calendar))
	})
	return out
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
	// The threshold follows the cadence, never IntervalSeconds: that is the
	// next wait, which grows while passes fail (live 2026-11-13).
	interval := time.Duration(h.CadenceSeconds) * time.Second
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
