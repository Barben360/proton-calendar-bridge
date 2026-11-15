//go:build windows

package seal_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// The Windows twin of TestKeyFileRefusesLooseMode: a key file whose access
// list lets Everyone read it is refused by Check, Open and Seal, with the
// same error kind (ErrKeyMode) and a message naming the access list and
// pcb doctor.
func TestKeyFileWithAnEveryoneACEIsRefused(t *testing.T) {
	path := keyPath(t)
	k := &seal.KeyFile{Path: path}
	sealed, err := k.Seal([]byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Check(); err != nil {
		t.Fatalf("Check of a private key = %v", err)
	}
	acltest.Allow(t, path, "S-1-1-0", windows.GENERIC_READ, windows.NO_INHERITANCE)
	for name, run := range map[string]func() error{
		"check": k.Check,
		"open":  func() error { _, err := k.Open(sealed, nil); return err },
		"seal":  func() error { _, err := k.Seal([]byte("x"), nil); return err },
	} {
		err := run()
		var me *seal.KeyModeError
		if !errors.Is(err, seal.ErrKeyMode) || !errors.As(err, &me) || me.Open == nil || !slices.Equal(me.Open.SIDs(), []string{"S-1-1-0"}) || me.Path != path {
			t.Fatalf("%s: err = %v, want a KeyModeError for S-1-1-0", name, err)
		}
		if msg, want := err.Error(), "key file "+path+": its access list lets "+acl.AccountName("S-1-1-0")+" (S-1-1-0) in: pcb refuses it. Fix: run pcb doctor"; msg != want {
			t.Errorf("%s: message %q, want %q", name, msg, want)
		}
	}
}

// A new key takes its directory's access list: a directory open to others,
// or owned by an account pcb does not accept, is refused before any key is
// written there.
func TestKeyFileRefusesADirectoryNotPrivate(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		path := keyPath(t)
		if err := acl.MkdirAll(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		acltest.OpenToOthers(t, filepath.Dir(path))
		_, err := (&seal.KeyFile{Path: path}).Seal([]byte("x"), nil)
		var me *seal.KeyModeError
		if !errors.As(err, &me) || !me.Dir || me.Open == nil || !slices.Equal(me.Open.SIDs(), []string{acltest.Everyone}) || !strings.Contains(err.Error(), "pcb doctor") {
			t.Fatalf("Seal = %v, want a KeyModeError for the directory and Everyone", err)
		}
		wantNoKey(t, path)
	})
	t.Run("foreign owner", func(t *testing.T) {
		path := keyPath(t)
		defer seal.SetKeyDirOwner(func(string) (string, error) { return acl.SIDSystem, nil })()
		_, err := (&seal.KeyFile{Path: path}).Seal([]byte("x"), nil)
		var me *seal.KeyModeError
		if !errors.As(err, &me) || !me.Dir || me.Owner != acl.AccountName(acl.SIDSystem) || !errors.Is(err, seal.ErrKeyMode) {
			t.Fatalf("Seal = %v, want a KeyModeError naming the directory's owner", err)
		}
		wantNoKey(t, path)
	})
}

func wantNoKey(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a key was written: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".key-*")); len(left) != 0 {
		t.Errorf("temporary key left: %v", left)
	}
}
