//go:build windows

package service

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

func TestCheckStateOwnerAcceptsOurDirectory(t *testing.T) {
	d := filepath.Join(t.TempDir(), "state")
	if err := CheckStateOwner(d, -1, nil); err != nil {
		t.Errorf("missing directory: %v", err)
	}
	if err := acl.MkdirAll(d); err != nil {
		t.Fatal(err)
	}
	if err := CheckStateOwner(d, -1, nil); err != nil {
		t.Errorf("our directory: %v", err)
	}
}

// wantForeignOwner checks err is the refusal of a state directory owned by
// the account named name.
func wantForeignOwner(t *testing.T, err error, name string) {
	t.Helper()
	if exitcode.From(err) != exitcode.MustAct {
		t.Fatalf("err = %v (exit %d), want exit 10", err, exitcode.From(err))
	}
	if msg := err.Error(); !strings.Contains(msg, "belongs to "+name) {
		t.Errorf("message %q does not name the owner %s", msg, name)
	}
	if hint := exitcode.HintOf(err); !strings.Contains(hint, "PCB_STATE_DIR") {
		t.Errorf("hint %q lacks PCB_STATE_DIR", hint)
	}
}

func TestCheckStateOwnerRefusesAnotherOwner(t *testing.T) {
	d := filepath.Join(t.TempDir(), "state")
	if err := acl.MkdirAll(d); err != nil {
		t.Fatal(err)
	}
	acltest.SetOwner(t, d, acl.SIDSystem)
	wantForeignOwner(t, CheckStateOwner(d, -1, nil), acl.AccountName(acl.SIDSystem))
}

// The same refusals without elevation, the owner injected: SYSTEM always,
// and Administrators, which only an elevated administrator's pcb accepts.
func TestCheckStateOwnerRefusesAForeignSID(t *testing.T) {
	d := filepath.Join(t.TempDir(), "state")
	if err := acl.MkdirAll(d); err != nil {
		t.Fatal(err)
	}
	owner := acl.SIDSystem
	old := stateDirOwner
	stateDirOwner = func(string) (string, error) { return owner, nil }
	t.Cleanup(func() { stateDirOwner = old })
	wantForeignOwner(t, CheckStateOwner(d, -1, nil), acl.AccountName(acl.SIDSystem))
	owner = acl.SIDAdministrators
	err := CheckStateOwner(d, -1, nil)
	if acl.Elevated() {
		if err != nil {
			t.Errorf("elevated, an Administrators-owned state: %v", err)
		}
		return
	}
	// A group: the way out is an elevated pcb or taking ownership.
	if exitcode.From(err) != exitcode.MustAct || !strings.Contains(err.Error(), "belongs to "+acl.AccountName(acl.SIDAdministrators)+" (an elevated pcb made it)") ||
		exitcode.HintOf(err) != acl.TakeOwnershipFix(d) {
		t.Errorf("Administrators-owned state, not elevated: %v, hint %q", err, exitcode.HintOf(err))
	}
}

// Files pcb writes take the state directory's access list: an existing one
// open to others is refused (exit 10) with pcb doctor's fix.
func TestCheckStateOwnerRefusesAnOpenRoot(t *testing.T) {
	d := filepath.Join(t.TempDir(), "state")
	if err := acl.MkdirAll(d); err != nil {
		t.Fatal(err)
	}
	acltest.OpenToOthers(t, d)
	err := CheckStateOwner(d, -1, nil)
	if exitcode.From(err) != exitcode.MustAct || !strings.Contains(err.Error(), "access list") || !strings.Contains(err.Error(), acltest.Everyone) || !strings.Contains(err.Error(), d) {
		t.Fatalf("err = %v, want exit 10 naming the directory's access list and Everyone", err)
	}
	if hint := exitcode.HintOf(err); !strings.Contains(hint, "pcb doctor") || !strings.Contains(hint, acl.Fix(d, acltest.Everyone)) {
		t.Errorf("hint %q, want pcb doctor and its fix", hint)
	}
}

// An owner that cannot be read is refused, not passed (fails closed); only a
// missing directory passes.
func TestCheckStateOwnerFailsClosed(t *testing.T) {
	d := filepath.Join(t.TempDir(), "state")
	old := stateDirOwner
	stateDirOwner = func(p string) (string, error) {
		return "", &fs.PathError{Op: "GetNamedSecurityInfo", Path: p, Err: windows.ERROR_ACCESS_DENIED}
	}
	t.Cleanup(func() { stateDirOwner = old })
	err := CheckStateOwner(d, -1, nil)
	if exitcode.From(err) != exitcode.MustAct || !strings.Contains(err.Error(), "cannot be read") || !strings.Contains(err.Error(), d) {
		t.Errorf("err = %v, want exit 10: the owner cannot be read", err)
	}
}

// The Windows hints about a private file name the access list, never a
// Unix mode or chmod/chown.
func TestWindowsHintsNameNoUnixMode(t *testing.T) {
	for _, h := range []string{keyFix, privateFile} {
		for _, unix := range []string{"chmod", "chown", "mode 0"} {
			if strings.Contains(h, unix) {
				t.Errorf("hint %q holds %q", h, unix)
			}
		}
	}
	if !strings.Contains(keyFix, "access list") {
		t.Errorf("keyFix %q does not name the access list", keyFix)
	}
}
