package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// isolate points the XDG dirs at a temp tree and returns the config dir.
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("PCB_CONFIG_DIR", "")
	t.Setenv("PCB_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	return filepath.Join(root, "cfg", "pcb")
}

func TestDoctorExitCode(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run("doctor", "--json")
	if code != exitcode.MustAct || errs != "" { // ux-I2: a mode to fix is the person's
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Results []struct {
				Name, Status, Fix string
			} `json:"results"`
		} `json:"data"`
		Error struct {
			Code          int
			Message, Hint string
		} `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	if dec.More() {
		t.Fatalf("more than one JSON value: %q", out)
	}
	if env.OK || env.Error.Code != 10 || env.Error.Message != "pcb doctor found 1 error" ||
		env.Error.Hint != "fix what the failed checks name (marked ✗), then run pcb doctor again" {
		t.Fatalf("bad envelope: %+v", env)
	}
	var names, statuses []string
	for _, r := range env.Data.Results {
		names = append(names, r.Name)
		statuses = append(statuses, r.Status)
	}
	// No account yet: the process checks, then "accounts" in place of the
	// account checks (design §4.6).
	if got, want := strings.Join(names, ","), "platform,config dir,state dir,config file,sealer,key file,caldav listener,tls certificate,accounts,service,daemon"; got != want {
		t.Fatalf("names = %s, want %s", got, want)
	}
	if got, want := strings.Join(statuses, ","), "ok,error,skip,ok,ok,skip,ok,skip,warn,skip,skip"; got != want {
		// platform may be warn on an unsupported test host
		if !strings.HasSuffix(got, ",error,skip,ok,ok,skip,ok,skip,warn,skip,skip") {
			t.Fatalf("statuses = %s, want %s", got, want)
		}
	}
	if !strings.Contains(env.Data.Results[1].Fix, "chmod 700 "+cfg) {
		t.Fatalf("fix = %q", env.Data.Results[1].Fix)
	}
}

func TestDoctorTextOK(t *testing.T) {
	cfg := isolate(t)
	state := filepath.Join(filepath.Dir(filepath.Dir(cfg)), "state", "pcb")
	code, out, errs := run("doctor")
	if code != exitcode.OK || errs != "" {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 12 { // 9 checks, the accounts warning with its fix line, the service, the daemon
		t.Fatalf("stdout = %q, want 12 lines", out)
	}
	if want := "–  service: no systemd here"; lines[10] != want {
		t.Errorf("line 10 = %q, want %q", lines[10], want)
	}
	if want := "–  daemon: not running: offline checks only"; lines[11] != want {
		t.Errorf("line 11 = %q, want %q", lines[11], want)
	}
	if want := "✓  sealer: keyfile"; lines[4] != want {
		t.Errorf("line 4 = %q, want %q", lines[4], want)
	}
	if want := "ok  platform: " + runtime.GOOS + "/" + runtime.GOARCH; !strings.HasPrefix(lines[0], "✓  platform: "+runtime.GOOS+"/"+runtime.GOARCH) {
		t.Errorf("line 0 = %q, want %q (or a warning on an unsupported host)", lines[0], want)
	}
	if want := "–  config dir: " + cfg + " does not exist yet"; lines[1] != want {
		t.Errorf("line 1 = %q, want %q", lines[1], want)
	}
	if want := "–  state dir: " + state + " does not exist yet"; lines[2] != want {
		t.Errorf("line 2 = %q, want %q", lines[2], want)
	}
	if want := "✓  config file: defaults, no file"; lines[3] != want {
		t.Errorf("line 3 = %q, want %q", lines[3], want)
	}
	if want := "✓  caldav listener: 127.0.0.1:1080, plain HTTP (loopback only)"; lines[6] != want {
		t.Errorf("line 6 = %q, want %q", lines[6], want)
	}
	if want := "–  tls certificate: plain HTTP on loopback: no certificate needed"; lines[7] != want {
		t.Errorf("line 7 = %q, want %q", lines[7], want)
	}
	if want := "!  accounts: no account: nothing can sign in to pcb serve"; lines[8] != want || lines[9] != "   fix: pcb login (it creates the client default), then pcb client show" {
		t.Errorf("lines 8-9 = %q, want %q and its fix", lines[8:10], want)
	}
}

func TestDoctorTextShowsFix(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg, 0o750); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("doctor")
	if code != exitcode.MustAct || !strings.Contains(out, "✗  config dir") || !strings.Contains(out, "   fix: chmod 700 "+cfg) {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}

func TestDoctorReportsUnresolvableDirs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("PCB_CONFIG_DIR", "")
	t.Setenv("PCB_STATE_DIR", "")
	t.Setenv("HOME", "")
	code, out, errs := run("doctor")
	if code != exitcode.MustAct || !strings.Contains(out, "✗  config dir") || !strings.Contains(out, "✗  state dir") ||
		!strings.Contains(out, "fix: set HOME, PCB_CONFIG_DIR or XDG_CONFIG_HOME to an absolute path") ||
		!strings.Contains(out, "fix: set HOME, PCB_STATE_DIR or XDG_STATE_HOME to an absolute path") {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	if want := "Error: pcb doctor found 2 errors\nHint: fix what the failed checks name (marked ✗), then run pcb doctor again\n"; errs != want {
		t.Errorf("stderr = %q, want %q", errs, want)
	}
}

func TestDoctorFlagsBadConfigFile(t *testing.T) {
	cfg := isolate(t)
	if err := os.MkdirAll(cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(cfg, "config.toml")
	if err := os.WriteFile(p, []byte("[proton]\nappversion = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run("doctor")
	// ux-I2: a bad config.toml is exit 10, as in every other command.
	if code != exitcode.MustAct || !strings.Contains(out, "✗  config file: unknown key proton.appversion") || !strings.Contains(out, "   fix: edit "+p) {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}

// With a daemon, doctor lists its alerts: an error is ✗ with its fix and
// exit 10 (a person acts on every error alert); a warning is ! and leaves
// the exit code alone.
func TestDoctorListsAlerts(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	since := daemonNow.Add(-time.Hour)
	api := &daemonAPI{status: service.Status{Phase: "ready"}, health: service.Health{Serve: service.ServeServing, Since: &since, IntervalSeconds: 30,
		Calendars: []service.CalendarHealth{{ID: "cal-2", Name: "Home", SyncedAt: &daemonNow, Gate: "undecided", GateReason: "the keys were not loaded yet"}}}}
	startDaemon(t, r, newStubHub(api), nil)

	r.want(exitcode.OK, "doctor")
	for _, want := range []string{
		"✓  daemon: running since 2026-11-12T10:00:00Z (pid 4242, version v1.2.3)\n",
		"!  " + stubAccountID + ": alert write-gate-undecided cal-2: writes to Home answer 503: the keys were not loaded yet\n" +
			"   fix: check the calendar's keys in the Proton web app, then restart pcb (pcb service restart, or restart its container)\n",
	} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
		}
	}

	api.status = service.Status{Phase: "needs-login", Reason: "Proton ended the session"}
	r.want(exitcode.MustAct, "doctor")
	if want := "✗  " + stubAccountID + ": alert needs-login: Proton ended the session\n   fix: run pcb login\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}

	r.want(exitcode.MustAct, "doctor", "--json")
	var data struct {
		Results []struct{ Name, Status, Message, Fix string } `json:"results"`
	}
	mustUnmarshal(t, r.envelope().Data, &data)
	var found bool
	for _, res := range data.Results {
		if res.Name == stubAccountID+": alert needs-login" && res.Status == "error" && res.Fix == "run pcb login" {
			found = true
		}
	}
	if !found {
		t.Errorf("results lack the needs-login alert: %+v", data.Results)
	}
}

// M5-R15 C4: the CalDAV checks use the running daemon's listener (here
// 0.0.0.0 over TLS while config.toml says the loopback default).
func TestDoctorUsesTheDaemonsListener(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	notAfter := time.Now().Add(365 * 24 * time.Hour).UTC()
	startDaemon(t, r, newStubHub(&daemonAPI{status: service.Status{Phase: "ready"}}),
		&ctl.Listener{Addr: "0.0.0.0:1080", URL: "https://pcb.lan:1080/", TLS: true, Fingerprint: "AB:CD", CertNotAfter: &notAfter})
	r.want(exitcode.OK, "doctor")
	for _, want := range []string{
		"✓  caldav listener: 0.0.0.0:1080, TLS (the running daemon; URL https://pcb.lan:1080/)\n",
		"✓  tls certificate: served by the running daemon, expires on " + notAfter.Format(time.DateOnly) + ", SHA-256 fingerprint AB:CD\n",
	} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
		}
	}
}

// ux-I2: the daemon of a fresh install waits for a login: its needs-login
// alert is a warning in pcb doctor (exit 0), as HUMAN-CHECKS 7.4 expects.
// Without the wait, it stays an error (exit 10).
func TestDoctorNeedsLoginWhileTheDaemonWaitsIsAWarning(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	api := &daemonAPI{status: service.Status{Phase: "logged-out"}, health: service.Health{Serve: service.ServeWaitingForLogin}}
	startDaemon(t, r, newStubHub(api), nil)
	r.want(exitcode.OK, "doctor")
	if want := "!  " + stubAccountID + ": alert needs-login: not logged in to Proton\n   fix: run pcb login\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}
	api.health.Serve = service.ServeServing
	r.want(exitcode.MustAct, "doctor")
	if want := "✗  " + stubAccountID + ": alert needs-login: not logged in to Proton\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}
}

// Ruling R44 with ux-I2: a daemon with no account waits for a login (pcb
// serve exits 10 without --wait-for-login): the process's needs-login is a
// warning, exit 0, as the systemd and container smoke tests expect.
func TestDoctorNeedsLoginOfADaemonWithNoAccountIsAWarning(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	startDaemon(t, r, newStubHub(), nil)
	r.want(exitcode.OK, "doctor")
	if want := "!  alert needs-login: not logged in to Proton\n   fix: run pcb login\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}
}

// ux-I2: a daemon that does not answer its control socket cleanly is the
// one error that may pass by itself: exit 20 when it is the only one.
func TestDoctorTransientDaemonErrorIsExit20(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	r.dial = func(context.Context, string) (*ctl.Client, error) {
		return nil, &exitcode.Error{Code: exitcode.Blocked, Err: errors.New("pcb serve did not answer in time"), Hint: "run pcb doctor again"}
	}
	r.want(exitcode.Blocked, "doctor")
	if want := "✗  daemon: pcb serve did not answer in time\n   fix: run pcb doctor again\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}
	if want := "Error: pcb doctor found 1 error\nHint: fix what the failed checks name (marked ✗), then run pcb doctor again\n"; r.errb.String() != want {
		t.Errorf("stderr = %q, want %q", r.errb.String(), want)
	}
	// With a mode to fix as well, a person must act: 10.
	if err := os.MkdirAll(r.dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(r.dirs.Config, 0o750); err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.MustAct, "doctor")
}

// ux Minor: pcb doctor warns per client whose sealed password the key
// file no longer opens (here: a new key file), with the rotation to run,
// named by its account. (Signed out first: the new key would fail the
// sealed session's check too.)
func TestDoctorWarnsAboutPasswordsTheKeyNoLongerOpens(t *testing.T) {
	t.Parallel()
	r := clientRig(t)
	r.want(exitcode.OK, "client", "add", "phone")
	r.want(exitcode.OK, "logout")
	key := filepath.Join(r.dirs.Config, "secrets.key")
	if err := os.WriteFile(key, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.OK, "doctor")
	if want := "!  " + r.f.Username + ": caldav client phone: password cannot be shown\n   fix: pcb client rotate phone\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}
}

// Ruling A11: a control socket pcb refuses (here a file that is not a
// socket) needs a person: exit 10, with ctl's own message and fix.
func TestDoctorRefusedControlSocketIsExit10(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("no control socket off Unix")
	}
	r := stubRig(t, &stubAPI{})
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := r.dirs.Socket()
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.MustAct, "doctor")
	want := "✗  daemon: " + sock + " is not a socket: pcb refuses to use it as the control socket\n" +
		"   fix: move " + sock + " away: pcb serve keeps its control socket there\n"
	if !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}
}

// Ruling R34: a move to the per-account layout that stopped midway is a
// warning naming it (the next command finishes it), not "no account"; an
// unreadable layout is an error with its own fix.
func TestDoctorNamesTheLayoutsState(t *testing.T) {
	cfg := isolate(t)
	state := filepath.Join(filepath.Dir(filepath.Dir(cfg)), "state", "pcb")
	staged := filepath.Join(state, "accounts", "aaaaaaaa.migrating")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	result := func(t *testing.T, want exitcode.Code) (status, message, fix string) {
		t.Helper()
		code, out, _ := run("doctor", "--json")
		if code != want {
			t.Fatalf("exit %d, want %d: %s", code, want, out)
		}
		var env struct {
			Data struct {
				Results []struct{ Name, Status, Message, Fix string } `json:"results"`
			} `json:"data"`
		}
		mustUnmarshal(t, []byte(out), &env)
		for _, r := range env.Data.Results {
			if r.Name == "accounts" {
				return r.Status, r.Message, r.Fix
			}
		}
		t.Fatalf("no accounts result: %s", out)
		return "", "", ""
	}
	if st, msg, fix := result(t, exitcode.OK); st != "warn" || !strings.Contains(msg, staged) || !strings.Contains(msg, "the next pcb command that opens the state finishes it") || fix != "pcb status" {
		t.Errorf("interrupted move: %s %q fix %q", st, msg, fix)
	}
	if err := os.WriteFile(filepath.Join(state, "layout.json"), []byte(`{"version": 3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _, fix := result(t, exitcode.MustAct); st != "error" || fix != "upgrade pcb" {
		t.Errorf("newer layout: %s, fix %q", st, fix)
	}
}
