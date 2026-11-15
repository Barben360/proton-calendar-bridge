//go:build windows

package tlsutil

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

// ownerSID is acl.Owner; tests replace it.
var ownerSID = acl.Owner

// accessError refuses a TLS file or directory for its access list; it
// unwraps to acl.Check's *acl.OpenError, which pcb serve maps to exit 10
// (the person's to fix, with pcb doctor's icacls line).
type accessError struct{ open *acl.OpenError }

func (e *accessError) Error() string {
	return e.open.Path + ": its access list lets " + e.open.Who() + " in: pcb will not use it (run pcb doctor for the fix)"
}

func (e *accessError) Unwrap() error { return e.open }

// guard refuses path when its access list lets in an account other than
// the user, SYSTEM or Administrators, and a directory owned by an account
// pcb does not accept (acl.OwnerAccepted). A file's owner is not checked:
// an elevated run's files are owned by Administrators, and its access list
// is what keeps other users out. Nothing is repaired (pcb doctor gives the
// fix), so it never returns a path.
func guard(path string, _ fs.FileInfo, _ fs.FileMode, dir bool) ([]string, error) {
	var oe *acl.OpenError
	switch err := acl.Check(path); {
	case errors.As(err, &oe):
		return nil, &accessError{open: oe}
	case err != nil:
		return nil, err
	}
	if !dir {
		return nil, nil
	}
	owner, err := ownerSID(path)
	if err != nil {
		return nil, err
	}
	if !acl.OwnerAccepted(owner) {
		return nil, fmt.Errorf("%s is owned by another user (%s): pcb will not use it", path, acl.AccountName(owner))
	}
	return nil, nil
}
