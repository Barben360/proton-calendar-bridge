package cli_test

import (
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/cli/clitest"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
)

// cobra adds a completion command on its own; pcb has none, so it must fall
// under the usage contract like any unknown command.
func TestCompletionCommandIsUnknown(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"completion", "--json"}, {"completion", "bash", "--json"}} {
		code, out, errs := run(args...)
		if code != exitcode.Usage || errs != "" {
			t.Fatalf("pcb %v: code=%d stdout=%q stderr=%q", args, code, out, errs)
		}
		if env := decodeOne(t, out); env.OK || env.Error.Code != 2 {
			t.Errorf("pcb %v: envelope = %+v", args, env)
		}
	}
}

func TestUsageContractListsEveryNewCommand(t *testing.T) {
	t.Parallel()
	got := map[string]bool{}
	for _, p := range clitest.Paths(cli.NewRoot()) {
		got[strings.Join(p, " ")] = true
	}
	for _, want := range []string{"login", "login resume", "logout", "status", "calendar", "calendar list", "sync", "event", "event list"} {
		if !got[want] {
			t.Errorf("command %q is not in the command tree the usage contract walks", want)
		}
	}
}
