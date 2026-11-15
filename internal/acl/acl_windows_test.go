//go:build windows

package acl_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
)

type ace struct {
	typ, flags uint8
	mask       windows.ACCESS_MASK
	sid        string
}

// aces lists path's DACL and reports whether it is protected.
func aces(t *testing.T, path string) (list []ace, protected bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo(%s): %v", path, err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("%s: no DACL (%v)", path, err)
	}
	for i := range uint32(dacl.AceCount) {
		var a *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &a); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&a.SidStart))
		list = append(list, ace{a.Header.AceType, a.Header.AceFlags, a.Mask, sid.String()})
	}
	return list, control&windows.SE_DACL_PROTECTED != 0
}

func userSID(t *testing.T) string {
	t.Helper()
	sid, err := acl.UserSID()
	if err != nil || sid == "" {
		t.Fatalf("UserSID = %q, %v", sid, err)
	}
	return sid
}

// fileAllAccess is SDDL's FA (FILE_ALL_ACCESS).
const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

func TestMkdirAllIsPrivate(t *testing.T) {
	user := userSID(t)
	t.Logf("elevated: %v", acl.Elevated())
	d := t.TempDir()
	b := filepath.Join(d, "a", "b")
	if err := acl.MkdirAll(b); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(d, "a"), b} {
		if err := acl.Check(p); err != nil {
			t.Errorf("Check(%s) = %v, want nil", p, err)
		}
		list, protected := aces(t, p)
		if !protected {
			t.Errorf("%s: DACL not protected (inherits from its parent)", p)
		}
		want := []ace{
			{windows.ACCESS_ALLOWED_ACE_TYPE, windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE, fileAllAccess, user},
			{windows.ACCESS_ALLOWED_ACE_TYPE, windows.OBJECT_INHERIT_ACE | windows.CONTAINER_INHERIT_ACE, fileAllAccess, acl.SIDSystem},
		}
		if len(list) != len(want) {
			t.Fatalf("%s: ACEs %+v, want %+v", p, list, want)
		}
		for i := range want {
			if list[i] != want[i] {
				t.Errorf("%s: ACE %d = %+v, want %+v", p, i, list[i], want[i])
			}
		}
		owner, err := acl.Owner(p)
		if err != nil {
			t.Fatal(err)
		}
		// An elevated token would make Administrators the owner of a
		// directory created without an explicit owner.
		if owner != user {
			t.Errorf("Owner(%s) = %s, want the user %s", p, owner, user)
		}
	}
	// What an os.Mkdir would have given, to show what the owner check
	// stands for on this run (Administrators when elevated, by default).
	plain := filepath.Join(d, "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if o, err := acl.Owner(plain); err == nil {
		t.Logf("owner of an os.Mkdir directory: %s (%s)", o, acl.AccountName(o))
	}
}

func TestFilesInheritThePrivateDACL(t *testing.T) {
	user := userSID(t)
	b := filepath.Join(t.TempDir(), "b")
	if err := acl.MkdirAll(b); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(b, "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := acl.Check(f); err != nil {
		t.Errorf("Check(%s) = %v, want nil", f, err)
	}
	list, _ := aces(t, f)
	want := []ace{
		{windows.ACCESS_ALLOWED_ACE_TYPE, windows.INHERITED_ACE, fileAllAccess, user},
		{windows.ACCESS_ALLOWED_ACE_TYPE, windows.INHERITED_ACE, fileAllAccess, acl.SIDSystem},
	}
	if len(list) != len(want) || list[0] != want[0] || list[1] != want[1] {
		t.Errorf("%s: ACEs %+v, want the directory's, inherited: %+v", f, list, want)
	}
}

