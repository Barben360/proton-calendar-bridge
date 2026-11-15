package cli_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/update"
)

// fakeReleases answers /releases/latest like github.com: a redirect to
// tag's page (status 404 when tag is empty). It counts the requests.
func fakeReleases(t *testing.T, r *rig, installed, tag string) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		if tag == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Location", srv.URL+"/Barben360/proton-calendar-bridge/releases/tag/"+tag)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	r.upd = &update.Checker{URL: srv.URL + "/latest", Installed: installed}
	return &n
}

// installHere makes the rig's binary a default install.sh install (pcb and
// pcb-ui in ~/.local/bin, the desktop entry): its hint is the bare line.
func installHere(t *testing.T, r *rig) {
	t.Helper()
	home := t.TempDir()
	r.env["HOME"] = home
	bin := filepath.Join(home, ".local", "bin")
	apps := filepath.Join(home, ".local", "share", "applications")
	for _, d := range []string{bin, apps} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(bin, "pcb"), filepath.Join(bin, "pcb-ui"), filepath.Join(apps, "pcb-ui.desktop")} {
		if err := os.WriteFile(f, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r.exe = filepath.Join(bin, "pcb")
}

func TestVersionSendsNoRequest(t *testing.T) {
	r := newRig(t, nil)
	n := fakeReleases(t, r, "v0.1.0", "v0.2.0")
	r.want(exitcode.OK, "version")
	if strings.Contains(r.out.String(), "update") || n.Load() != 0 {
		t.Errorf("pcb version: %q, %d requests, want none", r.out.String(), n.Load())
	}
}

func TestVersionCheckText(t *testing.T) {
	if !onUnixInstall() {
		t.Skip("the install line is for Linux and macOS")
	}
	r := newRig(t, nil)
	n := fakeReleases(t, r, "v0.1.0", "v0.2.0")
	installHere(t, r)
	r.want(exitcode.OK, "version", "--check")
	out := r.out.String()
	if !strings.Contains(out, "update available: v0.2.0 http") || !strings.Contains(out, "/releases/tag/v0.2.0") {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(out, "install.sh | sh") {
		t.Errorf("no install line: %q", out)
	}
	if n.Load() != 1 {
		t.Errorf("%d requests, want 1", n.Load())
	}
}

func TestVersionCheckOutcomes(t *testing.T) {
	for name, c := range map[string]struct{ installed, tag, want string }{
		"up to date": {"v0.2.0", "v0.2.0", "up to date"},
		"dev build":  {"dev", "v0.2.0", "check skipped: dev build"},
		"failed":     {"v0.1.0", "", "check failed: "},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			fakeReleases(t, r, c.installed, c.tag)
			r.want(exitcode.OK, "version", "--check")
			if !strings.Contains(r.out.String(), c.want) {
				t.Errorf("stdout = %q, want %q", r.out.String(), c.want)
			}
		})
	}
}

