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
		return path + ": its access list lets " + oe.Who() + " in", acl.Fix(path, oe.SIDs()...), true
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
	switch {
	case acl.OwnerAccepted(owner):
	case owner == acl.SIDAdministrators: // a group: what an elevated pcb leaves
		return path + " belongs to " + acl.AccountName(owner) + " (an elevated pcb made it)",
			"run pcb elevated, or take ownership: " + acl.TakeOwnershipFix(path), true
	default:
		return path + " belongs to " + acl.AccountName(owner), "run pcb as that user, or move " + shellquote.Quote(path) + " aside", true
	}
	return "", "", true
}

// unreadableKeyFix is the fix for a key file this Windows user cannot read:
// back to its directory's access list (pcb's private one), or the account
// that owns it.
func unreadableKeyFix(path string) string {
	return "icacls " + shellquote.PowerShell(path) + " /reset (the key file then takes its directory's access list); if another account owns it, run pcb as that account"
}
