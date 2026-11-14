package cli_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

const (
	autostartEntry = "/home/alice/.config/autostart/pcb-ui.desktop"
	otherAutostart = "/home/alice/.config/autostart/other.desktop"
)

// desktopInstall is a pcb-ui install with no service: its config and
// keyring state at the default directories, its autostart entry beside
// another program's.
func desktopInstall(t *testing.T) (*sysRig, map[string]string) {
	t.Helper()
	s := newSysRig(t)
	s.keyring = &memKeyring{}
	attrs := keyringState(t, s, s.keyring)
	s.write(userConfigDir+"/config.toml", "", 0o600)
	s.write(autostartEntry, "[Desktop Entry]\n", 0o600)
	s.write(otherAutostart, "[Desktop Entry]\n", 0o600)
	return s, attrs
}

// Live 2026-11-13, F10: with pcb-ui and no service, pcb uninstall
// --delete-data said "pcb is not installed as a service" (exit 10) and the
// keyring item could only be deleted by hand. It now deletes the
// directories this invocation resolves, the state's keyring item and
// pcb-ui's autostart entry, with the plan, the confirmation and --yes as
// for a service, and runs no systemctl.
func TestUninstallDeleteDataWithoutAService(t *testing.T) {
	t.Parallel()
	s, attrs := desktopInstall(t)
	s.want(exitcode.OK, "uninstall", "--delete-data", "--plan")
	plan := s.stdout.String()
	for _, want := range []string{
		"run logout " + userStateDir,
		"remove dir " + userConfigDir,
		"remove dir " + userStateDir,
		"remove keyring " + userStateDir + " — ",
		"remove file " + autostartEntry,
		"Plan only: nothing was changed.",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "systemctl") || strings.Contains(plan, otherAutostart) {
		t.Errorf("plan names systemctl or another autostart entry:\n%s", plan)
	}
	if !s.exists(userStateDir) || !s.exists(autostartEntry) {
		t.Fatal("the plan changed something")
	}

	s.want(exitcode.MustAct, "uninstall", "--delete-data", "--json")
	if _, msg, _ := s.failure(); !strings.Contains(msg, "--yes") {
		t.Errorf("non-interactive refusal does not name --yes: %q", msg)
	}
	if !s.exists(userStateDir) {
		t.Fatal("the refusal deleted the state")
	}

	s.want(exitcode.OK, "uninstall", "--delete-data", "--yes", "--non-interactive")
	for _, gone := range []string{userConfigDir, userStateDir, autostartEntry} {
		if s.exists(gone) {
			t.Errorf("%s survived", gone)
		}
	}
	if !s.exists(otherAutostart) {
		t.Error("another program's autostart entry was deleted")
	}
	if _, err := s.keyring.Get(t.Context(), attrs); !errors.Is(err, seal.ErrNoKey) {
		t.Errorf("the state's keyring item survived: %v", err)
	}
	if _, err := s.keyring.Get(t.Context(), map[string]string{"application": "pcb", "state": "/elsewhere/other-state"}); err != nil {
		t.Errorf("another state's item was deleted: %v", err)
	}
	if want := "remove keyring " + userStateDir; !strings.Contains(s.stdout.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, s.stdout.String())
	}
	if ev := s.takeEvents(); !slices.Contains(ev, "logout (state present: yes)") || slices.ContainsFunc(ev, func(e string) bool { return strings.HasPrefix(e, "systemctl") }) {
		t.Errorf("events = %q, want the logout and no systemctl", ev)
	}
	if s.logoutEnv != [2]string{filepath.Join(s.root, userConfigDir), filepath.Join(s.root, userStateDir)} {
		t.Errorf("logout on %q, want this invocation's directories", s.logoutEnv)
	}
}

// A daemon serving that state (pcb-ui, pcb serve, an agent session) holds
// serve.lock: the deletion is refused, exit 20, naming it, and nothing goes.
func TestUninstallDeleteDataWithoutAServiceRefusesWhileADaemonServes(t *testing.T) {
	t.Parallel()
	s, _ := desktopInstall(t)
	lock := filepath.Join(s.root, userStateDir, "serve.lock")
	unlock, ok, err := filelock.TryLock(lock)
	if err != nil || !ok {
		t.Fatalf("control: serve.lock: ok=%v err=%v", ok, err)
	}
	defer unlock()
	s.write(userStateDir+"/serve.lock", "4242 ui\n", 0o600)
	s.want(exitcode.Blocked, "uninstall", "--delete-data", "--yes", "--non-interactive", "--json")
	if _, msg, hint := s.failure(); !strings.Contains(msg, "pid 4242, kind ui") || !strings.Contains(hint, "pcb-ui") {
		t.Errorf("refusal = %q / %q, want the daemon named and a pcb-ui hint", msg, hint)
	}
	for _, kept := range []string{userConfigDir, userStateDir + "/layout.json", autostartEntry} {
		if !s.exists(kept) {
			t.Errorf("%s was deleted while a daemon serves", kept)
		}
	}
	if ev := s.takeEvents(); slices.ContainsFunc(ev, func(e string) bool { return strings.HasPrefix(e, "logout") }) {
		t.Errorf("logged out while a daemon serves: %q", ev)
	}
}

