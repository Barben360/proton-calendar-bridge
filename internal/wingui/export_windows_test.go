package wingui

import (
	"testing"

	"golang.org/x/sys/windows"
)

var (
	procSendMessageW = user32.NewProc("SendMessageW")
	procGetAncestor  = user32.NewProc("GetAncestor")
)

// gaParent is GetAncestor's GA_PARENT.
const gaParent = 1

func getAncestor(hwnd windows.HWND, flags uint32) windows.HWND {
	r, _, _ := procGetAncestor.Call(uintptr(hwnd), uintptr(flags))
	return windows.HWND(r)
}

func getDesktopWindow() windows.HWND  { return windows.GetDesktopWindow() }
func isWindow(hwnd windows.HWND) bool { return windows.IsWindow(hwnd) }

// hwndForTest is the window of the one running watcher.
func hwndForTest(t *testing.T) windows.HWND {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if len(watchers) != 1 {
		t.Fatalf("%d watchers running, want 1", len(watchers))
	}
	for h := range watchers {
		return h
	}
	return 0
}

// sendMessage delivers msg to hwnd and waits for its window procedure's
// answer, as Windows does with the session-end messages (lParam 0: a
// logoff carries ENDSESSION_LOGOFF there, which the watcher ignores).
func sendMessage(hwnd windows.HWND, msg uint32, wParam uintptr) uintptr {
	r, _, _ := procSendMessageW.Call(uintptr(hwnd), uintptr(msg), wParam, 0)
	return r
}
