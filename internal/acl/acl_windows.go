//go:build windows

package acl

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// accessAllowedCallbackACEType is ACCESS_ALLOWED_CALLBACK_ACE_TYPE: an
// allow ACE with a condition, laid out as ACCESS_ALLOWED_ACE up to its SID.
const accessAllowedCallbackACEType = 0x9

// everyone is the SID of Everyone, what a null DACL lets in.
const everyone = "S-1-1-0"

// mkdir and ownerOf are Mkdir and Owner; tests replace them to stage a
// directory made meanwhile by another process.
var (
	mkdir   = Mkdir
	ownerOf = Owner
)

var userSID = sync.OnceValues(func() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read this process's user SID: %w", err)
	}
	return u.User.Sid.String(), nil
})

// UserSID returns the SID of the user this process runs as.
func UserSID() (string, error) { return userSID() }

// Elevated reports whether this process runs elevated ("Run as
// administrator", or with UAC off as an administrator).
func Elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// MkdirAll makes path and its missing parents as private directories (see
// the package doc), like os.MkdirAll; directories that exist are left as
// they are.
func MkdirAll(path string) error {
	if fi, err := os.Stat(path); err == nil {
		if fi.IsDir() {
			return nil
		}
		return &fs.PathError{Op: "mkdir", Path: path, Err: syscall.ENOTDIR}
	}
	clean := filepath.Clean(path)
	if parent := filepath.Dir(clean); parent != clean { // not a volume root
		if err := MkdirAll(parent); err != nil {
			return err
		}
	}
	if err := mkdir(clean); err != nil {
		// Made meanwhile by another process: as good if pcb could have made it.
		if fi, serr := os.Lstat(clean); serr == nil && fi.IsDir() {
			return madeMeanwhile(clean)
		}
		return err
	}
	return nil
}

// madeMeanwhile accepts a directory another process made between
// MkdirAll's check and its Mkdir only as pcb makes them: owned by an
// account pcb accepts (OwnerAccepted), its access list letting no other
// account in (Check: an *OpenError otherwise).
func madeMeanwhile(path string) error {
	owner, err := ownerOf(path)
	if err != nil {
		return err
	}
	if !OwnerAccepted(owner) {
		return &fs.PathError{Op: "mkdir", Path: path,
			Err: fmt.Errorf("made meanwhile by %s, an owner pcb does not accept", AccountName(owner))}
	}
	return Check(path)
}

// Mkdir makes path as a private directory (see the package doc); its
// parent must exist. An existing path is an error matching fs.ErrExist.
func Mkdir(path string) error {
	sid, err := UserSID()
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	sd, err := windows.SecurityDescriptorFromString(DirSDDL(sid))
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	sa := windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(sa))
	if err := windows.CreateDirectory(p, &sa); err != nil {
		return &fs.PathError{Op: "mkdir", Path: path, Err: err}
	}
	return nil
}

// Check returns nil when path's DACL lets in only the user, SYSTEM and
// Administrators, else an *OpenError naming every other account with an
// allow entry (an inherit-only one too: it opens what is created inside),
// each once, in the list's order. A null DACL lets everyone in. Failing to
// read the DACL is an *fs.PathError (fs.ErrNotExist for a missing path).
// It is the one access-list check of pcb's private files and directories:
// callers add their own wording around OpenError.Who and Fix.
func Check(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return &fs.PathError{Op: "GetNamedSecurityInfo", Path: path, Err: err}
	}
	user, err := UserSID()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) || (err == nil && dacl == nil) {
		return openError(path, []string{everyone})
	}
	if err != nil {
		return &fs.PathError{Op: "GetSecurityDescriptorDacl", Path: path, Err: err}
	}
	var others []string
	for i := range uint32(dacl.AceCount) {
		var a *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &a); err != nil {
			return &fs.PathError{Op: "GetAce", Path: path, Err: err}
		}
		// Object ACE types are for directory-service objects, not files.
		if a.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE && a.Header.AceType != accessAllowedCallbackACEType {
			continue
		}
		switch sid := (*windows.SID)(unsafe.Pointer(&a.SidStart)).String(); sid {
		case user, SIDSystem, SIDAdministrators:
		default:
			if !slices.Contains(others, sid) {
				others = append(others, sid)
			}
		}
	}
	if len(others) > 0 {
		return openError(path, others)
	}
	return nil
}

// openError is the *OpenError of path for sids, each named (AccountName).
func openError(path string, sids []string) *OpenError {
	e := &OpenError{Path: path}
	for _, sid := range sids {
		e.Grantees = append(e.Grantees, Grantee{SID: sid, Account: AccountName(sid)})
	}
	return e
}

// Owner returns the SID of path's owner.
func Owner(path string) (string, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", &fs.PathError{Op: "GetNamedSecurityInfo", Path: path, Err: err}
	}
	o, _, err := sd.Owner()
	if err != nil {
		return "", &fs.PathError{Op: "GetSecurityDescriptorOwner", Path: path, Err: err}
	}
	if o == nil {
		return "", &fs.PathError{Op: "GetSecurityDescriptorOwner", Path: path, Err: errors.New("no owner")}
	}
	return o.String(), nil
}

// OwnerAccepted reports whether pcb accepts an object owned by sid: the
// user's, or Administrators' when this process runs elevated as one of them
// (what an elevated run makes the owner when it does not set one).
func OwnerAccepted(sid string) bool {
	if user, err := UserSID(); err == nil && sid == user {
		return true
	}
	return sid == SIDAdministrators && Elevated() && administrator()
}

// administrator reports whether this process's token counts as a member of
// Administrators (only when elevated, under UAC).
func administrator() bool {
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false
	}
	// CheckTokenMembership wants an impersonation token; token 0 makes it
	// use the thread's, or a copy of this process's.
	ok, err := windows.Token(0).IsMember(admins)
	return err == nil && ok
}

// AccountName returns sid's account as DOMAIN\name (name alone for an
// account with no domain, such as Everyone), or sid when it has no name.
func AccountName(sid string) string {
	s, err := windows.StringToSid(sid)
	if err != nil {
		return sid
	}
	name, domain, _, err := s.LookupAccount("")
	if err != nil || name == "" {
		return sid
	}
	if domain == "" {
		return name
	}
	return domain + `\` + name
}
