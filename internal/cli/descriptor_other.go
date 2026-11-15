//go:build !windows

package cli

// passwordFDHint is how to hand pcb a password on a descriptor.
const passwordFDHint = "pipe the password in (pcb login --password-fd 3 3<file), or omit --password-fd to be prompted"

// descriptor is the operating system's handle for descriptor fd: fd itself.
func descriptor(fd int) (uintptr, error) { return uintptr(fd), nil }
