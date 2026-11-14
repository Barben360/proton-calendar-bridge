package wingui

import (
	"os"

	"golang.org/x/sys/windows"
)

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
)

// attachParentProcess is AttachConsole's ATTACH_PARENT_PROCESS ((DWORD)-1).
const attachParentProcess = 0xFFFFFFFF

// needsConsole reports a standard handle that is missing: a windowsgui
// binary started from a terminal gets none. A pipe or a file (pcb-ui
// --version > v.txt, CI's bash) is a handle, kept as it is.
func needsConsole(h windows.Handle) bool { return h == 0 || h == windows.InvalidHandle }

// AttachParentConsole gives a windowsgui binary started from a terminal
// its stdout and stderr: when either is missing it attaches to the console
// of the parent process and reopens only the missing ones on CONOUT$. An
// inherited pipe or file is never replaced. Without a parent console
// (started from Explorer, or at login) it does nothing.
func AttachParentConsole() {
	out := needsConsole(stdHandle(windows.STD_OUTPUT_HANDLE))
	errh := needsConsole(stdHandle(windows.STD_ERROR_HANDLE))
	if !out && !errh {
		return
	}
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return
	}
	if out {
		if f := conout(windows.STD_OUTPUT_HANDLE, "/dev/stdout"); f != nil {
			os.Stdout = f
		}
	}
	if errh {
		if f := conout(windows.STD_ERROR_HANDLE, "/dev/stderr"); f != nil {
			os.Stderr = f
		}
	}
}

// HasStderr reports whether stderr reaches something: an inherited handle,
// or the console AttachParentConsole found.
func HasStderr() bool { return !needsConsole(stdHandle(windows.STD_ERROR_HANDLE)) }

func stdHandle(which uint32) windows.Handle {
	h, err := windows.GetStdHandle(which)
	if err != nil {
		return windows.InvalidHandle
	}
	return h
}

// conout opens the attached console's output as the standard handle which.
func conout(which uint32, name string) *os.File {
	p, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return nil
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil
	}
	_ = windows.SetStdHandle(which, h)
	return os.NewFile(uintptr(h), name)
}
