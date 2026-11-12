package service

import (
	"errors"
	"fmt"
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
