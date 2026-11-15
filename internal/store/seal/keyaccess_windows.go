//go:build windows

package seal

import (
	"errors"
	"io/fs"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

// checkKeyAccess refuses a key file whose access list lets in an account
// other than the user, SYSTEM or Administrators (acl.Check). Its owner is
// not checked: an elevated run's files are owned by Administrators.
func checkKeyAccess(path string, _ fs.FileInfo) error {
	var oe *acl.OpenError
	switch err := acl.Check(path); {
	case errors.As(err, &oe):
		return &KeyModeError{Path: path, Open: oe}
	case err != nil:
		return err
	}
	return nil
}

// keyDirOwner is acl.Owner; tests replace it.
var keyDirOwner = acl.Owner

// checkKeyDir refuses the directory a new key file is made in when the key
// would not be private there: it takes the directory's access list, which
// must let no other account in, and its owner, who can change that list,
// must be one pcb accepts (acl.OwnerAccepted).
func checkKeyDir(dir string) error {
	owner, err := keyDirOwner(dir)
	if err != nil {
		return err
	}
	if !acl.OwnerAccepted(owner) {
		return &KeyModeError{Path: dir, Dir: true, Owner: acl.AccountName(owner)}
	}
	var oe *acl.OpenError
	switch err := acl.Check(dir); {
	case errors.As(err, &oe):
		return &KeyModeError{Path: dir, Dir: true, Open: oe}
	case err != nil:
		return err
	}
	return nil
}
