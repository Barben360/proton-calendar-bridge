package service_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// Ready tells pcb serve when to bind its listener: never when Serve cannot
// run (logged out, the sync lock taken), and before the first pass once it
// holds the lock and its cache handle.
func TestServeCallsReadyOnlyOnceItRuns(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	var readies atomic.Int32
	o := service.ServeOptions{Ready: func() { readies.Add(1) }}

	err := e.svc.Serve(t.Context(), o)
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, "pcb login") {
		t.Errorf("logged out: hint %q", ee.Hint)
	}
	if n := readies.Load(); n != 0 {
		t.Fatalf("Ready called %d times while logged out", n)
	}

	e.login(t, e.req())
	if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := filelock.TryLock(filepath.Join(e.dirs.State, "sync.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	err = e.svc.Serve(t.Context(), o)
	unlock()
	if ee := wantExit(t, err, exitcode.Blocked); !strings.Contains(ee.Error(), "a sync is already running") {
		t.Errorf("lock taken: %v", err)
	}
	if n := readies.Load(); n != 0 {
		t.Fatalf("Ready called %d times while the lock was taken", n)
	}

	ctx, cancel := context.WithCancel(t.Context())
	var readyAtFirstPass int32 = -1
	o.OnPass = func(service.WatchPass) {
		if readyAtFirstPass < 0 {
			readyAtFirstPass = readies.Load()
		}
		cancel()
	}
	if err := e.svc.Serve(ctx, o); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if readyAtFirstPass != 1 || readies.Load() != 1 {
		t.Errorf("Ready calls at the first pass %d, in all %d; want 1 and 1", readyAtFirstPass, readies.Load())
	}
}
