package ui

import (
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
)

// Notification is one desktop notification.
// Code is the alert's (ctl.Code*).
type Notification struct{ Account, Code, Title, Body string }

// notified are the alerts that deserve a notification, whatever their
// severity (a stalled sync is a warning): the ones a person must look at.
var notified = map[string]bool{
	ctl.CodeNeedsLogin: true, ctl.CodeNeedsCaptcha: true, ctl.CodeHumanVerification: true,
	ctl.CodeSyncStalled: true, ctl.CodeKeyringLocked: true, ctl.CodeKeyringUnavailable: true,
	ctl.CodeKeyUnavailable: true,
}

// Notifier decides which alerts of a View are new.
type Notifier struct {
	mu   sync.Mutex
	seen map[string]bool
}

// Next is the notifications for the alerts of the codes above that
// were not in the previous view, keyed by account and code (a pending add
// has none: its login is on screen where it was started): an alert that
// clears and comes back notifies again. The title is the account's address,
// the body the alert's message and fix, without any link (a CAPTCHA URL
// holds a token).
func (n *Notifier) Next(v View) []Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []Notification
	now := map[string]bool{}
	add := func(account, title string, alerts []ctl.Alert) {
		for _, al := range alerts {
			if !notified[al.Code] {
				continue
			}
			key := account + "\x00" + al.Code
			if now[key] {
				continue
			}
			now[key] = true
			if !n.seen[key] {
				out = append(out, Notification{Account: account, Code: al.Code, Title: title, Body: body(al)})
			}
		}
	}
	add("", "pcb", v.Alerts)
	for _, a := range v.Accounts {
		if a.Pending {
			// A first login waiting for its CAPTCHA or 2FA code: the wizard
			// (or pcb login) that started it is on that step.
			continue
		}
		title := a.Address
		if title == "" {
			title = a.ID
		}
		add(a.ID, title, a.Alerts)
	}
	n.seen = now
	return out
}

var link = regexp.MustCompile(`https?://\S+`)

func body(al ctl.Alert) string {
	msg := strings.TrimSpace(link.ReplaceAllString(al.Message, "the link in the pcb-ui window"))
	if al.Fix == "" {
		return msg
	}
	return msg + "\n" + al.Fix
}

// Send sends each notification with send and logs it: at Info "notification
// sent" with its alert and the account's local ID ("" for the process), at
// Warn "notification failed" with the error. The title (an address) and the
// body are never logged.
func Send(log *slog.Logger, notes []Notification, send func(Notification) error) {
	for _, n := range notes {
		if err := send(n); err != nil {
			log.Warn("notification failed", "alert", n.Code, "account", n.Account, "err", err)
			continue
		}
		log.Info("notification sent", "alert", n.Code, "account", n.Account)
	}
}
