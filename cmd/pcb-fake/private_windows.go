package main

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

// private refuses an account file others can read: Windows has no mode
// bits, so its access list must let in only the user, SYSTEM and
// Administrators (acl.Check). The error and the hint that fixes it.
func private(path string, _ fs.FileInfo) (hint string, err error) {
	err = acl.Check(path)
	var open *acl.OpenError
	switch {
	case errors.As(err, &open):
		return acl.Fix(path, open.SID), fmt.Errorf("account file %s: others can read it (its access list lets %s in)", path, open.Account)
	case err != nil:
		return "make it readable by this user", fmt.Errorf("account file: %w", err)
	}
	return "", nil
}
