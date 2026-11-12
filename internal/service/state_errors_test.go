package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// The messages of a state this pcb cannot read (state-I1, I2): the cause
// named, "upgrade pcb" as the step, exit 10; and the fallback for a store
// that cannot be used no longer sends the person to a logout that keeps it.
func TestStateAndStoreMessages(t *testing.T) {
	t.Parallel()
	const keyFile = "/k/secrets.key"
	for _, tc := range []struct {
		name      string
		err       error
		code      exitcode.Code
		msg, hint string
	}{
		{"newer secrets", storeError(fmt.Errorf("load: %w", &store.UnsupportedError{Newer: true}), keyFile), exitcode.MustAct,
			"secrets.sealed was written by a newer pcb", "upgrade pcb"},
		{"other sealer", storeError(&store.UnsupportedError{Sealer: "tpm2"}, keyFile), exitcode.MustAct,
			`secrets.sealed is sealed with "tpm2", which this pcb cannot use`, "upgrade pcb"},
		{"other sealer, mapped", mapError(&store.UnsupportedError{Sealer: "tpm2"}, keyFile), exitcode.MustAct,
			`secrets.sealed is sealed with "tpm2", which this pcb cannot use`, "upgrade pcb"},
		{"newer state.json", stateError(fmt.Errorf("/s/state.json: %w", store.ErrNewerState)), exitcode.MustAct,
			"state.json was written by a newer pcb", "upgrade pcb"},
		{"store unusable", storeError(errors.New("odd"), keyFile), exitcode.Blocked,
			"cannot use the stored secrets", "check the permissions of the state directory and of the files in it (pcb doctor)"},
	} {
		var ee *exitcode.Error
		if !errors.As(tc.err, &ee) {
			t.Fatalf("%s: %#v is not an *exitcode.Error", tc.name, tc.err)
		}
		if ee.Code != tc.code || ee.Error() != tc.msg || ee.Hint != tc.hint {
			t.Errorf("%s: %d %q hint %q, want %d %q hint %q", tc.name, ee.Code, ee.Error(), ee.Hint, tc.code, tc.msg, tc.hint)
		}
	}
}

// A state.json this user cannot read is intact: the text names the file,
// owner and mode and asks for permissions, never a logout (A12 I1); malformed
// JSON keeps the reset hint; a newer file keeps "upgrade pcb".
func TestStateErrorSplitsUnreadableMalformedAndNewer(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs chmod 000 to be enforced")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	write := func(body string, mode os.FileMode) {
		t.Helper()
		_ = os.Chmod(path, 0o600)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	check := func(name, msgPart, hint string) {
		t.Helper()
		_, err := store.LoadState(dir)
		var ee *exitcode.Error
		if !errors.As(stateError(err), &ee) || ee.Code != exitcode.MustAct {
			t.Fatalf("%s: %v is not exit 10", name, err)
		}
		if !strings.Contains(ee.Error(), msgPart) || ee.Hint != hint {
			t.Errorf("%s: %q hint %q, want %q hint %q", name, ee.Error(), ee.Hint, msgPart, hint)
		}
	}
	write(`{"phase":"ready"}`, 0o000)
	cu := store.CurrentUser()
	check("unreadable", "the login state file "+path+" cannot be read by this user (owner ", "make it readable by "+cu+" (or run pcb as its owner)")
	_, err := store.LoadState(dir)
	if msg := stateError(err).Error(); !strings.Contains(msg, "mode 0000)") {
		t.Errorf("unreadable: %q lacks the mode", msg)
	}
	write(`{not json`, 0o600)
	check("malformed", "the login state file cannot be read", "run pcb logout to reset it")
	write(`{"phase":"ready","field_from_the_future":1}`, 0o600)
	check("newer", "state.json was written by a newer pcb", "upgrade pcb")
}
