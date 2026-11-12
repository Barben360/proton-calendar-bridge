package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
)

func TestServiceDetectsTheScope(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.MustAct, "service", "start", "--json")
	if _, msg, hint := s.failure(); msg != "pcb is not installed as a service" || hint != "pcb install" {
		t.Errorf("not installed: %q, hint %q", msg, hint)
	}

	s.write(userUnit, userUnitContent(), 0o644)
	s.want(exitcode.OK, "service", "start")
	s.write(install.SystemUnitPath, "[Unit]\n", 0o644)
	s.want(exitcode.Usage, "service", "stop", "--json")
	if _, msg, _ := s.failure(); !strings.Contains(msg, "both") {
		t.Errorf("ambiguous: %q", msg)
	}
	s.want(exitcode.OK, "service", "stop", "--user")
	if err := os.Remove(filepath.Join(s.root, userUnit)); err != nil {
		t.Fatal(err)
	}
	s.euid = 0
	s.want(exitcode.OK, "service", "restart")
	if got, want := s.takeEvents(), []string{
		"systemctl --user start pcb.service",
		"systemctl --user stop pcb.service",
		"systemctl restart pcb.service",
	}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

// M5-R7: changing the system service as non-root is refused before
// anything runs; reading it (status, logs) is not.
func TestServiceSystemNeedsRoot(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.write(install.SystemUnitPath, "[Unit]\n", 0o644)
	for _, verb := range []string{"start", "stop", "restart"} {
		s.want(exitcode.MustAct, "service", verb, "--json")
		if _, msg, hint := s.failure(); msg != "needs root: sudo pcb service "+verb || hint != "sudo pcb service "+verb {
			t.Errorf("%s: %q, hint %q", verb, msg, hint)
		}
	}
	if e := s.takeEvents(); len(e) != 0 {
		t.Errorf("ran %q as non-root on the system scope", e)
	}
	const show = "systemctl show pcb.service --property=ActiveState,SubState,UnitFileState,MainPID"
	s.out[show] = "ActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=7\n"
	s.want(exitcode.OK, "service", "status")
	if want := "pcb.service (system): active (running), enabled, PID 7\n"; s.stdout.String() != want {
		t.Errorf("status = %q, want %q", s.stdout.String(), want)
	}
	if got := s.takeEvents(); !slices.Equal(got, []string{show}) {
		t.Errorf("status ran %q", got)
	}
}

// Non-root system logs: the journal may show nothing without the
// systemd-journal group, and pcb then says so; with entries it does not.
func TestServiceSystemLogsWithoutRoot(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.write(install.SystemUnitPath, "[Unit]\n", 0o644)
	const logs = "journalctl -u pcb.service --no-pager -n 200"
	const hint = "Hint: system logs need root or the systemd-journal group: sudo pcb service logs\n"
	for _, out := range []string{"", "-- No entries --\n"} {
		s.out[logs] = out
		s.want(exitcode.OK, "service", "logs")
		if s.errb.String() != hint {
			t.Errorf("journal %q: stderr = %q, want %q", out, s.errb.String(), hint)
		}
	}
	s.out[logs] = "nov. 12 pcb[7]: serving\n"
	s.want(exitcode.OK, "service", "logs")
	if s.errb.Len() != 0 || s.stdout.String() != "nov. 12 pcb[7]: serving\n" {
		t.Errorf("with entries: stdout %q, stderr %q", s.stdout.String(), s.errb.String())
	}
	s.euid = 0
	s.out[logs] = ""
	s.want(exitcode.OK, "service", "logs")
	if s.errb.Len() != 0 {
		t.Errorf("root got the hint: %q", s.errb.String())
	}
	if got := s.takeEvents(); len(got) != 4 || got[0] != logs {
		t.Errorf("ran %q", got)
	}
}

