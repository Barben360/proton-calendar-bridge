package report_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/report"
)

// helperEnv names the crash a re-executed test binary performs instead of
// running the tests: its stderr is a real Go traceback.
const helperEnv = "PCB_REPORT_HELPER"

// stateEnv names the state directory the crash-file helpers write to.
const stateEnv = "PCB_REPORT_HELPER_STATE"

// crashMeta is the Meta the crash-file helpers declare; its command holds a
// space and an "=" to check the header's escaping.
var crashMeta = report.Meta{Binary: "pcb", Command: "calendar a=b", Install: "test", Version: "1.2.3"}

func TestMain(m *testing.M) {
	if name := os.Getenv(helperEnv); name != "" {
		runHelper(name)
		os.Exit(3) // unknown helper, or one that did not crash
	}
	os.Exit(m.Run())
}

//go:noinline
func runHelper(name string) {
	switch name {
	case "goroutine-panic":
		helperGoroutinePanic("MARKER-7f3a")
	case "runtime-error":
		helperRuntimeError(5)
	case "map-fatal":
		helperMapFatal()
	case "crashfile-goroutine":
		_, _ = report.StartCrashFile(os.Getenv(stateEnv), crashMeta)
		helperGoroutinePanic("MARKER-7f3a")
	case "crashfile-clean":
		stop, _ := report.StartCrashFile(os.Getenv(stateEnv), crashMeta)
		stop()
		os.Exit(0)
	case "runtime-lookalike":
		helperLookalike("MARKER-7f3a")
	}
}

// helperGoroutinePanic panics in a goroutine it starts, the panic text and
// an argument carrying word; it never returns.
//
//go:noinline
func helperGoroutinePanic(word string) {
	go helperPanicky(word)
	select {}
}

//go:noinline
func helperPanicky(word string) { panic("helper " + word) }

//go:noinline
func helperRuntimeError(i int) int {
	a := []int{1, 2, 3}
	return a[i]
}

// helperLookalike panics with an error of its own whose text starts like
// the runtime's: Go prints it "panic: runtime error: …".
//
//go:noinline
func helperLookalike(word string) { panic(errors.New("runtime error: " + word)) }

// helperMapFatal writes one map from several goroutines until the runtime
// stops the process with "concurrent map writes"; helperIdle sleeps in
// another goroutine, so a GOTRACEBACK=all traceback has more than one block.
//
//go:noinline
func helperMapFatal() {
	go helperIdle()
	m := map[int]int{}
	for i := range 4 {
		go helperMapWriter(m, i)
	}
	time.Sleep(time.Minute)
}

//go:noinline
func helperIdle() { time.Sleep(time.Hour) }

//go:noinline
func helperMapWriter(m map[int]int, i int) {
	for {
		m[i] = i
	}
}

// crashOutput re-executes the test binary as helper name and returns its
// stderr, failing unless the helper died with a non-zero status.
func crashOutput(t *testing.T, name string, env ...string) []byte {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(append(os.Environ(), helperEnv+"="+name, "GOTRACEBACK=single"), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() == 3 {
		t.Fatalf("helper %s: %v (stderr %q)", name, err, stderr.Bytes())
	}
	return stderr.Bytes()
}
