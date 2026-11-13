package ui_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

var now = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func ago(d time.Duration) *time.Time { t := now.Add(-d); return &t }

// ready is a signed-in account whose last pass was age ago, every interval
// seconds.
func ready(id string, age time.Duration, interval int) ctl.AccountStatus {
	return ctl.AccountStatus{
		AccountInfo: service.AccountInfo{ID: id, Address: id + "@proton.test", Phase: "ready", CalDAV: true},
		Login:       service.Status{Phase: "ready", Secrets: "ok"},
		Health: &service.Health{Serve: service.ServeServing, IntervalSeconds: interval,
			LastPass: &service.PassHealth{At: *ago(age), OK: true}},
		Alerts: []ctl.Alert{},
	}
}

func status(accts ...ctl.AccountStatus) ctl.Status {
	return ctl.Status{Accounts: accts, Alerts: []ctl.Alert{}}
}

func errAlert(account, code string) ctl.Alert {
	return ctl.Alert{Account: account, Code: code, Severity: ctl.SeverityError, Message: "msg " + code, Fix: "fix " + code}
}

func TestTrayAttentionOnErrorAlert(t *testing.T) {
	a := ready("a", time.Minute, 300)
	if got := ui.Build(status(a), now).Tray; got != ui.TrayOK {
		t.Fatalf("control: Tray = %q, want ok", got)
	}
	b := a
	b.Alerts = []ctl.Alert{errAlert("a", ctl.CodeNeedsLogin)}
	v := ui.Build(status(b), now)
	if v.Tray != ui.TrayAttention || v.Accounts[0].Dot != ui.TrayAttention {
		t.Errorf("account alert: Tray=%q Dot=%q, want attention both", v.Tray, v.Accounts[0].Dot)
	}
	s := status(a)
	s.Alerts = []ctl.Alert{errAlert("", ctl.CodeTLSExpired)}
	if got := ui.Build(s, now).Tray; got != ui.TrayAttention {
		t.Errorf("process alert: Tray = %q, want attention", got)
	}
	w := a
	w.Alerts = []ctl.Alert{{Account: "a", Code: ctl.CodeSyncStalled, Severity: ctl.SeverityWarning}}
	if got := ui.Build(status(w), now).Tray; got != ui.TrayOK {
		t.Errorf("warning only: Tray = %q, want ok", got)
	}
}

func TestTraySyncingBeforeFirstPass(t *testing.T) {
	none := ready("a", 0, 300)
	none.Health.LastPass = nil
	v := ui.Build(status(none), now)
	if v.Tray != ui.TraySyncing || v.Accounts[0].Dot != ui.TraySyncing || v.Accounts[0].Short != "syncing…" {
		t.Errorf("no pass yet: %q %q %q", v.Tray, v.Accounts[0].Dot, v.Accounts[0].Short)
	}
	late := ready("a", 10*time.Minute, 300) // older than its interval
	if got := ui.Build(status(late), now).Tray; got != ui.TraySyncing {
		t.Errorf("pass older than the interval: Tray = %q, want syncing", got)
	}
	mixed := ui.Build(status(ready("b", time.Minute, 300), late), now)
	if mixed.Tray != ui.TraySyncing || mixed.Accounts[0].Dot != ui.TrayOK {
		t.Errorf("one account late: Tray=%q first Dot=%q", mixed.Tray, mixed.Accounts[0].Dot)
	}
	// An error outranks syncing.
	late.Alerts = []ctl.Alert{errAlert("a", ctl.CodeNeedsCaptcha)}
	if got := ui.Build(status(late), now).Tray; got != ui.TrayAttention {
		t.Errorf("syncing with an error: Tray = %q, want attention", got)
	}
}

func TestTrayOK(t *testing.T) {
	v := ui.Build(status(ready("a", time.Minute, 300), ready("b", 2*time.Minute, 300)), now)
	if v.Tray != ui.TrayOK {
		t.Errorf("Tray = %q, want ok", v.Tray)
	}
	if got := ui.Build(status(), now).Tray; got != ui.TrayOK {
		t.Errorf("no account: Tray = %q, want ok", got)
	}
}

