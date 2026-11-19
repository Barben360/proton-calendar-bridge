package report_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

func crashFiles(t *testing.T, state string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(report.Dir(state), "crash-*.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// crashInHelper runs the goroutine-panic helper with a crash file in state.
func crashInHelper(t *testing.T, state string) {
	t.Helper()
	crashOutput(t, "crashfile-goroutine", stateEnv+"="+state)
}

func TestCrashInGoroutineBecomesPending(t *testing.T) {
	state := t.TempDir()
	crashInHelper(t, state)
	if len(crashFiles(t, state)) != 1 {
		t.Fatalf("crash files %v, want one", crashFiles(t, state))
	}
	s := report.Store{StateDir: state}
	n, err := s.Collect(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("Collect = %d, %v; want 1", n, err)
	}
	if left, _ := filepath.Glob(filepath.Join(report.Dir(state), "crash-*")); len(left) != 0 {
		t.Errorf("collected file or its lock left: %v", left)
	}
	pend, err := s.Pending()
	if err != nil || len(pend) != 1 {
		t.Fatalf("pending %v, %v", pend, err)
	}
	p := pend[0]
	if p.Level != "fatal" || p.Source != "crash" {
		t.Errorf("level %q source %q", p.Level, p.Source)
	}
	if strings.Contains(string(p.JSON), "MARKER-7f3a") {
		t.Errorf("marker leaked: %s", p.JSON)
	}
	var ev struct {
		Release string
		Tags    struct{ Binary, Command, Install string }
		Exc     struct {
			Values []struct {
				Stacktrace struct {
					Frames []struct {
						Function string
						InApp    bool `json:"in_app"`
					}
				}
			}
		} `json:"exception"`
	}
	if err := json.Unmarshal(p.JSON, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Release != "pcb@1.2.3" || ev.Tags.Binary != "pcb" || ev.Tags.Command != "calendar a=b" || ev.Tags.Install != "test" {
		t.Errorf("meta lost: %+v", ev)
	}
	found := false
	for _, f := range ev.Exc.Values[0].Stacktrace.Frames {
		if strings.HasSuffix(f.Function, ".helperPanicky") && f.InApp {
			found = true
		}
	}
	if !found {
		t.Errorf("no in-app helperPanicky frame in %s", p.JSON)
	}
}

func TestCleanExitLeavesNothing(t *testing.T) {
	state := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=crashfile-clean", stateEnv+"="+state)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, out)
	}
	if files := crashFiles(t, state); len(files) != 0 {
		t.Errorf("clean exit left %v", files)
	}
	if n, err := (report.Store{StateDir: state}).Collect(context.Background()); err != nil || n != 0 {
		t.Errorf("Collect = %d, %v", n, err)
	}
}

func TestOwnFileNotCollected(t *testing.T) {
	state := t.TempDir()
	stop, err := report.StartCrashFile(state, crashMeta)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	own := crashFiles(t, state)
	if len(own) != 1 {
		t.Fatalf("crash files %v", own)
	}
	// Even with something in it, a running process's own file is its own.
	if err := os.WriteFile(own[0], []byte("panic: x\n\ngoroutine 1 [running]:\nmain.main()\n\tmain.go:1 +0x1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := (report.Store{StateDir: state}).Collect(context.Background()); err != nil || n != 0 {
		t.Fatalf("Collect = %d, %v", n, err)
	}
	if _, err := os.Stat(own[0]); err != nil {
		t.Errorf("own file gone: %v", err)
	}
	stop()
	if files := crashFiles(t, state); len(files) != 0 {
		t.Errorf("stop left %v", files)
	}
}

// writeDeadCrash writes a crash file with a traceback that no process holds,
// as one a dead process left, named for pid.
func writeDeadCrash(t *testing.T, state string, pid int) string {
	t.Helper()
	dir := report.Dir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("crash-pcb-%d-1.txt", pid))
	body := "# pcb crash file v1 binary=pcb command=x version=1 install=test\npanic: x\n\ngoroutine 1 [running]:\nmain.main()\n\tmain.go:1 +0x1\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A pid is reused: in a container pcb serve is PID 1 at every start, so
// the crash file of the previous PID 1 carries this process's pid. It is
// collected all the same, while this process's own file is kept.
func TestDeadFileWithOwnPidCollected(t *testing.T) {
	state := t.TempDir()
	stop, err := report.StartCrashFile(state, crashMeta)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	own := crashFiles(t, state)
	dead := writeDeadCrash(t, state, os.Getpid())
	s := report.Store{StateDir: state}
	if n, err := s.Collect(context.Background()); err != nil || n != 1 {
		t.Fatalf("Collect = %d, %v; want the dead file collected", n, err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Errorf("dead file with this pid kept: %v", err)
	}
	if pend, _ := s.Pending(); len(pend) != 1 {
		t.Errorf("%d pending, want 1", len(pend))
	}
	if files := crashFiles(t, state); len(own) != 1 || len(files) != 1 || files[0] != own[0] {
		t.Errorf("crash files %v, want only this process's own %v", files, own)
	}
}

// A header-only file nobody holds is a process that died without a crash.
func TestDeadProcessEmptyFileRemoved(t *testing.T) {
	state := t.TempDir()
	dir := report.Dir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "crash-pcb-999999-1.txt")
	if err := os.WriteFile(path, []byte("# pcb crash file v1 binary=pcb command=x version=1 install=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := (report.Store{StateDir: state}).Collect(context.Background()); err != nil || n != 0 {
		t.Fatalf("Collect = %d, %v", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty file of a dead process kept: %v", err)
	}
}

// A long-running pcb (serve, the daemon) keeps its crash file, however old,
// so that a later crash still lands in it.
func TestRunningProcessFileNeverCollected(t *testing.T) {
	state := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=crashfile-live", stateEnv+"="+state)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper said %q, %v", line, err)
	}
	files := crashFiles(t, state)
	if len(files) != 1 {
		t.Fatalf("crash files %v", files)
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(files[0], past, past); err != nil {
		t.Fatal(err)
	}
	s := report.Store{StateDir: state}
	if n, err := s.Collect(context.Background()); err != nil || n != 0 {
		t.Fatalf("Collect = %d, %v", n, err)
	}
	if _, err := os.Stat(files[0]); err != nil {
		t.Fatalf("a running process's crash file was deleted: %v", err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(report.Dir(state), "crash-*")); len(left) != 0 {
		t.Errorf("clean exit left %v", left)
	}
}

func TestCollectConcurrentMakesOnePending(t *testing.T) {
	state := t.TempDir()
	crashInHelper(t, state)
	s := report.Store{StateDir: state}
	var wg sync.WaitGroup
	counts := make([]int, 2)
	for i := range counts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.Collect(context.Background())
			if err != nil {
				t.Error(err)
			}
			counts[i] = n
		}()
	}
	wg.Wait()
	if counts[0]+counts[1] != 1 {
		t.Errorf("saved %v, want 1 in all", counts)
	}
	if pend, _ := s.Pending(); len(pend) != 1 {
		t.Errorf("%d pending, want 1", len(pend))
	}
}

func TestUnparsableFileStillReported(t *testing.T) {
	state := t.TempDir()
	dir := report.Dir(state)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "# pcb crash file v1 binary=pcb command=x version=1 install=test\nsomething MARKER-7f3a\n"
	if err := os.WriteFile(filepath.Join(dir, "crash-pcb-999999-1.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s := report.Store{StateDir: state}
	if n, err := s.Collect(context.Background()); err != nil || n != 1 {
		t.Fatalf("Collect = %d, %v", n, err)
	}
	pend, _ := s.Pending()
	if len(pend) != 1 || strings.Contains(string(pend[0].JSON), "MARKER-7f3a") || !strings.Contains(string(pend[0].JSON), `"type":"panic"`) {
		t.Errorf("pending %v", pend)
	}
}

func TestNoStateDirNoCrashFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	stop, err := report.StartCrashFile(missing, crashMeta)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("state directory created: %v", err)
	}
	if n, err := (report.Store{StateDir: missing}).Collect(context.Background()); err != nil || n != 0 {
		t.Errorf("Collect = %d, %v", n, err)
	}
}
