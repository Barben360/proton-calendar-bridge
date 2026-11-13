package ui

import (
	"regexp"
	"strings"
	"sync"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
)

// Notification is one desktop notification.
type Notification struct{ Account, Title, Body string }

// notified are the alerts that deserve a notification, whatever their
// severity (a stalled sync is a warning): the ones a person must look at.
var notified = map[string]bool{
	ctl.CodeNeedsLogin: true, ctl.CodeNeedsCaptcha: true, ctl.CodeHumanVerification: true,
	ctl.CodeSyncStalled: true, ctl.CodeKeyringLocked: true, ctl.CodeKeyringUnavailable: true,
}

// Notifier decides which alerts of a View are new.
type Notifier struct {
	mu   sync.Mutex
	seen map[string]bool
}

// Next is the notifications for the alerts of the codes above that
// were not in the previous view, keyed by account and code: an alert that
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
				out = append(out, Notification{Account: account, Title: title, Body: body(al)})
			}
		}
	}
	add("", "pcb", v.Alerts)
	for _, a := range v.Accounts {
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
