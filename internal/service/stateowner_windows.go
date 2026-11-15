//go:build windows

package service

import (
	"errors"
	"io/fs"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

// privateFile is what a hint asks of a file only its user may read.
const privateFile = "no other account in its access list"

// stateDirOwner is acl.Owner; tests replace it.
var stateDirOwner = acl.Owner

// CheckStateOwner refuses the state directory unless pcb could have made it
// (checkStateRoot). euid and userName are Unix's and ignored: on Windows the
// owner is checked whoever runs pcb.
func CheckStateOwner(stateDir string, _ int, _ func(uid int) (string, error)) error {
	return checkStateRoot(stateDir)
}

// checkStateRoot refuses an existing state directory that pcb would not
// have made: owned by an account pcb does not accept (acl.OwnerAccepted:
// this user, or Administrators when this process runs elevated as one), or
// whose access list lets another account in. Files pcb writes there take the
// directory's access list, so an open one (a directory made in Explorer on
// another drive, say) would let other users read them. A missing directory
// passes: pcb makes it private. An owner or access list that cannot be read
// is refused (fails closed).
func checkStateRoot(dir string) error {
	owner, err := stateDirOwner(dir)
	var pe *fs.PathError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case errors.As(err, &pe):
		return mustAct("the owner of the state directory "+dir+" cannot be read: "+pe.Err.Error(), "run pcb doctor")
	case err != nil:
		return mustAct("the owner of the state directory "+dir+" cannot be read", "run pcb doctor")
	case owner == acl.SIDAdministrators && !acl.OwnerAccepted(owner):
		return mustAct("the state directory belongs to "+acl.AccountName(owner)+" (an elevated pcb made it): run pcb elevated, or take ownership of it",
			acl.TakeOwnershipFix(dir))
	case !acl.OwnerAccepted(owner):
		return mustAct("the state directory belongs to "+acl.AccountName(owner)+": run pcb as that user",
			"sign in to Windows as that user, or set PCB_STATE_DIR to a directory of your own")
	}
	var oe *acl.OpenError
	switch err := acl.Check(dir); {
	case errors.As(err, &oe):
		return mustAct("the state directory "+dir+": its access list (ACL) lets "+oe.Who()+" in, so the files pcb writes there would be readable by others",
			"run pcb doctor for the fix: "+acl.Fix(dir, oe.SIDs()...))
	case errors.As(err, &pe):
		return mustAct("the access list of the state directory "+dir+" cannot be read: "+pe.Err.Error(), "run pcb doctor")
	case err != nil:
		return mustAct("the access list of the state directory "+dir+" cannot be read", "run pcb doctor")
	}
	return nil
}