// --user names a service: without its unit the directories stay unknown
// (M5-R6), and the hint points at the run without --user.
func TestUninstallUserDeleteDataWithoutAUnitPointsAtTheDesktopRun(t *testing.T) {
	t.Parallel()
	s, _ := desktopInstall(t)
	s.want(exitcode.MustAct, "uninstall", "--user", "--delete-data", "--plan", "--json")
	if _, _, hint := s.failure(); !strings.Contains(hint, "pcb uninstall --delete-data") || !strings.Contains(hint, "without --user") {
		t.Errorf("hint = %q, want the run without --user", hint)
	}
}

// pcb-ui's autostart entry carries no environment: it starts pcb-ui on the
// default directories. An invocation on other ones (PCB_*_DIR, a test
// state) keeps it, saying why, so the real install still starts at login.
func TestUninstallDeleteDataOnOtherDirsKeepsTheAutostartEntry(t *testing.T) {
	t.Parallel()
	s, _ := desktopInstall(t)
	s.env["PCB_CONFIG_DIR"] = "/tmp/x/cfg/pcb"
	s.env["PCB_STATE_DIR"] = "/tmp/x/pcb"
	s.mkdir("/tmp/x/cfg/pcb")
	s.mkdir("/tmp/x/pcb")
	s.want(exitcode.OK, "uninstall", "--delete-data", "--yes", "--non-interactive")
	if s.exists("/tmp/x/pcb") || s.exists("/tmp/x/cfg/pcb") {
		t.Fatal("control: the invocation's directories survived")
	}
	if !s.exists(autostartEntry) {
		t.Error("the autostart entry of the default directories was deleted")
	}
	if want := "keep file " + autostartEntry + " — "; !strings.Contains(s.stdout.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, s.stdout.String())
	}
	if !s.exists(userStateDir + "/layout.json") {
		t.Error("the default state was touched")
	}
}

// serve.lock is taken only in a state directory the plan removes: a kept
// one (not named pcb) gets no file of pcb's.
func TestUninstallDeleteDataLeavesAKeptStateUntouched(t *testing.T) {
	t.Parallel()
	s, _ := desktopInstall(t)
	s.env["PCB_STATE_DIR"] = "/srv/mystate"
	s.write("/srv/mystate/layout.json", `{"version":2,"sealer":"keyfile"}`+"\n", 0o600)
	s.want(exitcode.OK, "uninstall", "--delete-data", "--yes", "--non-interactive")
	if s.exists(userConfigDir) {
		t.Fatal("control: the config directory survived")
	}
	if !s.exists("/srv/mystate/layout.json") {
		t.Fatal("control: the kept state lost its files")
	}
	if s.exists("/srv/mystate/serve.lock") {
		t.Error("serve.lock was left in a state directory the plan keeps")
	}
}

// --plan says when a daemon serves the state (the run would be refused),
// and probes without creating serve.lock.
func TestUninstallDeleteDataPlanNamesTheServingDaemon(t *testing.T) {
	t.Parallel()
	s, _ := desktopInstall(t)
	s.want(exitcode.OK, "uninstall", "--delete-data", "--plan")
	if s.exists(userStateDir + "/serve.lock") {
		t.Error("--plan created serve.lock")
	}
	if strings.Contains(s.stdout.String(), "already serving") {
		t.Errorf("a plan with no daemon names one:\n%s", s.stdout.String())
	}
	lock := filepath.Join(s.root, userStateDir, "serve.lock")
	unlock, ok, err := filelock.TryLock(lock)
	if err != nil || !ok {
		t.Fatalf("control: serve.lock: ok=%v err=%v", ok, err)
	}
	defer unlock()
	s.write(userStateDir+"/serve.lock", "4242 ui\n", 0o600)
	s.stdout.Reset()
	s.want(exitcode.OK, "uninstall", "--delete-data", "--plan")
	out := s.stdout.String()
	if !strings.Contains(out, "Note: pcb is already serving (pid 4242, kind ui)") || !strings.Contains(out, "quit pcb-ui") {
		t.Errorf("plan does not name the serving daemon and its fix:\n%s", out)
	}
}

// The confirmation names everything that goes: the keyring item (it cannot
// be recovered) and the autostart entry; an entry alone is confirmed too.
func TestUninstallDeleteDataConfirmationNamesTheKeyringAndAutostart(t *testing.T) {
	t.Parallel()
	s, _ := desktopInstall(t)
	s.term.interactive = true
	s.term.lines = []string{"no"}
	s.want(exitcode.MustAct, "uninstall", "--delete-data")
	if len(s.term.linePrompts) != 1 {
		t.Fatalf("prompts = %q, want one", s.term.linePrompts)
	}
	for _, want := range []string{"Delete " + userConfigDir, `the "pcb sealing key" item in the desktop keyring`, "pcb-ui's autostart entry"} {
		if !strings.Contains(s.term.linePrompts[0], want) {
			t.Errorf("prompt %q lacks %q", s.term.linePrompts[0], want)
		}
	}

	only := newSysRig(t)
	only.write(autostartEntry, "[Desktop Entry]\n", 0o600)
	only.want(exitcode.MustAct, "uninstall", "--delete-data", "--json")
	if _, msg, _ := only.failure(); !strings.Contains(msg, "pcb-ui's autostart entry") || !strings.Contains(msg, "--yes") {
		t.Errorf("refusal = %q, want the autostart entry and --yes named", msg)
	}
	if !only.exists(autostartEntry) {
		t.Error("the autostart entry was deleted without a confirmation")
	}
}
