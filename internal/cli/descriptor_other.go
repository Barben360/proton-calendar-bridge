//go:build !windows

package cli

// descriptor is the operating system's handle for descriptor fd: fd itself.
func descriptor(fd int) uintptr { return uintptr(fd) }
