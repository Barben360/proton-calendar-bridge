//go:build !windows

package cli

// fdHint is how to hand pcb the secret s on a descriptor.
func fdHint(s fdSecret) string {
	if s == mailboxFDSecret {
		return "pipe the mailbox password in (pcb login --mailbox-password-fd 4 4<file), or omit --mailbox-password-fd to be prompted"
	}
	return "pipe the password in (pcb login --password-fd 3 3<file), or omit --password-fd to be prompted"
}

// descriptor is the operating system's handle for descriptor fd: fd itself.
func descriptor(fd int, _ fdSecret) (uintptr, error) { return uintptr(fd), nil }
