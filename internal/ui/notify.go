package ui

import (
	"regexp"
	"strings"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
)

// Notification is one desktop notification.
type Notification struct{ Account, Title, Body string }

// notified are the alerts that deserve a notification: the ones a person
// must act on to get the bridge going again.
var notified = map[string]bool{
	ctl.CodeNeedsLogin: true, ctl.CodeNeedsCaptcha: true, ctl.CodeHumanVerification: true,
	ctl.CodeSyncStalled: true, ctl.CodeKeyringLocked: true,
}

// Notifier decides which alerts of a View are new.
type Notifier struct{ seen map[string]bool }

// Next is the notifications for the error alerts of the codes above that
// were not in the previous view, keyed by account and code: an alert that
// clears and comes back notifies again. The title is the account's address,
// the body the alert's message and fix, without any link (a CAPTCHA URL
// holds a token).
func (n *Notifier) Next(v View) []Notification {
	var out []Notification
	now := map[string]bool{}
	add := func(account, title string, alerts []ctl.Alert) {
		for _, al := range alerts {
			if al.Severity != ctl.SeverityError || !notified[al.Code] {
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
