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
	return &service.Health{Serve: service.ServeServing, Since: ptr(now.Add(-time.Hour)), IntervalSeconds: 30, CadenceSeconds: 30,
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
		{"clients file unreadable", ctl.AlertInput{Login: service.Status{Phase: "ready", ClientsFile: "/s/clients.json"}},
			[]ctl.Alert{{Code: "clients-file-unreadable", Severity: "error",
				Message: "the CalDAV clients file cannot be loaded: every CalDAV request answers 503",
				Fix:     "fix or restore /s/clients.json (pcb client list shows the error)"}}},
		{"needs totp is no alert", ctl.AlertInput{Login: service.Status{Phase: "needs-totp"}}, nil},
		{"captcha", ctl.AlertInput{Login: service.Status{Phase: "needs-captcha", CaptchaURL: "https://verify.proton.me/?token=x"}},
			[]ctl.Alert{{Code: "needs-captcha", Severity: "error",
				Message: "Proton asks for a CAPTCHA: solve it at https://verify.proton.me/?token=x",
				Fix:     "pcb login resume --captcha-token TOKEN (the token parameter of that URL)"}}},
		{"human verification", ctl.AlertInput{Login: ready, Health: &service.Health{Serve: service.ServeServing, Since: ptr(now),
			IntervalSeconds: 30, CadenceSeconds: 30, HumanVerification: []service.HumanVerification{
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
			Serve: service.ServeServing, Since: ptr(now.Add(-time.Minute)), IntervalSeconds: 30, CadenceSeconds: 30,
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
		{"a stopped account", ctl.AlertInput{Account: "ab12cd34", Login: ready, Stopped: &service.StopInfo{Code: 20, Message: "the cache is damaged", Hint: "remove pcb.db", At: now}},
			[]ctl.Alert{{Account: "ab12cd34", Code: "account-stopped", Severity: "error", Message: "the cache is damaged", Fix: "remove pcb.db"}}},
		{"every alert names its account", ctl.AlertInput{Account: "ab12cd34", Login: service.Status{Phase: "logged-out"}},
			[]ctl.Alert{{Account: "ab12cd34", Code: "needs-login", Severity: "error", Message: "not logged in to Proton", Fix: loginFix}}},
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

// Live finding 2026-11-13 (build bc0f196): with Proton unreachable the
// threshold followed the back-off wait and no alert showed after 3m41s. The
// threshold is 5 × the cadence, whatever the next wait is.
func TestSyncStalledIgnoresTheBackOffWait(t *testing.T) {
	t.Parallel()
	ready := service.Status{Phase: "ready"}
	health := func(cadence, next int, since time.Duration) *service.Health {
		h := serving(cal(func(c *service.CalendarHealth) { c.SyncedAt = ptr(now.Add(-since)) }))
		h.CadenceSeconds, h.IntervalSeconds = cadence, next
		return h
	}
	stalled := func(h *service.Health) bool {
		for _, a := range ctl.Alerts(ctl.AlertInput{Login: ready, Health: h, Now: now}) {
			if a.Code == "sync-stalled" {
				return true
			}
		}
		return false
	}
	// The measured sequence: next waits 31 s, 56 s, 2m6s after failed passes.
	for _, next := range []int{31, 56, 126, 600} {
		if !stalled(health(30, next, 3*time.Minute+41*time.Second)) {
			t.Errorf("no sync-stalled 3m41s after the last good sync with a %d s back-off wait, cadence 30 s", next)
		}
		if stalled(health(30, next, 150*time.Second)) {
			t.Errorf("sync-stalled at exactly 5 cadence intervals with a %d s back-off wait", next)
		}
	}
	if stalled(health(300, 300, 24*time.Minute)) || !stalled(health(300, 300, 25*time.Minute+time.Second)) {
		t.Error("an idle cadence (300 s) must keep a 5 × 300 s threshold")
	}
	if stalled(health(300, 600, 25*time.Minute)) {
		t.Error("a back-off wait moved an idle threshold")
	}
	if stalled(health(30, 126, time.Minute)) {
		t.Error("a good pass a minute ago is stalled")
	}
	// A wake: the loop is active again (not idle), the last good sync is 290 s
	// old, and the cadence still reported is the idle one until the woken pass ends.
	woken := health(300, 30, 290*time.Second)
	woken.Idle = false
	if stalled(woken) {
		t.Error("a wake alone raised sync-stalled")
	}
	// An older daemon reports no cadence: its idle loop keeps 5 × 300 s.
	idleOld := health(0, 300, 24*time.Minute)
	idleOld.Idle = true
	if stalled(idleOld) {
		t.Error("an idle daemon without cadence_seconds is stalled before 5 × 300 s")
	}
	idleOld = health(0, 300, 25*time.Minute+time.Second)
	idleOld.Idle = true
	if !stalled(idleOld) {
		t.Error("an idle daemon without cadence_seconds is not stalled past 5 × 300 s")
	}
	// A health without a cadence (older daemon) falls back to the active one.
	if !stalled(health(0, 126, 3*time.Minute+41*time.Second)) {
		t.Error("no cadence: want the active 30 s, not the back-off wait")
	}
}

// Errors first, then by code, then by calendar ID.
func TestAlertsAreOrdered(t *testing.T) {
	t.Parallel()
	h := &service.Health{Serve: service.ServeServing, Since: ptr(now.Add(-time.Hour)), IntervalSeconds: 30, CadenceSeconds: 30,
		HumanVerification: []service.HumanVerification{{Source: "sync", Since: now}},
		Calendars: []service.CalendarHealth{
			{ID: "cal-b", Name: "B", Gate: "undecided", GateReason: "r", KeysError: "k"},
			{ID: "cal-a", Name: "A", Gate: "undecided", GateReason: "r", KeysError: "k"},
		}}
	got := ctl.Alerts(ctl.AlertInput{Login: service.Status{Phase: "needs-login"}, Health: h, Now: now})
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
		"warning write-gate-undecided cal-a",
		"warning write-gate-undecided cal-b",
	}
	if !slices.Equal(order, want) {
		t.Errorf("order =\n%q\nwant\n%q", order, want)
	}
}

// The certificate's alerts belong to the process, not to an account.
func TestProcessAlerts(t *testing.T) {
	t.Parallel()
	ready := []ctl.AccountStatus{{AccountInfo: service.AccountInfo{ID: "a1", Phase: "ready"}}}
	for _, tc := range []struct {
		name string
		l    *ctl.Listener
		want []ctl.Alert // empty: no alert
	}{
		{"within 30 days", tlsListener(now.Add(30*24*time.Hour - time.Minute)),
			[]ctl.Alert{{Code: "tls-certificate-expiring", Severity: "warning", Message: "the CalDAV certificate expires on 2026-12-12", Fix: certFix}}},
		{"at exactly 30 days", tlsListener(now.Add(30 * 24 * time.Hour)),
			[]ctl.Alert{{Code: "tls-certificate-expiring", Severity: "warning", Message: "the CalDAV certificate expires on 2026-12-12", Fix: certFix}}},
		{"past 30 days", tlsListener(now.Add(30*24*time.Hour + time.Minute)), nil},
		{"expiring now is expired", tlsListener(now),
			[]ctl.Alert{{Code: "tls-certificate-expired", Severity: "error", Message: "the CalDAV certificate expired on 2026-11-12", Fix: certFix}}},
		{"expired", tlsListener(now.Add(-48 * time.Hour)),
			[]ctl.Alert{{Code: "tls-certificate-expired", Severity: "error", Message: "the CalDAV certificate expired on 2026-11-10", Fix: certFix}}},
		{"plain HTTP has no certificate", &ctl.Listener{Addr: "127.0.0.1:1080", URL: "http://localhost:1080"}, nil},
		{"no listener", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ctl.ProcessAlerts(tc.l, ready, now)
			if got == nil {
				t.Fatal("ProcessAlerts returned nil; want an empty list (alerts: [])")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ProcessAlerts =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

// Ruling R44: with no account at all (a fresh install), the process says
// it needs a login, as M5's one account did; an account that exists says
// it itself (its own needs-login, needs-captcha or account-unreadable), so
// the process never repeats it.
func TestProcessNeedsLoginWithNoAccount(t *testing.T) {
	t.Parallel()
	needsLogin := ctl.Alert{Code: "needs-login", Severity: "error", Message: "not logged in to Proton", Fix: loginFix}
	expired := ctl.Alert{Code: "tls-certificate-expired", Severity: "error", Message: "the CalDAV certificate expired on 2026-11-10", Fix: certFix}
	account := func(phase string, pending bool) []ctl.AccountStatus {
		return []ctl.AccountStatus{{AccountInfo: service.AccountInfo{ID: "a1", Phase: phase, Pending: pending}}}
	}
	for _, tc := range []struct {
		name     string
		l        *ctl.Listener
		accounts []ctl.AccountStatus
		want     []ctl.Alert
	}{
		{"no account", nil, nil, []ctl.Alert{needsLogin}},
		{"an empty list", nil, []ctl.AccountStatus{}, []ctl.Alert{needsLogin}},
		{"no account, errors sorted by code", tlsListener(now.Add(-48 * time.Hour)), nil, []ctl.Alert{needsLogin, expired}},
		{"a ready account", nil, account("ready", false), nil},
		{"a pending account", nil, account("needs-captcha", true), nil},
		{"a logged-out account says it itself", nil, account("logged-out", false), nil},
		{"a login waiting for its 2FA code", nil, account("needs-totp", false), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ctl.ProcessAlerts(tc.l, tc.accounts, now)
			if got == nil || !slices.Equal(got, tc.want) {
				t.Errorf("ProcessAlerts =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}

func TestKeyringLockedAlert(t *testing.T) {
	t.Parallel()
	got := ctl.Alerts(ctl.AlertInput{Account: "acc1", Login: service.Status{Phase: "ready", Secrets: "locked"}, Now: now})
	want := []ctl.Alert{{Account: "acc1", Code: "keyring-locked", Severity: "error", Message: "the desktop keyring is locked",
		Fix: "unlock it (log in to the desktop session), pcb retries every 30 s"}}
	if !slices.Equal(got, want) {
		t.Errorf("Alerts = %+v, want %+v", got, want)
	}
	if ctl.CodeKeyringLocked != "keyring-locked" {
		t.Errorf("CodeKeyringLocked = %q", ctl.CodeKeyringLocked)
	}
	// A sealed store raises none.
	if got := ctl.Alerts(ctl.AlertInput{Login: service.Status{Phase: "ready", Secrets: "sealed"}, Now: now}); len(got) != 0 {
		t.Errorf("sealed: Alerts = %+v, want none", got)
	}
}
