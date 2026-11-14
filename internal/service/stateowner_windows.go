//go:build windows

package service

import (
	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

// stateDirOwner is acl.Owner; tests replace it.
var stateDirOwner = acl.Owner

// CheckStateOwner refuses a state directory owned by an account pcb does
// not accept (acl.OwnerAccepted: this user, or Administrators when this
// process runs elevated as one): pcb would write another user's state, or
// use a directory someone else made where pcb expected its own. euid and
// userName are Unix's and ignored. A directory that does not exist yet, or
// whose owner cannot be read, passes, as on Unix: what pcb then does with it
// reports its own errors.
func CheckStateOwner(stateDir string, _ int, _ func(uid int) (string, error)) error {
	owner, err := stateDirOwner(stateDir)
	if err != nil || acl.OwnerAccepted(owner) {
		return nil //nolint:nilerr // as on Unix, see above
	}
	return mustAct("the state directory belongs to "+acl.AccountName(owner)+": run pcb as that user",
		"sign in to Windows as that user, or set PCB_STATE_DIR to a directory of your own")
}
