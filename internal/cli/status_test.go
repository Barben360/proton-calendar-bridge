package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

type statusStub struct{ stubAPI }

func (*statusStub) Status(context.Context) (service.Status, error) {
	at := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	return service.Status{Phase: "needs-captcha", Reason: "Proton asks for a CAPTCHA", Username: "alice", AccountID: "id1",
		Secrets: "sealed", KeyFile: "/k/secrets.key", CaptchaURL: "https://verify.proton.me/?t=1", LastLogin: &at}, nil
}

func TestStatusTextIsOneKeyValuePerLine(t *testing.T) {
	t.Parallel()
	r := stubRig(t, nil)
	r.svc = &statusStub{}
	r.want(exitcode.OK, "status")
	got := r.out.String()
	for _, want := range []string{
		"phase: needs-captcha\n", "reason: Proton asks for a CAPTCHA\n", "username: alice\n", "account_id: id1\n",
		"secrets: sealed\n", "key_file: /k/secrets.key\n", "unattended: false\n",
		"captcha_url: https://verify.proton.me/?t=1\n", "last_login: 2026-11-11T10:00:00Z\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "last_relogin") {
		t.Errorf("an unset field is printed:\n%s", got)
	}
}

func TestStatusWhenLoggedOutExits0(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "status", "--json")
	var st service.Status
	mustUnmarshal(t, r.envelope().Data, &st)
	if st.Phase != "logged-out" {
		t.Errorf("status = %+v", st)
	}
}

func TestLogoutText(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "logout")
	if !strings.Contains(r.out.String(), "Not logged in") {
		t.Errorf("stdout = %q", r.out.String())
	}
	r.want(exitcode.OK, "login", "--non-interactive")
	r.want(exitcode.OK, "logout")
	if !strings.Contains(r.out.String(), "Logged out") {
		t.Errorf("stdout = %q", r.out.String())
	}
}

type neverSyncedStub struct{ stubAPI }

func (*neverSyncedStub) Status(context.Context) (service.Status, error) {
	return service.Status{Phase: "ready", Username: "alice", AccountID: "id1", Secrets: "sealed"}, nil
}

// Logged in but never synced: status says what to run.
func TestStatusNeverSynced(t *testing.T) {
	t.Parallel()
	r := stubRig(t, nil)
	r.svc = &neverSyncedStub{}
	r.want(exitcode.OK, "status")
	if !strings.Contains(r.out.String(), "sync: never (run pcb sync)\n") {
		t.Errorf("status = %q, want the never-synced line", r.out.String())
	}
	r.svc = &statusStub{} // no account: no sync line
	r.want(exitcode.OK, "status")
	if strings.Contains(r.out.String(), "sync:") {
		t.Errorf("logged-out status shows a sync line: %q", r.out.String())
	}
}

// daemonAPI is the service a test daemon answers from.
type daemonAPI struct {
	stubAPI
	status service.Status
	health service.Health
}

func (d *daemonAPI) Status(context.Context) (service.Status, error) { return d.status, nil }
func (d *daemonAPI) Health(context.Context) (service.Health, error) { return d.health, nil }

// daemonNow is the clock of the test daemons.
var daemonNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

// startDaemon serves a control socket on api at the rig's socket path, as
// pcb serve does, until the test ends.
func startDaemon(t *testing.T, r *rig, api service.API, ln *ctl.Listener) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	sl, err := ctl.Listen(r.dirs.Socket())
	if err != nil {
		t.Fatal(err)
	}
	srv := &ctl.Server{API: api, Listener: func() *ctl.Listener { return ln }, Now: func() time.Time { return daemonNow },
		Daemon: ctl.Daemon{PID: 4242, Version: "v1.2.3", StartedAt: daemonNow.Add(-2 * time.Hour), LogLevel: "info", Socket: r.dirs.Socket()}}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(sl) }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the daemon's Serve returned %v", err)
		}
	})
}

// A login the daemon holds for its 2FA code shows until when.
func TestStatusShowsWhenTheHeldLoginExpires(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{statusPhase: "logged-out"})
	exp := daemonNow.Add(5 * time.Minute)
	startDaemon(t, r, &daemonAPI{status: service.Status{Phase: "needs-totp", LoginExpires: &exp}}, nil)
	r.want(exitcode.OK, "status")
	if !strings.Contains(r.out.String(), "login_expires: 2026-11-12T12:05:00Z\n") {
		t.Errorf("status lacks the expiry:\n%s", r.out.String())
	}
}

