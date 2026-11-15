package acl_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
)

func TestDirSDDL(t *testing.T) {
	got := acl.DirSDDL("S-1-5-21-1-2-3-1001")
	want := "O:S-1-5-21-1-2-3-1001D:P(A;OICI;FA;;;S-1-5-21-1-2-3-1001)(A;OICI;FA;;;SY)"
	if got != want {
		t.Errorf("DirSDDL = %q, want %q", got, want)
	}
}

func TestFix(t *testing.T) {
	got := acl.Fix(`C:\Users\a b\pcb`, "S-1-1-0")
	want := `icacls 'C:\Users\a b\pcb' /inheritance:d; icacls 'C:\Users\a b\pcb' /remove:g '*S-1-1-0'`
	if got != want {
		t.Errorf("Fix = %q, want %q", got, want)
	}
	// Every account in one call: one fix settles a directory made in Explorer.
	got = acl.Fix(`C:\p`, "S-1-5-11", "S-1-5-32-545", "S-1-3-0")
	want = `icacls C:\p /inheritance:d; icacls C:\p /remove:g '*S-1-5-11' '*S-1-5-32-545' '*S-1-3-0'`
	if got != want {
		t.Errorf("Fix (three SIDs) = %q, want %q", got, want)
	}
}

func TestTakeOwnershipFix(t *testing.T) {
	got := acl.TakeOwnershipFix(`C:\Users\Zoë O’Brien\pcb`)
	want := `icacls 'C:\Users\Zoë O’’Brien\pcb' /setowner $env:USERNAME /t /c`
	if got != want {
		t.Errorf("TakeOwnershipFix = %q, want %q", got, want)
	}
}

func TestOpenErrorNamesTheACLAndDoctor(t *testing.T) {
	everyone := acl.Grantee{SID: "S-1-1-0", Account: "Everyone"}
	for _, c := range []struct {
		e    acl.OpenError
		want string
	}{
		{acl.OpenError{Path: `C:\p`, Grantees: []acl.Grantee{everyone}},
			`C:\p: its access list (ACL) lets Everyone (S-1-1-0) in: pcb refuses it; run pcb doctor for the fix`},
		// An account that could not be named is the SID itself: said once.
		{acl.OpenError{Path: `C:\p`, Grantees: []acl.Grantee{{SID: "S-1-5-21-9", Account: "S-1-5-21-9"}}},
			`C:\p: its access list (ACL) lets S-1-5-21-9 in: pcb refuses it; run pcb doctor for the fix`},
		{acl.OpenError{Path: `C:\p`, Grantees: []acl.Grantee{{SID: "S-1-5-21-9"}}},
			`C:\p: its access list (ACL) lets S-1-5-21-9 in: pcb refuses it; run pcb doctor for the fix`},
	} {
		if got := c.e.Error(); got != c.want {
			t.Errorf("%+v.Error() = %q, want %q", c.e, got, c.want)
		}
	}
}

// Who names every account, each with its SID (the name looked up, the SID
// kept for icacls), joined as a sentence; SIDs lists them for Fix.
func TestOpenErrorWhoNamesEveryAccount(t *testing.T) {
	au := acl.Grantee{SID: "S-1-5-11", Account: `NT AUTHORITY\Authenticated Users`}
	users := acl.Grantee{SID: "S-1-5-32-545", Account: `BUILTIN\Users`}
	co := acl.Grantee{SID: "S-1-3-0", Account: "CREATOR OWNER"}
	unnamed := acl.Grantee{SID: "S-1-5-21-9"}
	for _, c := range []struct {
		g    []acl.Grantee
		want string
	}{
		{[]acl.Grantee{au}, `NT AUTHORITY\Authenticated Users (S-1-5-11)`},
		{[]acl.Grantee{au, unnamed}, `NT AUTHORITY\Authenticated Users (S-1-5-11) and S-1-5-21-9`},
		{[]acl.Grantee{au, users, co}, `NT AUTHORITY\Authenticated Users (S-1-5-11), BUILTIN\Users (S-1-5-32-545) and CREATOR OWNER (S-1-3-0)`},
	} {
		e := &acl.OpenError{Path: `C:\p`, Grantees: c.g}
		if got := e.Who(); got != c.want {
			t.Errorf("Who() = %q, want %q", got, c.want)
		}
		if got, want := e.SIDs(), sids(c.g); !slices.Equal(got, want) {
			t.Errorf("SIDs() = %q, want %q", got, want)
		}
	}
}

func sids(g []acl.Grantee) []string {
	var s []string
	for _, x := range g {
		s = append(s, x.SID)
	}
	return s
}

func TestMkdirAllCreatesNestedPrivateDirs(t *testing.T) {
	d := t.TempDir()
	b := filepath.Join(d, "a", "b")
	if err := acl.MkdirAll(b); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, p := range []string{filepath.Join(d, "a"), b} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !fi.IsDir() {
			t.Errorf("%s is not a directory", p)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode %o, want 700", p, fi.Mode().Perm())
		}
	}
	if err := acl.MkdirAll(b); err != nil {
		t.Errorf("MkdirAll of an existing directory: %v, want nil", err)
	}
	f := filepath.Join(d, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := acl.MkdirAll(filepath.Join(f, "c")); err == nil {
		t.Error("MkdirAll under a regular file: nil error, want a failure")
	}
	if err := acl.MkdirAll(f); err == nil {
		t.Error("MkdirAll of a regular file: nil error, want a failure")
	}
}

func TestMkdirExistingIsErrExist(t *testing.T) {
	b := filepath.Join(t.TempDir(), "b")
	if err := acl.Mkdir(b); err != nil {
		t.Fatalf("first Mkdir: %v", err)
	}
	err := acl.Mkdir(b)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second Mkdir: %v, want fs.ErrExist", err)
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) || pe.Op != "mkdir" || pe.Path != b {
		t.Errorf("second Mkdir: %#v, want *fs.PathError{Op: mkdir, Path: %s}", err, b)
	}
}

func TestMkdirMissingParentIsErrNotExist(t *testing.T) {
	err := acl.Mkdir(filepath.Join(t.TempDir(), "no", "b"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Mkdir under a missing parent: %v, want fs.ErrNotExist", err)
	}
}
