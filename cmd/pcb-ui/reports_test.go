//go:build (linux || windows) && cgo

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
	"github.com/Barben360/proton-calendar-bridge/internal/ui"
)

// reportsChildEnv makes the test binary run withReports as pcb-ui's run
// does, in the state directory named by reportsStateEnv: "panic" panics on
// the main goroutine (as a Fyne callback would), "clean" returns.
const (
	reportsChildEnv = "PCB_UI_TEST_REPORTS"
	reportsStateEnv = "PCB_UI_TEST_REPORTS_STATE"
)

func reportsChild(how string) int {
	start := func() (*ui.Reports, func()) {
		return ui.StartReports(os.Getenv(reportsStateEnv), "https://k@127.0.0.1/1",
			report.Meta{Binary: "pcb-ui", Command: "pcb-ui", Install: "test", Version: "0"},
			func(string) string { return "" }, nil)
	}
	return withReports(start, func(r *ui.Reports) int {
		if r == nil {
			return 4 // reports did not start: the test would prove nothing
		}
		if how == "panic" {
			panic("pcb-ui test panic")
		}
		return 0
	})
}

func runReportsChild(t *testing.T, how string) (state string, err error) {
	t.Helper()
	state = t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), reportsChildEnv+"="+how, reportsStateEnv+"="+state, "GOTRACEBACK=single")
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 4 {
		t.Fatalf("reports did not start in the child: %s", out)
	}
	return state, err
}

func crashFilesIn(t *testing.T, state string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(report.Dir(state), "crash-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A panic on pcb-ui's main goroutine (every Fyne callback runs there)
// leaves its traceback in the crash file, offered at the next start.
func TestMainGoroutinePanicKeepsTheCrashFile(t *testing.T) {
	state, err := runReportsChild(t, "panic")
	if err == nil {
		t.Fatal("the child did not crash")
	}
	files := crashFilesIn(t, state)
	if len(files) != 1 {
		t.Fatalf("crash files %v, want one", files)
	}
	s := report.Store{StateDir: state, Env: func(string) string { return "" }}
	if n, err := s.Collect(context.Background()); err != nil || n != 1 {
		t.Fatalf("Collect = %d, %v; want the crash as one report", n, err)
	}
}

func TestCleanReturnRemovesTheCrashFile(t *testing.T) {
	state, err := runReportsChild(t, "clean")
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	if files := crashFilesIn(t, state); len(files) != 0 {
		t.Errorf("a clean return left %v", files)
	}
}
