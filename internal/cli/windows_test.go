package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/runkey"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// fakeRunKey is pcb-ui's Run value in memory.
type fakeRunKey struct {
	mu      sync.Mutex
	data    string
	set     bool
	deletes int
}

var _ runkey.Store = (*fakeRunKey)(nil)

func (f *fakeRunKey) Get() (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data, f.set, nil
}

func (f *fakeRunKey) Set(d string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data, f.set = d, true
	return nil
}

func (f *fakeRunKey) Delete() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data, f.set = "", false
	f.deletes++
	return nil
}

func (f *fakeRunKey) present() bool { _, ok, _ := f.Get(); return ok }

// winLogoutAPI counts the Proton sessions the uninstall revokes.
type winLogoutAPI struct {
	stubAPI
	mu    sync.Mutex
	calls int
}

func (a *winLogoutAPI) Logout(context.Context, service.LogoutRequest) (service.LogoutResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return service.LogoutResult{WasLoggedIn: true, Revoked: true}, nil
}

// winRig runs pcb as on Windows (IO.GOOS) over real directories named pcb
// in a temp dir, a fake Run key and a stub account.
type winRig struct {
	t             *testing.T
	config, state string
	key           *fakeRunKey
	exe           string // this pcb's executable
	override      map[string]string
	api           *winLogoutAPI
	stdout        strings.Builder
}

func newWinRig(t *testing.T) *winRig {
	t.Helper()
	base := t.TempDir()
	exe := filepath.Join(base, "bin", "pcb.exe")
	w := &winRig{t: t, config: filepath.Join(base, "cfg", "pcb"), state: filepath.Join(base, "state", "pcb"), exe: exe,
		key: &fakeRunKey{data: runkey.Data(filepath.Join(base, "bin", "pcb-ui.exe")), set: true}, api: &winLogoutAPI{}}
	for _, f := range []string{
		filepath.Join(w.config, "config.toml"),
		filepath.Join(w.state, "layout.json"),
		filepath.Join(w.state, "secrets.key.dpapi"),
		filepath.Join(w.state, "accounts", "a", "state.json"),
	} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *winRig) run(args ...string) exitcode.Code {
	w.t.Helper()
	var out strings.Builder
	// The default directories of every OS are the rig's: an invocation
	// without override is "ours" (the one pcb-ui's Run value starts).
	base := filepath.Dir(w.config)
	env := map[string]string{"XDG_CONFIG_HOME": base, "XDG_STATE_HOME": filepath.Dir(w.state), "HOME": filepath.Dir(base),
		"APPDATA": base, "LOCALAPPDATA": filepath.Dir(w.state)}
	for k, v := range w.override {
		env[k] = v
	}
	h := noSystemHost()
	h.Root = ""
	h.Executable = func() (string, error) { return w.exe, nil }
	code := cli.Main(w.t.Context(), args, cli.IO{Stdout: &out, Stderr: &strings.Builder{}, Term: &fakeTerm{}, GOOS: "windows", RunKey: w.key,
		Env: func(k string) string { return env[k] }, Host: h,
		OpenHub: func(context.Context, func(string) string) (cli.Hub, error) { return newStubHub(w.api), nil }})
	w.stdout.Reset()
	w.stdout.WriteString(out.String())
	return code
}

func (w *winRig) want(code exitcode.Code, args ...string) {
	w.t.Helper()
	if got := w.run(args...); got != code {
		w.t.Fatalf("pcb %s: exit %d, want %d\n%s", strings.Join(args, " "), got, code, w.stdout.String())
	}
}

func (w *winRig) failure() (message, hint string) {
	w.t.Helper()
	var env envelope
	if err := json.Unmarshal([]byte(w.stdout.String()), &env); err != nil || env.Error == nil {
		w.t.Fatalf("no failure envelope (%v): %s", err, w.stdout.String())
	}
	return env.Error.Message, env.Error.Hint
}

func (w *winRig) steps() []install.Step {
	w.t.Helper()
	var env struct {
		Data planData `json:"data"`
	}
	if err := json.Unmarshal([]byte(w.stdout.String()), &env); err != nil {
		w.t.Fatalf("no envelope (%v): %s", err, w.stdout.String())
	}
	return env.Data.Steps
}

