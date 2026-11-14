package wingui

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// pcb-ui --version > v.txt (or piped, as CI's bash runs it): the inherited
// handle is used, never replaced by the parent's console. Only a missing
// handle (a windowsgui binary started from a terminal) asks for the console.
func TestNeedsConsole(t *testing.T) {
	if !needsConsole(0) || !needsConsole(windows.InvalidHandle) {
		t.Error("a missing handle (0 or INVALID_HANDLE_VALUE) does not ask for the console")
	}
	// go test hands the test binary a pipe or a file, never nothing.
	if h := windows.Handle(os.Stdout.Fd()); needsConsole(h) {
		t.Errorf("the test's stdout (handle %#x) would be replaced by the console", h)
	}
	f, err := os.Create(t.TempDir() + `\v.txt`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if needsConsole(windows.Handle(f.Fd())) {
		t.Error("a redirected file would be replaced by the console")
	}
}

// AttachParentConsole keeps handles that are already there: under go test
// stdout and stderr are pipes or files, and must stay the same files.
func TestAttachParentConsoleKeepsInheritedHandles(t *testing.T) {
	out, errf := os.Stdout, os.Stderr
	AttachParentConsole()
	if os.Stdout != out || os.Stderr != errf {
		t.Errorf("stdout or stderr replaced although inherited (%v %v → %v %v)", out.Fd(), errf.Fd(), os.Stdout.Fd(), os.Stderr.Fd())
	}
}

// At logoff or shutdown Windows asks (WM_QUERYENDSESSION, answered yes),
// then tells (WM_ENDSESSION, wParam TRUE) and may end the process as soon as
// that message returns: end must have run, to its end, by then. Once only.
func TestSessionEndRunsEndBeforeAnswering(t *testing.T) {
	var calls atomic.Int32
	var done atomic.Bool
	stop, err := WatchSessionEnd(func() {
		calls.Add(1)
		time.Sleep(200 * time.Millisecond)
		done.Store(true)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	hwnd := hwndForTest(t)
	if r := sendMessage(hwnd, wmQueryEndSession, 0); r != 1 {
		t.Errorf("WM_QUERYENDSESSION answered %d, want 1 (the session may end)", r)
	}
	if calls.Load() != 0 {
		t.Error("end ran on WM_QUERYENDSESSION: the session end may still be cancelled")
	}
	sendMessage(hwnd, wmEndSession, 1)
	if !done.Load() {
		t.Error("WM_ENDSESSION returned before end finished")
	}
	sendMessage(hwnd, wmEndSession, 1)
	if n := calls.Load(); n != 1 {
		t.Errorf("end ran %d times, want once", n)
	}
}

// A cancelled session end (WM_ENDSESSION with wParam FALSE) leaves pcb-ui
// running; stop ends the watcher promptly, and twice is harmless.
func TestCancelledSessionEndDoesNotEnd(t *testing.T) {
	var calls atomic.Int32
	stop, err := WatchSessionEnd(func() { calls.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	hwnd := hwndForTest(t)
	sendMessage(hwnd, wmQueryEndSession, 0)
	sendMessage(hwnd, wmEndSession, 0)
	if n := calls.Load(); n != 0 {
		t.Errorf("end ran %d times on a cancelled session end", n)
	}
	stopped := make(chan struct{})
	go func() { stop(); stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not return within 1 s")
	}
	if isWindow(hwnd) {
		t.Error("the window is still there after stop")
	}
	mu.Lock()
	n := len(watchers)
	mu.Unlock()
	if n != 0 {
		t.Errorf("%d watchers left after stop", n)
	}
}

// The watcher's window is top level: a message-only window (HWND_MESSAGE)
// gets no session-end broadcast.
func TestWatcherWindowIsTopLevel(t *testing.T) {
	stop, err := WatchSessionEnd(func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	hwnd := hwndForTest(t)
	if p := getAncestor(hwnd, gaParent); p != getDesktopWindow() {
		t.Errorf("parent %#x, want the desktop window %#x (a top-level window)", p, getDesktopWindow())
	}
}
