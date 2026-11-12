package cli_test

import (
	"encoding/json"
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
	if code != exitcode.Blocked || errs != "" {
		t.Fatalf("code=%d stderr=%q", code, errs)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Results []struct {
				Name, Status, Fix string
			} `json:"results"`
		} `json:"data"`
		Error struct{ Code int } `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&env); err != nil {
		t.Fatal(err)
	}
	if dec.More() {
		t.Fatalf("more than one JSON value: %q", out)
	}
	if env.OK || env.Error.Code != 20 {
		t.Fatalf("bad envelope: %+v", env)
	}
	var names, statuses []string
	for _, r := range env.Data.Results {
		names = append(names, r.Name)
		statuses = append(statuses, r.Status)
	}
	if got, want := strings.Join(names, ","), "platform,config dir,state dir,config file,key file,sealed secrets,secrets,cache,caldav listener,tls certificate,caldav clients,service,daemon"; got != want {
		t.Fatalf("names = %s, want %s", got, want)
	}
	if got, want := strings.Join(statuses, ","), "ok,error,skip,ok,skip,skip,skip,ok,ok,skip,warn,skip,skip"; got != want {
		// platform may be warn on an unsupported test host
		if !strings.HasSuffix(got, ",error,skip,ok,skip,skip,skip,ok,ok,skip,warn,skip,skip") {
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
	if len(lines) != 14 { // 11 checks, the clients warning with its fix line, the service, the daemon
		t.Fatalf("stdout = %q, want 14 lines", out)
	}
	if want := "–  service: no systemd here"; lines[12] != want {
		t.Errorf("line 12 = %q, want %q", lines[12], want)
	}
	if want := "–  daemon: not running: offline checks only"; lines[13] != want {
		t.Errorf("line 13 = %q, want %q", lines[13], want)
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
	if want := "–  secrets: no secrets stored yet"; lines[6] != want {
		t.Errorf("line 6 = %q, want %q", lines[6], want)
	}
	if want := "✓  cache: no cache yet"; lines[7] != want {
		t.Errorf("line 7 = %q, want %q", lines[7], want)
	}
	if want := "✓  caldav listener: 127.0.0.1:1080, plain HTTP (loopback only)"; lines[8] != want {
		t.Errorf("line 8 = %q, want %q", lines[8], want)
	}
	if want := "–  tls certificate: plain HTTP on loopback: no certificate needed"; lines[9] != want {
		t.Errorf("line 9 = %q, want %q", lines[9], want)
	}
	if want := "!  caldav clients: no CalDAV client: nothing can sign in to pcb serve"; lines[10] != want || lines[11] != "   fix: pcb login (it creates the client default), then pcb client show" {
		t.Errorf("lines 10-11 = %q, want %q and its fix", lines[10:], want)
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
	if code != exitcode.Blocked || !strings.Contains(out, "✗  config dir") || !strings.Contains(out, "   fix: chmod 700 "+cfg) {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}

func TestDoctorReportsUnresolvableDirs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("PCB_CONFIG_DIR", "")
	t.Setenv("PCB_STATE_DIR", "")
	t.Setenv("HOME", "")
	code, out, _ := run("doctor")
	if code != exitcode.Blocked || !strings.Contains(out, "✗  config dir") || !strings.Contains(out, "✗  state dir") ||
		!strings.Contains(out, "fix: set HOME, PCB_CONFIG_DIR or XDG_CONFIG_HOME to an absolute path") ||
		!strings.Contains(out, "fix: set HOME, PCB_STATE_DIR or XDG_STATE_HOME to an absolute path") {
		t.Fatalf("code=%d stdout=%q", code, out)
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
	if code != exitcode.Blocked || !strings.Contains(out, "✗  config file: unknown key proton.appversion") || !strings.Contains(out, "   fix: edit "+p) {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
}

// With a daemon, doctor lists its alerts: an error is ✗ with its fix and
// exit 20; a warning is ! and leaves the exit code alone.
func TestDoctorListsAlerts(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{})
	since := daemonNow.Add(-time.Hour)
	api := &daemonAPI{status: service.Status{Phase: "ready"}, health: service.Health{Serve: service.ServeServing, Since: &since, IntervalSeconds: 30,
		Calendars: []service.CalendarHealth{{ID: "cal-2", Name: "Home", SyncedAt: &daemonNow, Gate: "undecided", GateReason: "the keys were not loaded yet"}}}}
	startDaemon(t, r, api, nil)

	r.want(exitcode.OK, "doctor")
	for _, want := range []string{
		"✓  daemon: serving since 2026-11-12T11:00:00Z (pid 4242, version v1.2.3)\n",
		"!  alert write-gate-undecided cal-2: writes to Home answer 503: the keys were not loaded yet\n" +
			"   fix: check the calendar's keys in the Proton web app, then pcb service restart\n",
	} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
		}
	}

	api.status = service.Status{Phase: "needs-login", Reason: "Proton ended the session"}
	r.want(exitcode.Blocked, "doctor")
	if want := "✗  alert needs-login: Proton ended the session\n   fix: run pcb login\n"; !strings.Contains(r.out.String(), want) {
		t.Errorf("doctor lacks %q:\n%s", want, r.out.String())
	}

	r.want(exitcode.Blocked, "doctor", "--json")
	var data struct {
		Results []struct{ Name, Status, Message, Fix string } `json:"results"`
	}
	mustUnmarshal(t, r.envelope().Data, &data)
	var found bool
	for _, res := range data.Results {
		if res.Name == "alert needs-login" && res.Status == "error" && res.Fix == "run pcb login" {
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
	startDaemon(t, r, &daemonAPI{status: service.Status{Phase: "ready"}},
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