func TestStatusFallsBackOffline(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{statusPhase: "logged-out"})
	r.want(exitcode.OK, "status", "--json")
	var data map[string]json.RawMessage
	mustUnmarshal(t, r.envelope().Data, &data)
	if d, ok := data["daemon"]; !ok || string(d) != "null" {
		t.Errorf("daemon = %s (present %v), want null", d, ok)
	}
	if string(data["phase"]) != `"logged-out"` {
		t.Errorf("phase = %s, want the flat login field", data["phase"])
	}
	var alerts []ctl.Alert
	mustUnmarshal(t, data["alerts"], &alerts)
	if len(alerts) != 1 || alerts[0].Code != "needs-login" {
		t.Errorf("alerts = %+v, want the login's", alerts)
	}
	for _, absent := range []string{"health", "listener"} {
		if _, has := data[absent]; has {
			t.Errorf("the offline view has %s: %s", absent, data[absent])
		}
	}

	r.want(exitcode.OK, "status")
	for _, want := range []string{
		"phase: logged-out\n",
		"daemon: not running (offline view)\n",
		"alert: error needs-login: not logged in to Proton (fix: run pcb login)",
	} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, r.out.String())
		}
	}
}

func TestStatusShowsTheDaemon(t *testing.T) {
	t.Parallel()
	// The offline service says logged out: what status shows must come
	// from the daemon.
	r := stubRig(t, &stubAPI{statusPhase: "logged-out"})
	since, synced, notAfter := daemonNow.Add(-time.Hour), daemonNow.Add(-time.Minute), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	lag := 60
	api := &daemonAPI{
		status: service.Status{Phase: "ready", Username: "alice", Secrets: "sealed"},
		health: service.Health{Serve: service.ServeServing, Since: &since, IntervalSeconds: 30,
			Calendars: []service.CalendarHealth{
				{ID: "cal-1", Name: "Work", SyncedAt: &synced, LagSeconds: &lag, Gate: "closed", GateReason: "our member lacks the Write permission"},
				{ID: "cal-2", Name: "Home", Gate: "undecided", GateReason: "the keys were not loaded yet", ErrorClass: "network", Error: "dial tcp: refused"},
			},
			Render: service.RenderCounts{Undecryptable: 1, Unverified: 2, DuplicateMasters: 3}},
	}
	startDaemon(t, r, api, &ctl.Listener{Addr: "0.0.0.0:8443", URL: "https://pcb.lan:8443/", TLS: true, Fingerprint: "AB:CD", CertNotAfter: &notAfter})

	r.want(exitcode.OK, "status")
	got := r.out.String()
	for _, want := range []string{
		"phase: ready\n",
		"daemon: serving since 2026-11-12T11:00:00Z (pid 4242, version v1.2.3)\n",
		"listener: https://pcb.lan:8443/ (TLS, certificate until 2027-01-01, fingerprint AB:CD)\n",
		"calendar: Work (cal-1): synced 2026-11-12T11:59:00Z, lag 1m0s, write gate closed (our member lacks the Write permission)\n",
		"calendar: Home (cal-2): synced never, write gate undecided (the keys were not loaded yet), last error network: dial tcp: refused\n",
		"render: undecryptable 1, unverified 2, duplicate masters 3\n",
		"alert: warning sync-stalled: no successful sync of Home for 1h0m0s (fix: see last_pass and the logs (pcb service logs))\n",
		"alert: warning write-gate-undecided: writes to Home answer 503: the keys were not loaded yet (fix: check the calendar's keys in the Proton web app, then restart pcb (pcb service restart, or restart its container))",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "offline") || strings.Contains(got, "needs-login") {
		t.Errorf("status used the offline service:\n%s", got)
	}
	if i, j := strings.Index(got, "sync-stalled"), strings.Index(got, "write-gate-undecided"); i < 0 || j < i {
		t.Errorf("alerts out of order:\n%s", got)
	}

	r.want(exitcode.OK, "status", "--json", "--daemon")
	var st ctl.Status
	mustUnmarshal(t, r.envelope().Data, &st)
	if st.Daemon == nil || st.Daemon.PID != 4242 || st.Username != "alice" || st.Health == nil || len(st.Alerts) != 2 {
		t.Errorf("status = %+v", st)
	}
}

