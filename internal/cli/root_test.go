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
	for _, want := range []string{"login", "login resume", "logout", "status", "calendar", "calendar list", "sync", "event", "event list", "client", "client add", "client list", "client remove"} {
		if !got[want] {
			t.Errorf("command %q is not in the command tree the usage contract walks", want)
		}
	}
}

// ux Minor: a flag value that does not parse says what the flag takes,
// with a hint, never the parser's Go chain (strconv.ParseInt: ...).
func TestBadFlagValueSaysWhatTheFlagTakes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"login", "--password-fd", "x"}, "Error: invalid value \"x\" for --password-fd: it takes a whole number\nHint: run pcb login --help\n"},
		{[]string{"sync", "--watch", "--interval", "3x"}, "Error: invalid value \"3x\" for --interval: it takes a duration such as 30s or 5m\nHint: run pcb sync --help\n"},
	} {
		code, out, errs := run(c.args...)
		if code != exitcode.Usage || out != "" || errs != c.want {
			t.Errorf("pcb %s: exit %d, stdout %q, stderr %q; want 2 and %q", strings.Join(c.args, " "), code, out, errs, c.want)
		}
	}
}

// ux Minor: help offers --json only where it is accepted, and login's
// flags show no sentinel default.
func TestHelpOffersOnlyWhatTheCommandTakes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		args     []string
		withJSON bool
	}{{[]string{"serve", "--help"}, false}, {[]string{"service", "logs", "--help"}, false}, {[]string{"sync", "--help"}, true}} {
		code, out, _ := run(c.args...)
		if code != exitcode.OK || strings.Contains(out, "      --json ") != c.withJSON {
			t.Errorf("pcb %s: exit %d, --json offered = %v, want %v:\n%s", strings.Join(c.args, " "), code, !c.withJSON, c.withJSON, out)
		}
	}
	_, out, _ := run("login", "--help")
	for _, want := range []string{
		"      --password-fd N        read the password from file descriptor N until end of input (without it: $PCB_PASSWORD, else a prompt)\n",
		"      --username NAME        log in as the Proton user NAME (without it: $PCB_USERNAME, else a prompt)\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pcb login --help lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "(default -1)") || strings.Contains(out, "(default 0)") {
		t.Errorf("pcb login --help shows a sentinel default:\n%s", out)
	}
}
