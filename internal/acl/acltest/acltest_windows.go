//go:build windows

// Package acltest opens a file's access list in tests, so the packages whose
// Windows checks refuse an open ACL share one way to make one.
package acltest

import (
	"testing"

	"golang.org/x/sys/windows"
)

// Allow adds to path's DACL an allow ACE for sid with mask and the
// inheritance flags inherit (windows.NO_INHERITANCE, …), keeping the other
// ACEs and the DACL's protection.
func Allow(t testing.TB, path, sid string, mask windows.ACCESS_MASK, inherit uint32) {
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
		AccessMode:        windows.GRANT_ACCESS,
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
