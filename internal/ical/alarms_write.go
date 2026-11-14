package ical

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
)

// Notification is one entry of a row's Notifications (research D): Type 0
// is an email, 1 a device notification; Trigger is an iCalendar duration
// relative to the event's start (negative before it). Email notifications
// are Proton-side (Fix 4, choice A): Proton's servers send them, a CalDAV
// client cannot act on them, so pcb neither serves nor writes them.
type Notification struct {
	Type    int    `json:"Type"`
	Trigger string `json:"Trigger"`
}

// Proton's notification limits (research D).
const (
	MaxNotifications = 10
	maxWeeks         = 999
	maxDays          = 6999
	maxHours         = 999
	maxMinutes       = 9999
)

// NotificationsFromAlarms maps a VEVENT's VALARMs to notifications, in
// order, the first MaxNotifications supported ones; dropped counts the
// rest. Supported: ACTION DISPLAY or AUDIO (Type 1; an EMAIL alarm is
// dropped: email notifications are Proton-side, see Notification), one
// TRIGGER that is a duration relative to the start (no VALUE=DATE-TIME,
// no RELATED=END), not after the start (an all-day event takes less than
// 24 hours after it: that day at that time), seconds cut away. The trigger
// is written on one unit for a timed event (the largest dividing it: W, D,
// H or M), as days plus a time of day for an all-day one, within Proton's
// per-unit bounds. The list is empty, never nil, when nothing is kept (the
// wire's []).
func NotificationsFromAlarms(alarms []Component, allDay bool) ([]Notification, int) {
	out := []Notification{}
	dropped := 0
	for _, a := range alarms {
		n, ok := notificationOf(a, allDay)
		if !ok || len(out) == MaxNotifications {
			dropped++
			continue
		}
		out = append(out, n)
	}
	return out, dropped
}

func notificationOf(a Component, allDay bool) (Notification, bool) {
	var actions, triggers []Property
	for _, p := range a.Props() {
		switch strings.ToUpper(p.Name) {
		case "ACTION":
			actions = append(actions, p)
		case "TRIGGER":
			triggers = append(triggers, p)
		}
	}
	if len(actions) != 1 || len(triggers) != 1 {
		return Notification{}, false
	}
	var n Notification
	switch strings.ToUpper(strings.TrimSpace(actions[0].Value)) {
	case "DISPLAY", "AUDIO":
		n.Type = 1
	default: // EMAIL among them
		return Notification{}, false
	}
	tr := triggers[0]
	if v, ok := param(tr.Params, "VALUE"); ok && !strings.EqualFold(v, "DURATION") {
		return Notification{}, false
	}
	if v, ok := param(tr.Params, "RELATED"); ok && !strings.EqualFold(v, "START") {
		return Notification{}, false
	}
	d, ok := parseDuration(strings.TrimSpace(tr.Value))
	if !ok {
		return Notification{}, false
	}
	offset := d.signed()
	if offset > 0 && (!allDay || offset >= 86400) {
		return Notification{}, false
	}
	n.Trigger, ok = triggerOf(offset/60, allDay)
	return n, ok
}

// triggerOf writes a signed whole-minute offset as a Proton trigger, false
// when a unit is over its bound.
func triggerOf(minutes int64, allDay bool) (string, bool) {
	sign := ""
	if minutes < 0 {
		sign, minutes = "-", -minutes
	}
	const day, week = 24 * 60, 7 * 24 * 60
	unit := func(v int64, before, after string, limit int64) (string, bool) {
		return sign + "P" + before + strconv.FormatInt(v, 10) + after, v <= limit
	}
	switch {
	case minutes == 0:
		return "PT0S", true
	case minutes%week == 0:
		return unit(minutes/week, "", "W", maxWeeks)
	case allDay:
		days, rest := minutes/day, minutes%day
		var b strings.Builder
		b.WriteString(sign + "P")
		if days > 0 {
			b.WriteString(strconv.FormatInt(days, 10) + "D")
		}
		if rest > 0 {
			b.WriteString("T")
			if rest/60 > 0 {
				b.WriteString(strconv.FormatInt(rest/60, 10) + "H")
			}
			if rest%60 > 0 {
				b.WriteString(strconv.FormatInt(rest%60, 10) + "M")
			}
		}
		return b.String(), days <= maxDays
	case minutes%day == 0:
		return unit(minutes/day, "", "D", maxDays)
	case minutes%60 == 0:
		return unit(minutes/60, "T", "H", maxHours)
	}
	return unit(minutes, "T", "M", maxMinutes)
}

// SplitEmail parts a notification list into its device notifications and
// its email ones (Type 0), each in order.
func SplitEmail(list []Notification) (device, email []Notification) {
	for _, n := range list {
		if n.Type == 0 {
			email = append(email, n)
		} else {
			device = append(device, n)
		}
	}
	return device, email
}

// SameNotifications compares two notification lists by meaning: the
// multisets of (Type, signed offset in seconds), whatever the order or the
// trigger's spelling (-P1D is -PT24H). A trigger that is not a duration
// makes the lists different.
func SameNotifications(a, b []Notification) bool {
	if len(a) != len(b) {
		return false
	}
	ka, okA := meanings(a)
	kb, okB := meanings(b)
	return okA && okB && slices.Equal(ka, kb)
}

type meaning struct {
	typ    int
	offset int64
}

func meanings(list []Notification) ([]meaning, bool) {
	out := make([]meaning, len(list))
	for i, n := range list {
		d, ok := parseDuration(strings.TrimSpace(n.Trigger))
		if !ok {
			return nil, false
		}
		out[i] = meaning{n.Type, d.signed()}
	}
	slices.SortFunc(out, func(x, y meaning) int {
		return cmp.Or(cmp.Compare(x.typ, y.typ), cmp.Compare(x.offset, y.offset))
	})
	return out, true
}
