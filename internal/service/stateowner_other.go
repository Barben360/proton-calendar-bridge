//go:build !windows

package service

import (
	"strconv"

	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// CheckStateOwner refuses euid 0 on a state directory another user owns (a
// system install's pcb user): every file root writes there would become
// root's, and that user could no longer read it (state Minor). userName
// names a UID (nil or failing: "#UID", which sudo takes too). A directory
// that does not exist yet, or a system without owners, passes.
func CheckStateOwner(stateDir string, euid int, userName func(uid int) (string, error)) error {
	if euid != 0 {
		return nil
	}
	uid, ok := store.DirOwner(stateDir)
	if !ok || uid == 0 {
		return nil
	}
	name, sudoName := "#"+strconv.Itoa(uid), "'#"+strconv.Itoa(uid)+"'"
	if userName != nil {
		if n, err := userName(uid); err == nil && n != "" {
			name, sudoName = n, n
		}
	}
	return mustAct("the state directory belongs to "+name+": run pcb as that user", "sudo -u "+sudoName+" pcb …")
}
