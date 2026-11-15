//go:build linux && cgo

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// No graphical session, or one pcb-ui cannot open (live 2026-11-13, F8:
// over SSH with DISPLAY set and no X authority, Fyne printed its raw GLFW
// error and the process panicked, exit 2): a pcb message saying what is
// missing and what to do, exit 10 (the user must act), before any state is
// opened.
func TestCheckDisplay(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	opened := 0
	ok := func() error { opened++; return nil }
	err := checkDisplay(env(nil), ok)
	if err == nil || err.Error() != noDisplayLine || exitcode.From(err) != exitcode.MustAct || !strings.Contains(exitcode.HintOf(err), "pcb serve") || opened != 0 {
		t.Errorf("no display: err=%v (exit %d, hint %q), opened %d", err, exitcode.From(err), exitcode.HintOf(err), opened)
	}
	for _, k := range []string{"DISPLAY", "WAYLAND_DISPLAY"} {
		if err := checkDisplay(env(map[string]string{k: ":0"}), ok); err != nil {
			t.Errorf("%s set: %v", k, err)
		}
	}
	if opened != 2 {
		t.Errorf("the display was opened %d times, want once per check that has one", opened)
	}
	err = checkDisplay(env(map[string]string{"DISPLAY": ":0"}), func() error { return errors.New("X11: Failed to open display :0") })
	if err == nil || exitcode.From(err) != exitcode.MustAct ||
		err.Error() != "pcb-ui cannot open the display (DISPLAY=:0): X11: Failed to open display :0" ||
		exitcode.HintOf(err) != displayHint {
		t.Errorf("unusable display: err=%v (exit %d, hint %q)", err, exitcode.From(err), exitcode.HintOf(err))
	}
}

// The real binary's path with a display GLFW cannot open: the pcb message
// on stderr, exit 10, no Fyne error or panic, and no state created (the
// sealer is not chosen by a start that cannot show anything).
func TestUnusableDisplayIsAPcbMessage(t *testing.T) {
	if os.Getenv("PCB_UI_TEST_RUN") == "1" {
		t.Skip("the child")
	}
	tmp, err := os.MkdirTemp("", "pcbx") // short: a Wayland socket path is bounded
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	runtimeDir := filepath.Join(tmp, "rt")
	if err := os.Mkdir(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = []string{
		"PCB_UI_TEST_RUN=1", "HOME=" + tmp, "PATH=" + os.Getenv("PATH"),
		"DISPLAY=:247", "XDG_RUNTIME_DIR=" + runtimeDir, "XDG_SESSION_TYPE=x11",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + filepath.Join(tmp, "nobus"),
		"PCB_CONFIG_DIR=" + filepath.Join(tmp, "cfg"), "PCB_STATE_DIR=" + filepath.Join(tmp, "state"),
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 10 {
		t.Fatalf("exit: %v, want 10\nstderr: %s", err, stderr.String())
	}
	out := stderr.String()
	for _, want := range []string{"Error: pcb-ui cannot open the display (DISPLAY=:247)", "Hint: " + displayHint} {
		if !strings.Contains(out, want) {
			t.Errorf("stderr lacks %q:\n%s", want, out)
		}
	}
	for _, raw := range []string{"Fyne error", "panic", "goroutine"} {
		if strings.Contains(out, raw) {
			t.Errorf("stderr holds %q:\n%s", raw, out)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the state directory was created: %v", err)
	}
}
