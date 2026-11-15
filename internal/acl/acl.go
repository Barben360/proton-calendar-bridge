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

// Fix returns the PowerShell command lines that take every account of sids
// out of path's access list: inherited entries become explicit, then those
// accounts' grants go, all in one icacls call.
func Fix(path string, sids ...string) string {
	p := shellquote.PowerShell(path)
	fix := "icacls " + p + " /inheritance:d; icacls " + p + " /remove:g"
	for _, sid := range sids {
		fix += " " + shellquote.PowerShell("*"+sid)
	}
	return fix
}

// TakeOwnershipFix returns the PowerShell command line that makes the user
// running it the owner of path and of what it holds: the way out of a
// directory an elevated run made, which Administrators owns.
func TakeOwnershipFix(path string) string {
	return "icacls " + shellquote.PowerShell(path) + " /setowner $env:USERNAME /t /c"
}

// Grantee is an account an access list lets in.
type Grantee struct {
	SID     string
	Account string // its name (DOMAIN\name), or "" or the SID when it has none
}

// String names the account as "name (SID)", or by its SID alone when it has
// no name: the name says who it is, the SID is what icacls takes.
func (g Grantee) String() string {
	if g.Account != "" && g.Account != g.SID {
		return g.Account + " (" + g.SID + ")"
	}
	return g.SID
}

// OpenError is an access list that lets in accounts other than the user,
// SYSTEM or Administrators: every one of them, so one message and one fix
// (Fix(Path, SIDs()...)) settle them all.
type OpenError struct {
	Path     string
	Grantees []Grantee // in the list's order, each account once
}

// SIDs returns the SIDs of the accounts the access list should not let in.
func (e *OpenError) SIDs() []string {
	sids := make([]string, len(e.Grantees))
	for i, g := range e.Grantees {
		sids[i] = g.SID
	}
	return sids
}

// Who names the accounts the access list should not let in, each as
// Grantee.String: "A (S-1-…)", "A (S-1-…) and B (S-1-…)", "A (…), B (…) and C (…)".
func (e *OpenError) Who() string {
	var who string
	for i, g := range e.Grantees {
		switch {
		case i == 0:
		case i == len(e.Grantees)-1:
			who += " and "
		default:
			who += ", "
		}
		who += g.String()
	}
	return who
}

func (e *OpenError) Error() string {
	return e.Path + ": its access list (ACL) lets " + e.Who() + " in: pcb refuses it; run pcb doctor for the fix"
}
