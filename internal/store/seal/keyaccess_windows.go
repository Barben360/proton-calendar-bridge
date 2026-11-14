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
		return &KeyModeError{Path: path, Grantee: oe.SID}
	case err != nil:
		return err
	}
	return nil
}
