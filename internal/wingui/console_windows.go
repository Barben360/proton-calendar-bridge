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
	out, errf := reopen(stdHandle(windows.STD_OUTPUT_HANDLE), stdHandle(windows.STD_ERROR_HANDLE), attachParent, conout)
	if out != nil {
		os.Stdout = out
	}
	if errf != nil {
		os.Stderr = errf
	}
}

// reopen is AttachParentConsole's choice: given the current stdout and
// stderr handles, it attaches (attach reports success) only when one is
// missing, and opens (open) only the missing ones; a nil result keeps that
// handle as it is.
func reopen(stdout, stderr windows.Handle, attach func() bool, open func(which uint32, name string) *os.File) (out, errf *os.File) {
	needOut, needErr := needsConsole(stdout), needsConsole(stderr)
	if !needOut && !needErr || !attach() {
		return nil, nil
	}
	if needOut {
		out = open(windows.STD_OUTPUT_HANDLE, "/dev/stdout")
	}
	if needErr {
		errf = open(windows.STD_ERROR_HANDLE, "/dev/stderr")
	}
	return out, errf
}

// attachParent attaches to the console of the parent process, if it has one.
func attachParent() bool {
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	return r != 0
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
