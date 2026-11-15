//go:build windows

package service_test

import (
	"os"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/acl"
	"github.com/Barben360/proton-calendar-bridge/internal/acl/acltest"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// A state directory that exists with an access list open to others (made
// in Explorer on another drive, say) is refused before pcb writes anything
// there: its files would take that list (pcb-ui opens the hub directly,
// without the cli's owner check).
func TestOpenHubRefusesAnOpenStateDirectory(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	if err := acl.MkdirAll(e.dirs.State); err != nil {
		t.Fatal(err)
	}
	acltest.OpenToOthers(t, e.dirs.State)
	_, err := service.OpenHub(e.options())
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "access list") || !strings.Contains(ee.Hint, "pcb doctor") {
		t.Errorf("error = %q, hint %q; want the access list and pcb doctor", ee.Error(), ee.Hint)
	}
	if entries, _ := os.ReadDir(e.dirs.State); len(entries) != 0 {
		t.Errorf("the refused OpenHub wrote %d entries in the state directory", len(entries))
	}
}
