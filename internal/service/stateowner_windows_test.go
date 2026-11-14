//go:build windows

package service

import (
	"path/filepath"
	"strings"
	"testing"

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
	wantForeignOwner(t, err, acl.AccountName(acl.SIDAdministrators))
}
