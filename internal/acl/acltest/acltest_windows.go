//go:build windows

package acltest

import (
	"io/fs"
	"os"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

// WantPrivate fails t unless path is as private as pcb makes it: its
// access list passes acl.Check, and a directory has a protected DACL (no
// inheritance from its parent) and the user as owner, as acl.Mkdir makes
// it. mode is Unix's.
func WantPrivate(t testing.TB, path string, _ fs.FileMode) {
	t.Helper()
	if err := acl.Check(path); err != nil {
		t.Errorf("%s is not private: %v", path, err)
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		return
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("acltest: read the security of %s: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("acltest: read the control of %s: %v", path, err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Errorf("%s inherits its parent's access list: not made by acl.Mkdir", path)
	}
	owner, _, err := sd.Owner()
	if user, uerr := acl.UserSID(); err != nil || uerr != nil || owner.String() != user {
		t.Errorf("%s is owned by %v, want this user (%s)", path, owner, user)
	}
}

// Unreadable makes the file path unreadable by this user until t ends: a
// deny ACE for reading its data, which leaves its attributes and access
// list readable, as a mode 0000 file's stat is on Unix.
func Unreadable(t testing.TB, path string) {
	t.Helper()
	user, err := acl.UserSID()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("acltest: read the DACL of %s: %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("acltest: %s has no DACL: %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("acltest: read the control of %s: %v", path, err)
	}
	protection := windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		protection = windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	Deny(t, path, user, windows.FILE_READ_DATA)
	t.Cleanup(func() { // the path may be gone or replaced by then
		_ = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|protection, nil, nil, dacl, nil)
		_ = sd // keeps dacl's memory alive
	})
}

// OpenToOthers lets Everyone read path, through an allow ACE (inherited by
// what a directory holds).
func OpenToOthers(t testing.TB, path string) {
	t.Helper()
	inherit := uint32(windows.NO_INHERITANCE)
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	Allow(t, path, Everyone, windows.GENERIC_READ, inherit)
}

// OthersFix is the fix pcb doctor gives for OpenToOthers(path).
func OthersFix(path string) string { return acl.Fix(path, Everyone) }

// Allow adds to path's DACL an allow ACE for sid with mask and the
// inheritance flags inherit (windows.NO_INHERITANCE, …), keeping the other
// ACEs and the DACL's protection.
func Allow(t testing.TB, path, sid string, mask windows.ACCESS_MASK, inherit uint32) {
	t.Helper()
	add(t, path, sid, mask, inherit, windows.GRANT_ACCESS)
}

// Deny adds to path's DACL a deny ACE for sid with mask, not inherited,
// keeping the other ACEs: Deny(t, f, user, windows.FILE_READ_DATA) makes f
// unreadable by the user, who can still delete it (the parent's grant) and,
// as its owner, change its DACL back.
func Deny(t testing.TB, path, sid string, mask windows.ACCESS_MASK) {
	t.Helper()
	add(t, path, sid, mask, windows.NO_INHERITANCE, windows.DENY_ACCESS)
}

func add(t testing.TB, path, sid string, mask windows.ACCESS_MASK, inherit uint32, mode windows.ACCESS_MODE) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatalf("acltest: SID %s: %v", sid, err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("acltest: read the DACL of %s: %v", path, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatalf("acltest: %s has no DACL: %v", path, err)
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask,
		AccessMode:        mode,
		Inheritance:       inherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(s),
		},
	}}, dacl)
	if err != nil {
		t.Fatalf("acltest: build the DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatalf("acltest: set the DACL of %s: %v", path, err)
	}
}

// SetOwner makes sid the owner of path. Only an elevated process can (with
// SeRestorePrivilege, which it enables): the test is skipped otherwise.
func SetOwner(t testing.TB, path, sid string) {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("setting another owner needs SeRestorePrivilege (an elevated run)")
	}
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &tok); err != nil {
		t.Fatalf("acltest: open this process's token: %v", err)
	}
	defer tok.Close() //nolint:errcheck // a token handle
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeRestorePrivilege"), &luid); err != nil {
		t.Fatalf("acltest: look up SeRestorePrivilege: %v", err)
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	if err := windows.AdjustTokenPrivileges(tok, false, &tp, 0, nil, nil); err != nil {
		t.Fatalf("acltest: enable SeRestorePrivilege: %v", err)
	}
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatalf("acltest: SID %s: %v", sid, err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION, s, nil, nil, nil); err != nil {
		t.Fatalf("acltest: make %s the owner of %s: %v", sid, path, err)
	}
}