func TestCheckRefusesAnEveryoneACE(t *testing.T) {
	for name, inherit := range map[string]uint32{
		"applies here":      windows.NO_INHERITANCE,
		"inherit-only ACEs": windows.INHERIT_ONLY_ACE | windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
	} {
		t.Run(name, func(t *testing.T) {
			b := filepath.Join(t.TempDir(), "b")
			if err := acl.MkdirAll(b); err != nil {
				t.Fatal(err)
			}
			acltest.Allow(t, b, "S-1-1-0", windows.GENERIC_READ, inherit)
			err := acl.Check(b)
			var oe *acl.OpenError
			if !errors.As(err, &oe) {
				t.Fatalf("Check = %v, want *OpenError", err)
			}
			if len(oe.Grantees) != 1 || oe.Grantees[0].SID != "S-1-1-0" || oe.Path != b || oe.Grantees[0].Account == "" {
				t.Errorf("Check = %+v, want SID S-1-1-0 alone, Path %s, an account name", oe, b)
			}
		})
	}
}

// A directory made in Explorer lets several accounts in: Check names every
// one, each once (two entries of one account, here one inherit-only), in the
// list's order, the user, SYSTEM and Administrators left out.
func TestCheckNamesEveryOtherAccount(t *testing.T) {
	b := filepath.Join(t.TempDir(), "b")
	if err := acl.MkdirAll(b); err != nil {
		t.Fatal(err)
	}
	const users = "S-1-5-32-545"
	// Allow puts its entry before the others: the last one added comes first.
	acltest.Allow(t, b, acl.SIDAdministrators, windows.GENERIC_ALL, windows.NO_INHERITANCE)
	acltest.Allow(t, b, users, windows.GENERIC_READ, windows.INHERIT_ONLY_ACE|windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT)
	acltest.Allow(t, b, acltest.Everyone, windows.GENERIC_READ, windows.NO_INHERITANCE)
	acltest.Allow(t, b, users, windows.GENERIC_READ, windows.NO_INHERITANCE)
	list, _ := aces(t, b)
	n := 0
	for _, a := range list {
		if a.sid == users {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("setup: %d entries for Users, want 2: %+v", n, list)
	}
	var oe *acl.OpenError
	if err := acl.Check(b); !errors.As(err, &oe) {
		t.Fatalf("Check = %v, want *OpenError (ACEs %+v)", err, list)
	}
	if got, want := oe.SIDs(), []string{users, acltest.Everyone}; !slices.Equal(got, want) {
		t.Errorf("SIDs = %q, want %q (ACEs %+v)", got, want, list)
	}
	if want := acl.AccountName(users) + " (" + users + ") and " + acl.AccountName(acltest.Everyone) + " (" + acltest.Everyone + ")"; oe.Who() != want {
		t.Errorf("Who = %q, want %q", oe.Who(), want)
	}
}

func TestCheckRefusesANullDACL(t *testing.T) {
	b := filepath.Join(t.TempDir(), "b")
	if err := acl.MkdirAll(b); err != nil {
		t.Fatal(err)
	}
	// A null DACL lets everyone in.
	if err := windows.SetNamedSecurityInfo(b, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var oe *acl.OpenError
	if err := acl.Check(b); !errors.As(err, &oe) || !slices.Equal(oe.SIDs(), []string{"S-1-1-0"}) {
		t.Errorf("Check = %v, want *OpenError for S-1-1-0", err)
	}
}

func TestCheckAcceptsSystemAndAdministrators(t *testing.T) {
	b := filepath.Join(t.TempDir(), "b")
	if err := acl.MkdirAll(b); err != nil {
		t.Fatal(err)
	}
	acltest.Allow(t, b, acl.SIDSystem, windows.GENERIC_READ, windows.NO_INHERITANCE)
	acltest.Allow(t, b, acl.SIDAdministrators, windows.GENERIC_ALL, windows.NO_INHERITANCE)
	list, _ := aces(t, b)
	found := false
	for _, a := range list {
		found = found || a.sid == acl.SIDAdministrators
	}
	if !found {
		t.Fatalf("setup: no Administrators ACE in %+v", list)
	}
	if err := acl.Check(b); err != nil {
		t.Errorf("Check = %v, want nil", err)
	}
}

func TestCheckMissingIsErrNotExist(t *testing.T) {
	p := filepath.Join(t.TempDir(), "missing")
	err := acl.Check(p)
	var oe *acl.OpenError
	if !errors.Is(err, fs.ErrNotExist) || errors.As(err, &oe) {
		t.Errorf("Check(missing) = %v, want fs.ErrNotExist, not an *OpenError", err)
	}
}

func TestOwnerAccepted(t *testing.T) {
	user := userSID(t)
	if !acl.OwnerAccepted(user) {
		t.Error("OwnerAccepted(user) = false")
	}
	if acl.OwnerAccepted("S-1-5-21-1-2-3-1001") {
		t.Error("OwnerAccepted(another user) = true")
	}
	if acl.OwnerAccepted("S-1-1-0") {
		t.Error("OwnerAccepted(Everyone) = true")
	}
	if got, want := acl.OwnerAccepted(acl.SIDAdministrators), acl.Elevated(); got != want {
		t.Errorf("OwnerAccepted(Administrators) = %v, want Elevated() = %v", got, want)
	}
}

func TestAccountName(t *testing.T) {
	// Account names are localised: check their shape, not their words.
	for _, sid := range []string{acl.SIDSystem, acl.SIDAdministrators} {
		if got := acl.AccountName(sid); got == sid || !strings.Contains(got, `\`) || strings.HasPrefix(got, `\`) {
			t.Errorf("AccountName(%s) = %q, want DOMAIN\\name", sid, got)
		}
	}
	if got := acl.AccountName("S-1-1-0"); got == "S-1-1-0" || strings.Contains(got, `\`) {
		t.Errorf("AccountName(Everyone) = %q, want a name with no domain", got)
	}
	for _, sid := range []string{"S-1-5-21-1-2-3-1001", "not a SID"} {
		if got := acl.AccountName(sid); got != sid {
			t.Errorf("AccountName(%s) = %q, want the SID itself", sid, got)
		}
	}
}

// madeBy is a Mkdir that loses the race: another process makes path first
// (here os.Mkdir, so it takes its parent's access list), then Mkdir fails
// as it would, with fs.ErrExist.
func madeBy(t *testing.T) func(string) error {
	return func(path string) error {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return &fs.PathError{Op: "mkdir", Path: path, Err: fs.ErrExist}
	}
}

// A directory made meanwhile is accepted only as pcb would have made it:
// owned by this user, its access list letting no other account in.
func TestMkdirAllChecksADirectoryMadeMeanwhile(t *testing.T) {
	t.Run("private and ours", func(t *testing.T) {
		defer acl.SetMkdir(madeBy(t))()
		if err := acl.MkdirAll(filepath.Join(t.TempDir(), "pcb")); err != nil {
			t.Errorf("MkdirAll = %v, want the directory made meanwhile accepted", err)
		}
	})
	t.Run("open", func(t *testing.T) {
		parent := t.TempDir()
		acltest.OpenToOthers(t, parent) // inherited by what is made inside
		defer acl.SetMkdir(madeBy(t))()
		var oe *acl.OpenError
		if err := acl.MkdirAll(filepath.Join(parent, "pcb")); !errors.As(err, &oe) || !slices.Equal(oe.SIDs(), []string{acltest.Everyone}) {
			t.Errorf("MkdirAll = %v, want an *OpenError for Everyone", err)
		}
	})
	t.Run("foreign owner", func(t *testing.T) {
		defer acl.SetMkdir(madeBy(t))()
		defer acl.SetOwnerOf(func(string) (string, error) { return acl.SIDSystem, nil })()
		err := acl.MkdirAll(filepath.Join(t.TempDir(), "pcb"))
		if err == nil || !strings.Contains(err.Error(), "made meanwhile by "+acl.AccountName(acl.SIDSystem)) {
			t.Errorf("MkdirAll = %v, want the SYSTEM-owned directory refused", err)
		}
	})
}