func TestShortTexts(t *testing.T) {
	with := func(a ctl.AccountStatus, f func(*ctl.AccountStatus)) ctl.AccountStatus { f(&a); return a }
	tests := []struct {
		name string
		acct ctl.AccountStatus
		want string
	}{
		{"just now", ready("a", 20*time.Second, 300), "synced just now"},
		{"minutes", ready("a", 3*time.Minute+10*time.Second, 300), "synced 3 min ago"},
		{"59 minutes", ready("a", 59*time.Minute, 7200), "synced 59 min ago"},
		{"hours", ready("a", 150*time.Minute, 86400), "synced 2 h ago"},
		{"60 minutes", ready("a", 60*time.Minute, 7200), "synced 1 h ago"},
		{"syncing", with(ready("a", 0, 300), func(a *ctl.AccountStatus) { a.Health.LastPass = nil }), "syncing…"},
		{"needs login", with(ready("a", 0, 300), func(a *ctl.AccountStatus) { a.Phase, a.Login.Phase = "needs-login", "needs-login" }), "needs login"},
		{"logged out", with(ready("a", 0, 300), func(a *ctl.AccountStatus) { a.Phase, a.Login.Phase = "logged-out", "logged-out" }), "needs login"},
		{"needs totp", with(ready("a", 0, 300), func(a *ctl.AccountStatus) { a.Phase, a.Login.Phase = "needs-totp", "needs-totp" }), "needs 2FA code"},
		{"needs captcha", with(ready("a", 0, 300), func(a *ctl.AccountStatus) { a.Phase, a.Login.Phase = "needs-captcha", "needs-captcha" }), "needs CAPTCHA"},
		{"caldav off", with(ready("a", time.Minute, 300), func(a *ctl.AccountStatus) { a.CalDAV = false }), "CalDAV off"},
		{"stopped", with(ready("a", time.Minute, 300), func(a *ctl.AccountStatus) {
			a.Stopped = &service.StopInfo{Message: "the session was revoked"}
		}), "stopped: the session was revoked"},
		{"keyring locked", with(ready("a", time.Minute, 300), func(a *ctl.AccountStatus) { a.Login.Secrets = "locked" }), "keyring locked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ui.Build(status(tt.acct), now).Accounts[0].Short; got != tt.want {
				t.Errorf("Short = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccountFields(t *testing.T) {
	a := ready("a", time.Minute, 300)
	a.URL = "https://127.0.0.1:1/a/"
	a.Health.Calendars = []service.CalendarHealth{{Name: "Home", SyncedAt: ago(30 * time.Minute)}, {Name: "Work", SyncedAt: ago(5 * time.Minute)}}
	s := status(a)
	s.Listener = &ctl.Listener{Addr: "127.0.0.1:1", URL: "https://127.0.0.1:1", TLS: true, Fingerprint: "AA:BB"}
	v := ui.Build(s, now)
	got := v.Accounts[0]
	if got.Username != "a@proton.test" || got.Address != "a@proton.test" || got.URL != a.URL || got.Fingerprint != "AA:BB" {
		t.Errorf("credentials: %+v", got)
	}
	if !got.SignedIn || got.Pending || !got.CalDAV || got.ID != "a" || v.Listener != s.Listener {
		t.Errorf("flags: %+v", got)
	}
	if got.LastSync == nil || !got.LastSync.Equal(a.Health.LastPass.At) {
		t.Errorf("LastSync = %v, want the last pass %v", got.LastSync, a.Health.LastPass.At)
	}
	if len(got.Calendars) != 2 || got.Calendars[0].LastSync != "synced 30 min ago" {
		t.Errorf("calendars = %+v", got.Calendars)
	}
	s.Listener = &ctl.Listener{Addr: "127.0.0.1:1", URL: "http://127.0.0.1:1", Fingerprint: "stale"}
	if fp := ui.Build(s, now).Accounts[0].Fingerprint; fp != "" {
		t.Errorf("Fingerprint without TLS = %q, want empty", fp)
	}
}

func TestCalendarBadgeFromGate(t *testing.T) {
	a := ready("a", time.Minute, 300)
	a.Health.Calendars = []service.CalendarHealth{
		{Name: "Open", Gate: "open", GateReason: "stale reason", SyncedAt: ago(7 * time.Minute)},
		{Name: "Closed", Gate: "closed", GateReason: "our member lacks the Write permission", Error: "boom"},
		{Name: "Undecided", Gate: "undecided", GateReason: "the calendar keys did not unlock"},
	}
	cals := ui.Build(status(a), now).Accounts[0].Calendars
	want := []ui.CalendarView{
		{Name: "Open", ReadOnly: false, Badge: "", LastSync: "synced 7 min ago"},
		{Name: "Closed", ReadOnly: true, Badge: "our member lacks the Write permission", Error: "boom"},
		{Name: "Undecided", ReadOnly: true, Badge: "the calendar keys did not unlock"},
	}
	if len(cals) != len(want) {
		t.Fatalf("calendars = %+v", cals)
	}
	for i := range want {
		if cals[i] != want[i] {
			t.Errorf("calendar %d = %+v, want %+v", i, cals[i], want[i])
		}
	}
}

func TestNotifyOncePerOccurrence(t *testing.T) {
	var n ui.Notifier
	bad := ready("a", time.Minute, 300)
	bad.Alerts = []ctl.Alert{errAlert("a", ctl.CodeNeedsLogin)}
	bad.Phase = "needs-login"
	v := ui.Build(status(bad), now)
	if got := n.Next(v); len(got) != 1 {
		t.Fatalf("first view: %d notifications, want 1", len(got))
	}
	if got := n.Next(v); len(got) != 0 {
		t.Fatalf("same view again: %d notifications, want 0", len(got))
	}
	if got := n.Next(ui.Build(status(ready("a", time.Minute, 300)), now)); len(got) != 0 {
		t.Fatalf("cleared: %d notifications, want 0", len(got))
	}
	if got := n.Next(v); len(got) != 1 {
		t.Fatalf("back again: %d notifications, want 1", len(got))
	}
	// Another account with the same code is its own occurrence.
	other := bad
	other.ID, other.Address = "b", "b@proton.test"
	other.Alerts = []ctl.Alert{errAlert("b", ctl.CodeNeedsLogin)}
	if got := n.Next(ui.Build(status(bad, other), now)); len(got) != 1 || got[0].Account != "b" {
		t.Fatalf("second account: %+v, want one for b", got)
	}
}

func TestNotifyTitleAndBody(t *testing.T) {
	var n ui.Notifier
	a := ready("a", time.Minute, 300)
	a.Alerts = []ctl.Alert{errAlert("a", ctl.CodeKeyringLocked)}
	got := n.Next(ui.Build(status(a), now))
	if len(got) != 1 || got[0].Title != "a@proton.test" || got[0].Account != "a" ||
		!strings.Contains(got[0].Body, "msg keyring-locked") || !strings.Contains(got[0].Body, "fix keyring-locked") {
		t.Fatalf("notification = %+v", got)
	}
}

func TestNotifyCodes(t *testing.T) {
	for code, want := range map[string]bool{
		ctl.CodeNeedsLogin: true, ctl.CodeNeedsCaptcha: true, ctl.CodeHumanVerification: true,
		ctl.CodeKeyringLocked:  true,
		ctl.CodeAccountStopped: false, ctl.CodeTLSExpired: false, ctl.CodeClientsUnreadable: false,
	} {
		var n ui.Notifier
		a := ready("a", time.Minute, 300)
		a.Alerts = []ctl.Alert{errAlert("a", code)}
		if got := len(n.Next(ui.Build(status(a), now))) == 1; got != want {
			t.Errorf("%s: notified = %v, want %v", code, got, want)
		}
	}
}

// A stalled sync is a warning in the ctl rules, and still notifies.
func TestNotifyStalledSyncWarning(t *testing.T) {
	a := ready("a", time.Minute, 30)
	a.Health.Since = ago(time.Hour)
	a.Health.Calendars = []service.CalendarHealth{{ID: "c1", Name: "Home", SyncedAt: ago(time.Hour)}}
	a.Alerts = ctl.Alerts(ctl.AlertInput{Account: "a", Login: a.Login, Health: a.Health, Now: now})
	if len(a.Alerts) != 1 || a.Alerts[0].Code != ctl.CodeSyncStalled || a.Alerts[0].Severity != ctl.SeverityWarning {
		t.Fatalf("control: alerts = %+v, want one sync-stalled warning", a.Alerts)
	}
	var n ui.Notifier
	got := n.Next(ui.Build(status(a), now))
	if len(got) != 1 || got[0].Title != "a@proton.test" || !strings.Contains(got[0].Body, "no successful sync of Home") {
		t.Fatalf("notifications = %+v, want the stalled sync", got)
	}
}

func TestNotifyIgnoresOtherCodes(t *testing.T) {
	var n ui.Notifier
	a := ready("a", time.Minute, 300)
	a.Alerts = []ctl.Alert{{Account: "a", Code: ctl.CodeWriteGateUndecided, Severity: ctl.SeverityWarning, Message: "m", Fix: "f"}}
	if got := n.Next(ui.Build(status(a), now)); len(got) != 0 {
		t.Fatalf("an unlisted warning notified: %+v", got)
	}
}

func TestNotifierIsSafeConcurrently(t *testing.T) {
	var n ui.Notifier
	a := ready("a", time.Minute, 300)
	a.Alerts = []ctl.Alert{errAlert("a", ctl.CodeNeedsLogin)}
	v := ui.Build(status(a), now)
	var wg sync.WaitGroup
	var total atomic.Int32
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				total.Add(int32(len(n.Next(v))))
			}
		}()
	}
	wg.Wait()
	if total.Load() != 1 {
		t.Errorf("%d notifications for one occurrence, want 1", total.Load())
	}
}