func TestServiceStatusParsesShow(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.write(userUnit, userUnitContent(), 0o644)
	const show = "systemctl --user show pcb.service --property=ActiveState,SubState,UnitFileState,MainPID"
	s.out[show] = "ActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=4242\n"
	s.want(exitcode.OK, "service", "status", "--json")
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(s.stdout.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"scope": "user", "unit": userUnit, "active_state": "active", "sub_state": "running",
		"unit_file_state": "enabled", "main_pid": float64(4242)}
	if len(env.Data) != len(want) {
		t.Errorf("data = %v, want %v", env.Data, want)
	}
	for k, v := range want {
		if env.Data[k] != v {
			t.Errorf("%s = %v, want %v", k, env.Data[k], v)
		}
	}
	s.want(exitcode.OK, "service", "status")
	if want := "pcb.service (user): active (running), enabled, PID 4242\n"; s.stdout.String() != want {
		t.Errorf("text = %q, want %q", s.stdout.String(), want)
	}
	// Any state is a result, not a failure.
	s.out[show] = "ActiveState=failed\nSubState=failed\nUnitFileState=enabled\nMainPID=0\n"
	s.want(exitcode.OK, "service", "status")
	if want := "pcb.service (user): failed (failed), enabled\n"; s.stdout.String() != want {
		t.Errorf("text = %q, want %q", s.stdout.String(), want)
	}
	// systemctl failing: its last stderr line is the message.
	s.codes[show] = 1
	s.stderr[show] = "Failed to connect to bus: No medium found\n"
	s.want(exitcode.Blocked, "service", "status", "--json")
	if _, msg, _ := s.failure(); !strings.Contains(msg, "Failed to connect to bus: No medium found") {
		t.Errorf("failure = %q, want systemctl's stderr", msg)
	}
}

func TestServiceLogsFollow(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.write(userUnit, userUnitContent(), 0o644)
	const follow = "journalctl --user -u pcb.service --no-pager -n 200 -f"
	s.out[follow] = "nov. 12 pcb[42]: serving\n"
	s.want(exitcode.OK, "service", "logs", "-f")
	if s.stdout.String() != "nov. 12 pcb[42]: serving\n" {
		t.Errorf("stdout = %q, want the journal", s.stdout.String())
	}
	s.want(exitcode.OK, "service", "logs")
	s.want(exitcode.Usage, "service", "logs", "--json")
	if got, want := s.takeEvents(), []string{follow, "journalctl --user -u pcb.service --no-pager -n 200"}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestServiceStartFailureIsBlocked(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.write(userUnit, userUnitContent(), 0o644)
	const start = "systemctl --user start pcb.service"
	s.codes[start] = 1
	s.stderr[start] = "Warning: something\nJob for pcb.service failed because the control process exited with error code.\n"
	s.want(exitcode.Blocked, "service", "start", "--json")
	if _, msg, hint := s.failure(); !strings.Contains(msg, "Job for pcb.service failed because the control process exited with error code.") ||
		strings.Contains(msg, "Warning") || hint != "pcb service logs" {
		t.Errorf("failure = %q, hint %q", msg, hint)
	}
}

func TestDoctorReportsTheService(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.env["HOME"] = t.TempDir()
	result := func() (status, message string) {
		t.Helper()
		s.run("doctor", "--json")
		var env struct {
			Data struct {
				Results []struct{ Name, Status, Message string } `json:"results"`
			} `json:"data"`
		}
		if err := json.Unmarshal(s.stdout.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		for _, r := range env.Data.Results {
			if r.Name == "service" {
				return r.Status, r.Message
			}
		}
		t.Fatalf("no service result: %s", s.stdout.String())
		return "", ""
	}
	if st, msg := result(); st != "skip" || msg != "not installed as a service (pcb install)" {
		t.Errorf("not installed: %s %q", st, msg)
	}
	s.write(install.SystemUnitPath, "[Unit]\n", 0o644)
	s.out["systemctl show pcb.service --property=ActiveState,SubState,UnitFileState,MainPID"] = "ActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=7\n"
	if st, msg := result(); st != "ok" || msg != "system: active" {
		t.Errorf("active: %s %q", st, msg)
	}
	s.out["systemctl show pcb.service --property=ActiveState,SubState,UnitFileState,MainPID"] = "ActiveState=failed\nSubState=failed\nUnitFileState=enabled\nMainPID=0\n"
	if st, msg := result(); st != "warn" || msg != "system: failed" {
		t.Errorf("failed: %s %q", st, msg)
	}
	if err := os.RemoveAll(filepath.Join(s.root, "run")); err != nil {
		t.Fatal(err)
	}
	if st, msg := result(); st != "skip" || msg != "no systemd here" {
		t.Errorf("no systemd: %s %q", st, msg)
	}
}
