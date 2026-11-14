package service

import (
	"io/fs"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
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
