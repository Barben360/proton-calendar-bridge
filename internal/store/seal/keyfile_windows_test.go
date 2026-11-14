//go:build windows

package seal_test

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

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
		if !errors.Is(err, seal.ErrKeyMode) || !errors.As(err, &me) || me.Grantee != "S-1-1-0" || me.Path != path {
			t.Fatalf("%s: err = %v, want a KeyModeError for S-1-1-0", name, err)
		}
		if msg := err.Error(); !strings.Contains(msg, "access list") || !strings.Contains(msg, "pcb doctor") || !strings.Contains(msg, path) {
			t.Errorf("%s: message %q lacks the path, the access list or pcb doctor", name, msg)
		}
	}
}