func TestNotificationCarriesNoSecret(t *testing.T) {
	const token = "SECRETTOKEN123"
	acct := ctl.AccountStatus{
		AccountInfo: service.AccountInfo{ID: "a", Address: "a@proton.test", Phase: "needs-captcha", Pending: true},
		Login:       service.Status{Phase: "needs-captcha", CaptchaURL: "https://verify.example/captcha?token=" + token},
	}
	acct.Alerts = ctl.Alerts(ctl.AlertInput{Account: "a", Login: acct.Login, Now: now})
	if !strings.Contains(acct.Alerts[0].Message, token) {
		t.Fatalf("control: the alert no longer holds the token: %+v", acct.Alerts[0])
	}
	var n ui.Notifier
	got := n.Next(ui.Build(status(acct), now))
	if len(got) != 1 {
		t.Fatalf("notifications = %+v, want 1", got)
	}
	if all := got[0].Title + got[0].Body; strings.Contains(all, token) || strings.Contains(all, "verify.example") {
		t.Errorf("the notification holds the captcha URL: %+v", got[0])
	}
	if !strings.Contains(got[0].Body, "CAPTCHA") {
		t.Errorf("body = %q, want it to say what is asked", got[0].Body)
	}
}

func TestViewBridgeStopped(t *testing.T) {
	l := newLab(t)
	kill := make(chan struct{})
	var calls int
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, Listen: func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if calls++; err != nil || calls > 1 {
			return ln, err
		}
		return &dyingListener{Listener: ln, kill: kill, closed: make(chan struct{})}, nil
	}})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	l.login(t, b)
	if v := b.View(t.Context(), time.Now()); v.Bridge != "" || len(v.Accounts) != 1 {
		t.Fatalf("running: Bridge=%q accounts=%d", v.Bridge, len(v.Accounts))
	}
	close(kill)
	deadline := time.Now().Add(10 * time.Second)
	for b.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	v := b.View(t.Context(), time.Now())
	if want := "bridge stopped: " + b.Err().Error(); v.Bridge != want || !strings.Contains(v.Bridge, "listener failed") {
		t.Errorf("Bridge = %q, want %q", v.Bridge, want)
	}
	if v.Tray != ui.TrayAttention {
		t.Errorf("Tray = %q, want attention", v.Tray)
	}
}

