package service

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// A path error of the sealed file or the state directory on a state sealed
// by the desktop keyring names neither a key file nor the keyring as the
// culprit, and points at the directory's permissions; a key-file state
// keeps its wording.
func TestStoreErrorPathErrorWording(t *testing.T) {
	t.Parallel()
	pe := &fs.PathError{Op: "open", Path: "/state/accounts/x/secrets.sealed", Err: fs.ErrPermission}
	for _, tc := range []struct {
		name, keyFile, msg, hint string
	}{
		{"keyring", keyringRef,
			"cannot use the stored secrets: open /state/accounts/x/secrets.sealed: permission denied",
			"make sure the state directory and its files are readable and writable by this user (pcb doctor)"},
		{"key file", "/cfg/secrets.key",
			"cannot use the stored secrets or the key file /cfg/secrets.key: open /state/accounts/x/secrets.sealed: permission denied",
			keyFix + "; check the permissions of the state directory"},
	} {
		err := storeError(pe, tc.keyFile)
		if err.Error() != tc.msg || exitcode.HintOf(err) != tc.hint || exitcode.From(err) != exitcode.MustAct {
			t.Errorf("%s: %q, hint %q, exit %d; want %q, hint %q, exit %d", tc.name, err, exitcode.HintOf(err), exitcode.From(err),
				tc.msg, tc.hint, exitcode.MustAct)
		}
	}
}

// The DPAPI key file holds a blob Windows protects, not a 32-byte key: a
// fault of its access list or path gets its own fix, from a client command
// (keyHint) as from the sealed store (storeError).
func TestDPAPIKeyFaultsGetTheirOwnFix(t *testing.T) {
	t.Parallel()
	key := filepath.Join("state", seal.DPAPIKeyName)
	l := &Local{o: Options{Sealer: &seal.DPAPI{Path: key}}}
	open := &seal.KeyModeError{Path: key, Open: &acl.OpenError{Path: key, Grantees: []acl.Grantee{{SID: "S-1-1-0", Account: "Everyone"}}}}
	want := "the DPAPI key file must be readable by the user pcb runs as, no other account in its access list (pcb doctor)"
	for name, err := range map[string]error{
		"access list": open,
		"path":        &fs.PathError{Op: "open", Path: key, Err: fs.ErrPermission},
	} {
		if got := l.keyHint(err); got != want {
			t.Errorf("%s: keyHint = %q, want %q", name, got, want)
		}
		if got := exitcode.HintOf(storeError(err, key)); got != want+"; check the permissions of the state directory" {
			t.Errorf("%s: storeError hint = %q, want %q and the state directory", name, got, want)
		}
	}
	// A key file keeps its own: 32 bytes is what it must hold.
	if got := (&Local{o: Options{Sealer: &seal.KeyFile{Path: "secrets.key"}}}).keyHint(open); !strings.Contains(got, "32 bytes") {
		t.Errorf("key file: keyHint = %q, want the 32 bytes named", got)
	}
}
