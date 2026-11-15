package cli

import "golang.org/x/sys/windows"

// descriptor is the handle --password-fd N reads on Windows, where a
// descriptor number is not a handle: 0, 1 and 2 are the standard handles
// (Get-Content pw | pcb login --password-fd 0; handle 0 is never valid,
// live 2026-11-15), any other N a handle value inherited from the parent.
func descriptor(fd int) uintptr {
	std := map[int]uint32{0: windows.STD_INPUT_HANDLE, 1: windows.STD_OUTPUT_HANDLE, 2: windows.STD_ERROR_HANDLE}
	if n, ok := std[fd]; ok {
		if h, err := windows.GetStdHandle(n); err == nil {
			return uintptr(h)
		}
	}
	return uintptr(fd)
}