// credsOK says whether the account URL accepts password.
func (l *lab) status(t *testing.T, b *ui.Bridge, id, password string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://"+b.Info().Addr+service.AccountPath(id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("a@proton.test", password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestPasswordCreatesDefaultWhenMissing(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	id := l.login(t, b)
	api, _, err := b.Hub().Account(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.RemoveClient(t.Context(), "default"); err != nil {
		t.Fatalf("the login made no default client: %v", err)
	}
	pw, err := ui.NewActions(b).Password(t.Context(), id)
	if err != nil || pw == "" {
		t.Fatalf("Password = %q, %v", pw, err)
	}
	if got := l.status(t, b, id, pw); got == http.StatusUnauthorized {
		t.Errorf("the created password is refused: %d", got)
	}
	again, err := ui.NewActions(b).Password(t.Context(), id)
	if err != nil || again != pw {
		t.Errorf("a second Password = %q, %v; want the same %q (no second client)", again, err, pw)
	}
	list, _ := api.Clients(t.Context())
	if len(list) != 1 {
		t.Errorf("clients = %+v, want one", list)
	}
}

func TestNewPasswordStopsTheOldOne(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	id := l.login(t, b)
	act := ui.NewActions(b)
	old, err := act.Password(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got := l.status(t, b, id, old); got == http.StatusUnauthorized {
		t.Fatalf("control: the shown password is refused: %d", got)
	}
	fresh, err := act.NewPassword(t.Context(), id)
	if err != nil || fresh == "" || fresh == old {
		t.Fatalf("NewPassword = %q, %v (old %q)", fresh, err, old)
	}
	if got := l.status(t, b, id, old); got != http.StatusUnauthorized {
		t.Errorf("the old password answers %d, want 401", got)
	}
	if got := l.status(t, b, id, fresh); got == http.StatusUnauthorized {
		t.Errorf("the new password is refused: %d", got)
	}
}

func TestAccountActionsGoThroughTheHub(t *testing.T) {
	l := newLab(t)
	b := l.start(t)
	id := l.login(t, b)
	act := ui.NewActions(b)
	if err := act.SetCalDAV(t.Context(), id, false); err != nil {
		t.Fatalf("SetCalDAV: %v", err)
	}
	if got := b.Hub().Accounts()[0].CalDAV; got {
		t.Errorf("CalDAV = true after SetCalDAV(false)")
	}
	if err := act.SignOut(t.Context(), id); err != nil {
		t.Fatalf("SignOut: %v", err)
	}
	if got := b.Hub().Accounts()[0].Phase; got == "ready" {
		t.Errorf("Phase = %q after SignOut", got)
	}
	if err := act.Remove(t.Context(), id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if n := len(b.Hub().Accounts()); n != 0 {
		t.Errorf("%d accounts after Remove", n)
	}
}

func TestActionsRefuseWhenTheBridgeIsNotRunning(t *testing.T) {
	l := newLab(t)
	never := l.bridge(t, nil)
	stopped := l.start(t)
	if err := stopped.Stop(); err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]*ui.Bridge{"never started": never, "stopped": stopped} {
		t.Run(name, func(t *testing.T) { refused(t, ui.NewActions(b)) })
	}
}

func TestActionsRefuseAfterTheDaemonEnded(t *testing.T) {
	l := newLab(t)
	kill := make(chan struct{})
	var calls int
	b := ui.NewBridge(ui.BridgeOptions{Env: l.env, Listen: func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if calls++; err != nil || calls > 1 {
			return ln, err
		}
		return &dyingListener{Listener: ln, kill: kill, closed: make(chan struct{})}, nil
	}})
	t.Cleanup(func() { _ = b.Stop() })
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	id := l.login(t, b)
	close(kill)
	deadline := time.Now().Add(10 * time.Second)
	for b.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if b.Err() == nil {
		t.Fatal("the daemon did not end")
	}
	act := ui.NewActions(b)
	err := act.SetCalDAV(t.Context(), id, false)
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != exitcode.Blocked || !strings.Contains(err.Error(), "the bridge is not running") || ee.Hint == "" {
		t.Errorf("SetCalDAV after the daemon ended = %v, want the Blocked not-running error with a hint", err)
	}
	refused(t, act)
}

func refused(t *testing.T, act ui.Actions) {
	t.Helper()
	for name, err := range map[string]error{
		"SignOut":     act.SignOut(context.Background(), "a"),
		"Remove":      act.Remove(context.Background(), "a"),
		"SetCalDAV":   act.SetCalDAV(context.Background(), "a", true),
		"Password":    func() error { _, err := act.Password(context.Background(), "a"); return err }(),
		"NewPassword": func() error { _, err := act.NewPassword(context.Background(), "a"); return err }(),
	} {
		if err == nil || !strings.Contains(err.Error(), "the bridge is not running") || !hasHint(err) {
			t.Errorf("%s: err = %v, want the bridge is not running", name, err)
		}
		// Restart is in Settings, not in the tray menu (review M1).
		if h := exitcode.HintOf(err); !strings.Contains(h, "Settings → Restart bridge") {
			t.Errorf("%s: hint = %q, want it to point at Settings → Restart bridge", name, h)
		}
	}
}

func hasHint(err error) bool {
	var ee *exitcode.Error
	return errors.As(err, &ee) && ee.Hint != ""
}