// A daemon waiting for a login (pcb serve --wait-for-login) says so, in
// status and doctor, with the needs-login alert.
func TestStatusShowsADaemonWaitingForALogin(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{statusPhase: "ready"})
	since := daemonNow.Add(-time.Minute)
	startDaemon(t, r, &daemonAPI{status: service.Status{Phase: "logged-out"},
		health: service.Health{Serve: service.ServeWaitingForLogin, Since: &since, Calendars: []service.CalendarHealth{}}}, nil)
	r.want(exitcode.OK, "status")
	for _, want := range []string{
		"daemon: waiting for a login since 2026-11-12T11:59:00Z\n",
		"alert: error needs-login: not logged in to Proton (fix: run pcb login)",
	} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, r.out.String())
		}
	}
	if strings.Contains(r.out.String(), "serving since") {
		t.Errorf("status says serving while waiting:\n%s", r.out.String())
	}
}

// Ruling M5-P5: --daemon needs an answering daemon (the HEALTHCHECK's).
func TestStatusDaemonFlagNeedsADaemon(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{statusPhase: "ready"})
	r.want(exitcode.Blocked, "status", "--daemon", "--json")
	e := r.envelope()
	if e.Error == nil || e.Error.Message != "no pcb daemon answers on "+r.dirs.Socket() || e.Error.Hint != "start it: pcb serve" {
		t.Errorf("error = %+v", e.Error)
	}
	r.want(exitcode.Blocked, "status", "--daemon")
	if !strings.Contains(r.errb.String(), "no pcb daemon answers on "+r.dirs.Socket()) {
		t.Errorf("stderr = %q", r.errb.String())
	}
}

// Final review M2: the hint names a way to start pcb that exists here: the
// service where a unit is installed, the container's restart in one (no
// systemd there), else pcb serve.
func TestStatusDaemonHintFitsTheHost(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		files []string
		want  string
	}{
		{"systemd, no unit", []string{"/run/systemd/system/"}, "start it: pcb serve, or pcb install to run it as a service"},
		{"systemd, a unit", []string{"/run/systemd/system/", "/etc/systemd/system/pcb.service"}, "start it: pcb service start (pcb service logs says why it stopped)"},
		{"docker", []string{"/.dockerenv"}, "restart the container (docker restart NAME, or docker compose up -d); docker logs NAME says why it stopped"},
		{"podman", []string{"/run/.containerenv"}, "restart the container (docker restart NAME, or docker compose up -d); docker logs NAME says why it stopped"},
	} {
		r := stubRig(t, &stubAPI{statusPhase: "ready"})
		r.hostRoot = t.TempDir()
		for _, f := range c.files {
			p := filepath.Join(r.hostRoot, f)
			if strings.HasSuffix(f, "/") {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		r.want(exitcode.Blocked, "status", "--daemon", "--json")
		if e := r.envelope(); e.Error == nil || e.Error.Hint != c.want {
			t.Errorf("%s: error = %+v, want the hint %q", c.name, e.Error, c.want)
		}
	}
}

// Only "no daemon" falls back offline: any other dial failure exits 20,
// in status and in doctor (a daemon ✗).
func TestOnlyNoDaemonFallsBackOffline(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{statusPhase: "ready"})
	r.dial = func(context.Context, string) (*ctl.Client, error) {
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("the control socket answers garbage"), Hint: "restart pcb serve"}
	}
	r.want(exitcode.Blocked, "status")
	if strings.Contains(r.out.String(), "offline") || !strings.Contains(r.errb.String(), "the control socket answers garbage") ||
		!strings.Contains(r.errb.String(), "Hint: restart pcb serve") {
		t.Errorf("stdout %q, stderr %q; want the dial error, not the offline view", r.out.String(), r.errb.String())
	}
	r.want(exitcode.Blocked, "doctor")
	if want := "✗  daemon: the control socket answers garbage\n   fix: restart pcb serve\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}

	// The same rig with no daemon at all: offline, exit 0.
	r.dial = func(context.Context, string) (*ctl.Client, error) { return nil, ctl.ErrNoDaemon }
	r.want(exitcode.OK, "status")
	if !strings.Contains(r.out.String(), "daemon: not running (offline view)") {
		t.Errorf("status = %q, want the offline view", r.out.String())
	}
}

// Ruling M5-R2: a file that is not our socket at the socket path is
// refused (exit 20), never taken for "no daemon".
func TestStatusRefusesAForeignSocketPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	r := stubRig(t, &stubAPI{statusPhase: "ready"})
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.dirs.Socket(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.Blocked, "status", "--json")
	if e := r.envelope().Error; e == nil || !strings.Contains(e.Message, "is not a socket") || e.Hint == "" {
		t.Errorf("error = %+v", e)
	}
}
