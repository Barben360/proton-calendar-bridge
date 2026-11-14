// Package acl makes and checks pcb's private directories.
//
// On Unix a private directory is mode 0700. On Windows it has a protected
// DACL (no inheritance from its parent) granting full control to the
// current user's SID and SYSTEM, nothing else, and the user's SID as owner,
// set explicitly: an elevated process would otherwise make Administrators
// the owner. Files created inside inherit that DACL.
//
// This file holds the OS-neutral parts, tested on every OS; the calls to
// Windows live in acl_windows.go.
package acl

import (
	"github.com/Barben360/proton-calendar-bridge/internal/shellquote"
)

// Well-known SIDs an access list may grant besides the user's.
const (
	SIDSystem         = "S-1-5-18"     // NT AUTHORITY\SYSTEM
	SIDAdministrators = "S-1-5-32-544" // BUILTIN\Administrators
)

// DirSDDL returns the security descriptor of a private directory for the
// user sid: owner sid; protected DACL with full control, inherited by
// subdirectories and files, for sid and SYSTEM.
func DirSDDL(sid string) string {
	return "O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)"
}

// Fix returns the PowerShell command lines that take sid out of path's
// access list: inherited entries become explicit, then sid's grants go.
func Fix(path, sid string) string {
	p := shellquote.PowerShell(path)
	return "icacls " + p + " /inheritance:d; icacls " + p + " /remove:g " + shellquote.PowerShell("*"+sid)
}

// TakeOwnershipFix returns the PowerShell command line that makes the user
// running it the owner of path and of what it holds: the way out of a
// directory an elevated run made, which Administrators owns.
func TakeOwnershipFix(path string) string {
	return "icacls " + shellquote.PowerShell(path) + " /setowner $env:USERNAME /t /c"
}

// OpenError is an access list that lets in an account other than the user,
// SYSTEM or Administrators.
type OpenError struct {
	Path    string
	SID     string // the first such account's SID
	Account string // its name (DOMAIN\name), or the SID when it has none
}

func (e *OpenError) Error() string {
	who := e.SID
	if e.Account != "" && e.Account != e.SID {
		who = e.Account + " (" + e.SID + ")"
	}
	return e.Path + ": its access list (ACL) lets " + who + " in: pcb refuses it; run pcb doctor for the fix"
}
