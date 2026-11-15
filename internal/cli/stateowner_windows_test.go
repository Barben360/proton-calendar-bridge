//go:build windows

package cli_test

import (
	"os"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// The Windows twin of TestRootRefusesAnotherUsersState: a state directory
// owned by another account (SYSTEM here) is refused by every command that
// opens the state (exit 10), whoever runs pcb, and nothing is written there.
func TestAnotherOwnersStateIsRefused(t *testing.T) {
	r := newRig(t, &protontest.Options{})
	if err := acl.MkdirAll(r.dirs.State); err != nil {
		t.Fatal(err)
	}
	acltest.SetOwner(t, r.dirs.State, acl.SIDSystem)
	want := "the state directory belongs to " + acl.AccountName(acl.SIDSystem) + ": run pcb as that user"
	for _, args := range [][]string{{"status"}, {"login", "--non-interactive"}, {"logout"}, {"sync"}} {
		r.want(exitcode.MustAct, append(args, "--json")...)
		if e := r.envelope().Error; e == nil || e.Message != want || !strings.Contains(e.Hint, "PCB_STATE_DIR") {
			t.Errorf("pcb %s: error = %+v", strings.Join(args, " "), e)
		}
	}
	if entries, _ := os.ReadDir(r.dirs.State); len(entries) != 0 {
		t.Errorf("the refused commands wrote %d files in the state directory", len(entries))
	}
}