func (w *winRig) step(kind string) install.Step {
	w.t.Helper()
	for _, s := range w.steps() {
		if s.Kind == kind {
			return s
		}
	}
	w.t.Fatalf("no %s step in %s", kind, w.stdout.String())
	return install.Step{}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

const noServiceHint = `use pcb-ui; "Start at login" runs it at each login`

func TestInstallRefusedOnWindows(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	for _, args := range [][]string{{"install", "--json"}, {"install", "--plan", "--json"}, {"install", "--system", "--json"}} {
		w.want(exitcode.Blocked, args...)
		if msg, hint := w.failure(); msg != "no service on Windows" || hint != noServiceHint {
			t.Errorf("%v: %q / %q", args, msg, hint)
		}
	}
}

func TestServiceRefusedOnWindows(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	for _, verb := range []string{"start", "stop", "restart", "status", "logs"} {
		w.want(exitcode.Blocked, "service", verb, "--json")
		if msg, hint := w.failure(); msg != "no service on Windows" || hint != noServiceHint {
			t.Errorf("service %s: %q / %q", verb, msg, hint)
		}
	}
}

// pcb doctor's service line on Windows says why there is none, under the
// name it has on every OS; systemd is never looked at.
func TestDoctorServiceLineOnWindows(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	w.run("doctor", "--json") // the rig's files are placeholders: other checks fail
	var env struct {
		Data struct {
			Results []struct{ Name, Status, Message string } `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(w.stdout.String()), &env); err != nil {
		t.Fatalf("no envelope (%v): %s", err, w.stdout.String())
	}
	n := 0
	for _, r := range env.Data.Results {
		if r.Name != "service" {
			continue
		}
		n++
		if r.Status != "skip" || r.Message != "no service on Windows: pcb-ui's Start at login runs it" {
			t.Errorf("service = %+v", r)
		}
	}
	if n != 1 {
		t.Errorf("%d service results, want 1: %s", n, w.stdout.String())
	}
}

func TestUninstallOnWindowsRemovesOnlyTheRunValue(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	w.want(exitcode.OK, "uninstall", "--json")
	if w.key.present() {
		t.Error("the Run value survived")
	}
	for _, kept := range []string{w.config, w.state, filepath.Join(w.state, "layout.json")} {
		if !exists(kept) {
			t.Errorf("%s was deleted without --delete-data", kept)
		}
	}
	if st := w.step(install.KindAutostart); st.Action != install.ActionRemove {
		t.Errorf("autostart step = %+v", st)
	}
	if w.api.calls != 0 {
		t.Error("logged out without --delete-data")
	}
}

// The value carries no environment: it starts pcb-ui on the default
// directories, so an invocation on other ones keeps it.
func TestUninstallOnWindowsOnOtherDirsKeepsTheRunValue(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"uninstall", "--json"}, {"uninstall", "--delete-data", "--yes", "--json"}} {
		w := newWinRig(t)
		other := filepath.Join(t.TempDir(), "pcb")
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		w.override = map[string]string{"PCB_STATE_DIR": other}
		w.want(exitcode.OK, args...)
		if !w.key.present() {
			t.Errorf("%v: the default install's Run value was deleted", args)
		}
		st := w.step(install.KindAutostart)
		if st.Action != install.ActionKeep || !strings.Contains(st.Note, "default directories") {
			t.Errorf("%v: autostart step = %+v", args, st)
		}
		if !exists(w.state) {
			t.Errorf("%v: the default state was touched", args)
		}
	}
}

// Another copy's entry (a pcb-ui.exe not beside this pcb) is not ours.
func TestUninstallOnWindowsKeepsAnotherCopysRunValue(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	foreign := filepath.Join(t.TempDir(), "elsewhere", "pcb-ui.exe")
	w.key = &fakeRunKey{data: runkey.Data(foreign), set: true}
	w.want(exitcode.OK, "uninstall", "--json")
	if !w.key.present() {
		t.Error("another copy's Run value was deleted")
	}
	if st := w.step(install.KindAutostart); st.Action != install.ActionKeep || !strings.Contains(st.Note, foreign) {
		t.Errorf("autostart step = %+v, want keep naming %s", st, foreign)
	}
}

func TestUninstallOnWindowsWithoutRunValueSkips(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	w.key = &fakeRunKey{}
	w.want(exitcode.OK, "uninstall", "--json")
	if st := w.step(install.KindAutostart); st.Action != install.ActionSkip {
		t.Errorf("autostart step = %+v, want skip", st)
	}
	if w.key.deletes != 0 {
		t.Error("deleted an absent value")
	}
}

func TestUninstallOnWindowsPlanChangesNothing(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	w.want(exitcode.OK, "uninstall", "--delete-data", "--plan", "--json")
	if !w.key.present() || !exists(w.state) || !exists(w.config) || w.api.calls != 0 {
		t.Error("--plan changed something")
	}
	if exists(filepath.Join(w.state, "serve.lock")) {
		t.Error("--plan created serve.lock")
	}
}

// F7: it logs out of Proton as on Linux; the order of ruling 9 leaves
// nothing behind, serve.lock and the directory included.
func TestUninstallOnWindowsDeletesData(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	w.want(exitcode.OK, "uninstall", "--delete-data", "--yes", "--json")
	for _, gone := range []string{w.config, w.state} {
		if exists(gone) {
			t.Errorf("%s survived", gone)
		}
	}
	if w.key.present() {
		t.Error("the Run value survived")
	}
	if w.api.calls != 1 {
		t.Errorf("Proton logout calls = %d, want 1", w.api.calls)
	}
	if st := w.step(install.KindLogout); strings.Contains(st.Note, "not logged out") {
		t.Errorf("logout step note = %q", st.Note)
	}
}

func TestUninstallOnWindowsDeleteDataAsksWithoutYes(t *testing.T) {
	t.Parallel()
	w := newWinRig(t)
	w.want(exitcode.MustAct, "uninstall", "--delete-data", "--json")
	if !exists(w.state) || !w.key.present() {
		t.Error("deleted without confirmation")
	}
}

func TestUninstallOnWindowsRefusedWhileServing(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"uninstall", "--json"}, {"uninstall", "--delete-data", "--yes", "--json"}} {
		w := newWinRig(t)
		release, err := service.HoldServe(w.state, "ui")
		if err != nil {
			t.Fatal(err)
		}
		w.want(exitcode.Blocked, args...)
		release()
		if msg, _ := w.failure(); !strings.Contains(msg, "kind ui") || !strings.Contains(msg, "pid "+itoa(os.Getpid())) {
			t.Errorf("%v: message %q does not name the holder", args, msg)
		}
		if !w.key.present() || !exists(w.config) || !exists(filepath.Join(w.state, "layout.json")) || w.api.calls != 0 {
			t.Errorf("%v: something changed while the state is served", args)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
