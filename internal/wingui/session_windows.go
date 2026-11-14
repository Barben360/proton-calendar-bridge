package wingui

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The window messages the watcher handles.
const (
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
)

var (
	user32               = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
)

// wndClassEx is WNDCLASSEXW.
type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

// msg is MSG.
type msg struct {
	hwnd     windows.HWND
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       struct{ x, y int32 }
	lPrivate uint32
}

var (
	register    sync.Once
	registerErr error
	className   *uint16
	instance    windows.Handle

	mu       sync.Mutex
	watchers = map[windows.HWND]*watcher{}
)

// watcher is one invisible window and the thread that pumps its messages.
type watcher struct {
	end  func()
	once sync.Once
	hwnd windows.HWND
	done chan struct{}
}

// WatchSessionEnd runs end when the Windows session ends (logoff,
// shutdown): an invisible top-level window (a message-only one gets no
// session broadcast) answers WM_QUERYENDSESSION with yes, and on
// WM_ENDSESSION, unless the end was cancelled, runs end once and only then
// answers, since Windows may end the process as soon as it has. stop
// closes the window and waits for its thread; it may be called twice, never
// from end.
func WatchSessionEnd(end func()) (stop func(), err error) {
	register.Do(registerClass)
	if registerErr != nil {
		return nil, registerErr
	}
	w := &watcher{end: end, done: make(chan struct{})}
	ready := make(chan error, 1)
	go w.loop(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _, _ = procPostMessageW.Call(uintptr(w.hwnd), wmClose, 0, 0)
			<-w.done
		})
	}, nil
}

func registerClass() {
	if err := windows.GetModuleHandleEx(0, nil, &instance); err != nil {
		registerErr = fmt.Errorf("GetModuleHandleEx: %w", err)
		return
	}
	className = windows.StringToUTF16Ptr("pcb-ui-session-end")
	wc := wndClassEx{wndProc: windows.NewCallback(wndProc), instance: instance, className: className}
	wc.size = uint32(unsafe.Sizeof(wc))
	if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		registerErr = fmt.Errorf("RegisterClassEx: %w", e)
	}
}

// loop creates the window on a thread of its own (a window belongs to the
// thread that creates it, and only that thread gets its messages) and
// pumps its messages until WM_CLOSE destroys it.
func (w *watcher) loop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(w.done)
	// No WS_VISIBLE, no parent: top level, never shown.
	r, _, e := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0, 0, uintptr(instance), 0)
	if r == 0 {
		ready <- fmt.Errorf("CreateWindowEx: %w", e)
		return
	}
	w.hwnd = windows.HWND(r)
	mu.Lock()
	watchers[w.hwnd] = w
	mu.Unlock()
	defer func() {
		mu.Lock()
		delete(watchers, w.hwnd)
		mu.Unlock()
	}()
	ready <- nil
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // WM_QUIT, or an error
			return
		}
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd windows.HWND, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmQueryEndSession:
		return 1
	case wmEndSession:
		if wParam != 0 {
			mu.Lock()
			w := watchers[hwnd]
			mu.Unlock()
			if w != nil {
				w.once.Do(w.end)
			}
		}
		return 0
	case wmDestroy:
		_, _, _ = procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(message), wParam, lParam)
	return r
}
