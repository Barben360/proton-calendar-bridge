//go:build windows

package doctor

import (
	"errors"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// ownerSID is acl.Owner; tests replace it.
var ownerSID = acl.Owner

// aclProblem checks the existing path where Unix checks its mode: its
// access list must let in no account but the user, SYSTEM and
// Administrators (acl.Check), and a directory (dir) must be owned by an
// account pcb accepts (acl.OwnerAccepted; a file's owner is not checked, an
// elevated run's files being Administrators'). message is "" when path is
// fine; checked is always true.
func aclProblem(path string, dir bool) (message, fix string, checked bool) {
	var oe *acl.OpenError
	switch err := acl.Check(path); {
	case errors.As(err, &oe):
		who := oe.SID
		if oe.Account != "" && oe.Account != oe.SID {
			who = oe.Account + " (" + oe.SID + ")"
		}
		return path + ": its access list lets " + who + " in", acl.Fix(path, oe.SID), true
	case err != nil:
		return err.Error(), "", true
	}
	if !dir {
		return "", "", true
	}
	owner, err := ownerSID(path)
	if err != nil {
		return err.Error(), "", true
	}
	if !acl.OwnerAccepted(owner) {
		return path + " belongs to " + acl.AccountName(owner), "run pcb as that user, or move " + shellquote.Quote(path) + " aside", true
	}
	return "", "", true
}
