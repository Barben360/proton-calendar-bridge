package ctl_test

import (
	"slices"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// serving is a Health of a Serve that began serving an hour ago, with a
// 30 s interval and cal as its one calendar.
func serving(cal service.CalendarHealth) *service.Health {
	return &service.Health{Serve: service.ServeServing, Since: ptr(now.Add(-time.Hour)), IntervalSeconds: 30,
		Calendars: []service.CalendarHealth{cal}}
}

func cal(mod func(*service.CalendarHealth)) service.CalendarHealth {
	c := service.CalendarHealth{ID: "cal-1", Name: "Work", Gate: "open", SyncedAt: ptr(now.Add(-time.Minute))}
	if mod != nil {
		mod(&c)
	}
	return c
}

func tlsListener(notAfter time.Time) *ctl.Listener {
	return &ctl.Listener{Addr: "0.0.0.0:8443", URL: "https://pcb.lan:8443/", TLS: true, Fingerprint: "AB:CD", CertNotAfter: &notAfter}
}

const (
	certFix  = "renew caldav.tls_cert; a generated one is renewed by restarting pcb"
	loginFix = "run pcb login"
)

func TestAlerts(t *testing.T) {
	t.Parallel()
	ready := service.Status{Phase: "ready"}
	stalledAt := func(d time.Duration) *service.Health { // last sync d ago, serving for longer
		return serving(cal(func(c *service.CalendarHealth) { c.SyncedAt = ptr(now.Add(-d)) }))
	}
	for _, tc := range []struct {
		name string
		in   ctl.AlertInput
		want []ctl.Alert // nil: no alert
	}{
		{"logged out", ctl.AlertInput{Login: service.Status{Phase: "logged-out"}},
			[]ctl.Alert{{Code: "needs-login", Severity: "error", Message: "not logged in to Proton", Fix: loginFix}}},
		{"needs login with its reason", ctl.AlertInput{Login: service.Status{Phase: "needs-login", Reason: "Proton ended the session"}},
			[]ctl.Alert{{Code: "needs-login", Severity: "error", Message: "Proton ended the session", Fix: loginFix}}},
		// A login fails the same way over a store this pcb cannot read (state-I1).
		{"store this pcb cannot read", ctl.AlertInput{Login: service.Status{Phase: "needs-login", Secrets: service.SecretsUnsupported,
			Reason: "secrets.sealed was written by a newer pcb"}},
			[]ctl.Alert{{Code: "needs-login", Severity: "error", Message: "secrets.sealed was written by a newer pcb", Fix: "upgrade pcb"}}},
		{"ready", ctl.AlertInput{Login: ready}, nil},
		{"needs totp is no alert", ctl.AlertInput{Login: service.Status{Phase: "needs-totp"}}, nil},
		{"captcha", ctl.AlertInput{Login: service.Status{Phase: "needs-captcha", CaptchaURL: "https://verify.proton.me/?token=x"}},
			[]ctl.Alert{{Code: "needs-captcha", Severity: "error",
				Message: "Proton asks for a CAPTCHA: solve it at https://verify.proton.me/?token=x",
				Fix:     "pcb login resume --captcha-token TOKEN (the token parameter of that URL)"}}},
		{"human verification", ctl.AlertInput{Login: ready, Health: &service.Health{Serve: service.ServeServing, Since: ptr(now),
			IntervalSeconds: 30, HumanVerification: []service.HumanVerification{
				{Source: "write", Since: now.Add(-time.Minute)}, {Source: "sync", Since: now.Add(-2 * time.Minute)}}}},
			[]ctl.Alert{{Code: "human-verification", Severity: "error",
				Message: "Proton asks for a human verification (code 9001) since 2026-11-12T11:58:00Z (write, sync): writes answer 503",
				Fix:     "open the Proton web app from this machine's network and complete the verification"}}},
		{"no human verification", ctl.AlertInput{Login: ready, Health: serving(cal(nil))}, nil},
		{"stalled past 5 intervals", ctl.AlertInput{Login: ready, Health: stalledAt(150*time.Second + time.Second)},
			[]ctl.Alert{{Code: "sync-stalled", Severity: "warning", Message: "no successful sync of Work for 2m31s",
				Fix: "see last_pass and the logs (pcb service logs)", Calendar: "cal-1"}}},
		{"5 intervals is not stalled", ctl.AlertInput{Login: ready, Health: stalledAt(150 * time.Second)}, nil},
		{"under 5 intervals is not stalled", ctl.AlertInput{Login: ready, Health: stalledAt(150*time.Second - time.Second)}, nil},
		{"never synced counts from serving since", ctl.AlertInput{Login: ready,
			Health: serving(cal(func(c *service.CalendarHealth) { c.SyncedAt = nil }))},
			[]ctl.Alert{{Code: "sync-stalled", Severity: "warning", Message: "no successful sync of Work for 1h0m0s",
				Fix: "see last_pass and the logs (pcb service logs)", Calendar: "cal-1"}}},
		{"serving since is later than the last sync", ctl.AlertInput{Login: ready, Health: &service.Health{
			Serve: service.ServeServing, Since: ptr(now.Add(-time.Minute)), IntervalSeconds: 30,
			Calendars: []service.CalendarHealth{cal(func(c *service.CalendarHealth) { c.SyncedAt = ptr(now.Add(-48 * time.Hour)) })}}}, nil},
		{"stopped is never stalled", ctl.AlertInput{Login: ready, Health: &service.Health{Serve: service.ServeStopped,
			Calendars: []service.CalendarHealth{cal(func(c *service.CalendarHealth) { c.SyncedAt = nil })}}}, nil},
		{"keys failed", ctl.AlertInput{Login: ready, Health: serving(cal(func(c *service.CalendarHealth) {
			c.KeysError, c.Gate, c.GateReason = "no key unlocked", "closed", "not a normal calendar (subscribed or holidays)"
		}))},
			[]ctl.Alert{{Code: "calendar-keys-failed", Severity: "error",
				Message: "the keys of Work did not unlock (no key unlocked): its events are placeholders",
				Fix:     "run pcb login again; if it stays, check the calendar in the Proton web app", Calendar: "cal-1"}}},
		{"gate undecided", ctl.AlertInput{Login: ready, Health: serving(cal(func(c *service.CalendarHealth) {
			c.Gate, c.GateReason = "undecided", "our member's address has no unlocked primary key"
		}))},
			[]ctl.Alert{{Code: "write-gate-undecided", Severity: "warning",
				Message: "writes to Work answer 503: our member's address has no unlocked primary key",
				Fix:     "check the calendar's keys in the Proton web app, then restart pcb (pcb service restart, or restart its container)", Calendar: "cal-1"}}},
		{"gate closed is no alert", ctl.AlertInput{Login: ready, Health: serving(cal(func(c *service.CalendarHealth) {
			c.Gate, c.GateReason = "closed", "our member lacks the Write permission"
		}))}, nil},
		{"certificate within 30 days", ctl.AlertInput{Login: ready, Listener: tlsListener(now.Add(30*24*time.Hour - time.Minute))},
			[]ctl.Alert{{Code: "tls-certificate-expiring", Severity: "warning", Message: "the CalDAV certificate expires on 2026-12-12", Fix: certFix}}},
		{"certificate at exactly 30 days", ctl.AlertInput{Login: ready, Listener: tlsListener(now.Add(30 * 24 * time.Hour))},
			[]ctl.Alert{{Code: "tls-certificate-expiring", Severity: "warning", Message: "the CalDAV certificate expires on 2026-12-12", Fix: certFix}}},
		{"certificate past 30 days", ctl.AlertInput{Login: ready, Listener: tlsListener(now.Add(30*24*time.Hour + time.Minute))}, nil},
		{"certificate expiring now is expired", ctl.AlertInput{Login: ready, Listener: tlsListener(now)},
			[]ctl.Alert{{Code: "tls-certificate-expired", Severity: "error", Message: "the CalDAV certificate expired on 2026-11-12", Fix: certFix}}},
		{"certificate expired", ctl.AlertInput{Login: ready, Listener: tlsListener(now.Add(-48 * time.Hour))},
			[]ctl.Alert{{Code: "tls-certificate-expired", Severity: "error", Message: "the CalDAV certificate expired on 2026-11-10", Fix: certFix}}},
		{"plain HTTP has no certificate", ctl.AlertInput{Login: ready, Listener: &ctl.Listener{Addr: "127.0.0.1:1080", URL: "http://localhost:1080/"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.in.Now = now
			got := ctl.Alerts(tc.in)
			if got == nil {
				t.Fatal("Alerts returned nil; want an empty list (alerts: [])")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Alerts =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// Errors first, then by code, then by calendar ID.
func TestAlertsAreOrdered(t *testing.T) {
	t.Parallel()
	h := &service.Health{Serve: service.ServeServing, Since: ptr(now.Add(-time.Hour)), IntervalSeconds: 30,
		HumanVerification: []service.HumanVerification{{Source: "sync", Since: now}},
		Calendars: []service.CalendarHealth{
			{ID: "cal-b", Name: "B", Gate: "undecided", GateReason: "r", KeysError: "k"},
			{ID: "cal-a", Name: "A", Gate: "undecided", GateReason: "r", KeysError: "k"},
		}}
	got := ctl.Alerts(ctl.AlertInput{Login: service.Status{Phase: "needs-login"}, Health: h,
		Listener: tlsListener(now.Add(24 * time.Hour)), Now: now})
	var order []string
	for _, a := range got {
		order = append(order, a.Severity+" "+a.Code+" "+a.Calendar)
	}
	want := []string{
		"error calendar-keys-failed cal-a",
		"error calendar-keys-failed cal-b",
		"error human-verification ",
		"error needs-login ",
		"warning sync-stalled cal-a",
		"warning sync-stalled cal-b",
		"warning tls-certificate-expiring ",
		"warning write-gate-undecided cal-a",
		"warning write-gate-undecided cal-b",
	}
	if !slices.Equal(order, want) {
		t.Errorf("order =\n%q\nwant\n%q", order, want)
	}
}