func TestVersionCheckJSON(t *testing.T) {
	r := newRig(t, nil)
	fakeReleases(t, r, "v0.1.0", "v0.2.0")
	r.want(exitcode.OK, "version", "--check", "--json")
	var d struct {
		Version string `json:"version"`
		Update  *struct {
			Latest    string `json:"latest"`
			URL       string `json:"url"`
			Available bool   `json:"available"`
			Error     string `json:"error"`
		} `json:"update"`
	}
	if err := json.Unmarshal(r.envelope().Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Update == nil || d.Update.Latest != "v0.2.0" || !d.Update.Available || !strings.HasSuffix(d.Update.URL, "/releases/tag/v0.2.0") || d.Update.Error != "" {
		t.Errorf("data = %+v / %+v", d, d.Update)
	}
	r.want(exitcode.OK, "version", "--json")
	if strings.Contains(string(r.envelope().Data), `"update"`) {
		t.Errorf("plain version carries update: %s", r.envelope().Data)
	}
}

// doctorUpdate runs pcb doctor --json and returns its exit code and update line.
func doctorUpdate(t *testing.T, r *rig) (exitcode.Code, map[string]string) {
	t.Helper()
	code := r.run("doctor", "--json")
	var d struct {
		Results []map[string]string `json:"results"`
	}
	if err := json.Unmarshal(r.envelope().Data, &d); err != nil {
		t.Fatal(err)
	}
	for _, res := range d.Results {
		if res["name"] == "update" {
			return code, res
		}
	}
	t.Fatalf("no update line in %v", d.Results)
	return 0, nil
}

func TestDoctorUpdateLine(t *testing.T) {
	r := newRig(t, nil)
	fakeReleases(t, r, "v0.2.0", "v0.2.0")
	upToDate, res := doctorUpdate(t, r)
	if res["status"] != "ok" || res["message"] != "up to date (v0.2.0)" {
		t.Errorf("up to date: %v", res)
	}

	r = newRig(t, nil)
	n := fakeReleases(t, r, "v0.1.0", "v0.2.0")
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil { // as after the first login
		t.Fatal(err)
	}
	available, res := doctorUpdate(t, r)
	if res["status"] != "warn" || res["message"] != "v0.2.0 is available (you have v0.1.0)" || !strings.Contains(res["fix"], "/releases/tag/v0.2.0") {
		t.Errorf("available: %v", res)
	}
	if available != upToDate {
		t.Errorf("exit %d with an update, %d without: a warn must not change it", available, upToDate)
	}
	doctorUpdate(t, r)
	if n.Load() != 1 {
		t.Errorf("%d requests over two doctor runs, want 1 (the 24 h cache)", n.Load())
	}
}

func TestDoctorUpdateSwitchedOff(t *testing.T) {
	r := newRig(t, nil)
	n := fakeReleases(t, r, "v0.1.0", "v0.2.0")
	r.env["PCB_UPDATE_CHECK"] = "off"
	if _, res := doctorUpdate(t, r); res["status"] != "skip" || res["message"] != "switched off" {
		t.Errorf("env off: %v", res)
	}
	delete(r.env, "PCB_UPDATE_CHECK")
	r.want(exitcode.OK, "update-check", "off")
	if _, res := doctorUpdate(t, r); res["status"] != "skip" || res["message"] != "switched off" {
		t.Errorf("switch off: %v", res)
	}
	if n.Load() != 0 {
		t.Errorf("%d requests while off, want 0", n.Load())
	}
	r.want(exitcode.OK, "version", "--check")
	if n.Load() != 1 {
		t.Errorf("version --check while off: %d requests, want 1 (an explicit request)", n.Load())
	}
}

func TestUpdateCheckSwitch(t *testing.T) {
	r := newRig(t, nil)
	r.want(exitcode.OK, "update-check", "status")
	if got := strings.TrimSpace(r.out.String()); got != "update check: on" {
		t.Errorf("status = %q", got)
	}
	r.want(exitcode.OK, "update-check", "off")
	r.want(exitcode.OK, "update-check", "status")
	if got := strings.TrimSpace(r.out.String()); got != "update check: off" {
		t.Errorf("status after off = %q", got)
	}
	r.want(exitcode.OK, "update-check", "on")
	r.env["PCB_UPDATE_CHECK"] = "off"
	r.want(exitcode.OK, "update-check", "status", "--json")
	var d struct {
		Enabled bool `json:"enabled"`
		EnvOff  bool `json:"env_off"`
	}
	if err := json.Unmarshal(r.envelope().Data, &d); err != nil {
		t.Fatal(err)
	}
	if d.Enabled || !d.EnvOff {
		t.Errorf("env off: %+v", d)
	}
	r.want(exitcode.Usage, "update-check", "maybe")
}

func onUnixInstall() bool { return runtime.GOOS == "linux" || runtime.GOOS == "darwin" }
