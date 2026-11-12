package cli_test

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// newerStore logs r in, then makes secrets.sealed one a newer pcb wrote.
func newerStore(t *testing.T, r *rig) string {
	t.Helper()
	r.want(exitcode.OK, "login", "--non-interactive")
	path := filepath.Join(r.dirs.State, "secrets.sealed")
	if err := os.WriteFile(path, []byte(`{"version":3,"sealer":"keyfile","data":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const cannotRevoke = "secrets.sealed was written by a newer pcb, so pcb cannot revoke the Proton session"

// pcb logout over a store this pcb cannot read (state-I1): without a person
// nor --yes it refuses (exit 10), naming why and the way out; it keeps the
// store.
func TestLogoutOfAStoreItCannotReadNeedsConfirmation(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	path := newerStore(t, r)
	r.want(exitcode.MustAct, "logout", "--json")
	e := r.envelope().Error
	if e == nil || e.Message != cannotRevoke ||
		e.Hint != "end the session in the Proton web app (Settings → Security → Sessions), then run pcb logout --yes" {
		t.Errorf("error = %+v", e)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("secrets.sealed after the refusal: %v", err)
	}
	r.want(exitcode.OK, "status", "--json")
	if got := r.out.String(); !strings.Contains(got, `"secrets": "unsupported"`) || !strings.Contains(got, `"fix": "upgrade pcb"`) {
		t.Errorf("status = %s", got)
	}
	r.want(exitcode.OK, "logout", "--yes")
	if got := r.out.String(); !strings.Contains(got, "Logged out locally; Proton could not be told: end the session in the Proton web app (Settings → Security → Sessions).") {
		t.Errorf("stdout = %q", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secrets.sealed after logout --yes: %v", err)
	}
}

// On a terminal, pcb logout says it cannot revoke the session and asks
// before it discards the store; any answer but "discard" keeps it.
func TestLogoutOfAStoreItCannotReadAsksOnATerminal(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	path := newerStore(t, r)
	r.term.interactive = true
	r.term.lines = []string{"no"}
	r.want(exitcode.MustAct, "logout")
	if got := r.errb.String(); !strings.Contains(got, cannotRevoke+": end it in the Proton web app (Settings → Security → Sessions).") ||
		!strings.Contains(got, "Hint: type discard at the prompt, or pass --yes") {
		t.Errorf("stderr = %q", got)
	}
	if want := "Discard the stored session from this machine? Type discard to confirm: "; len(r.term.linePrompts) != 1 || r.term.linePrompts[0] != want {
		t.Errorf("prompts = %q, want %q", r.term.linePrompts, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("secrets.sealed after an answer of no: %v", err)
	}
	r.term.lines = []string{"discard"}
	r.want(exitcode.OK, "logout")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("secrets.sealed after the confirmation: %v", err)
	}
	// A readable store is revoked as ever, without a question.
	r = newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive")
	r.term.interactive = true
	r.want(exitcode.OK, "logout")
	if len(r.term.linePrompts) != 0 || !strings.Contains(r.out.String(), "Logged out.") {
		t.Errorf("logout of a readable store: prompts %q, stdout %q", r.term.linePrompts, r.out.String())
	}
}

// Run as root on a state directory another user owns, a command that opens
// the state refuses (exit 10): the files it writes would become root's and
// that user could no longer read them (state Minor). pcb install, uninstall
// and service, which run as root, are not concerned.
func TestRootRefusesAnotherUsersState(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("the state directory would be root's own")
	}
	r := newRig(t, &protontest.Options{})
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	r.asRoot = true
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	r.userName = func(uid int) (string, error) {
		if strconv.Itoa(uid) != me.Uid {
			return "", errors.New("unknown")
		}
		return "pcb", nil
	}
	for _, args := range [][]string{{"status"}, {"login", "--non-interactive"}, {"logout"}, {"sync"}} {
		r.want(exitcode.MustAct, append(args, "--json")...)
		if e := r.envelope().Error; e == nil || e.Message != "the state directory belongs to pcb: run pcb as that user" || e.Hint != "sudo -u pcb pcb …" {
			t.Errorf("pcb %s: error = %+v", strings.Join(args, " "), e)
		}
	}
	if entries, _ := os.ReadDir(r.dirs.State); len(entries) != 0 {
		t.Errorf("the refused commands wrote %d files in the state directory", len(entries))
	}
	// A user without a name is named by UID, which sudo takes as #UID.
	r.userName = func(int) (string, error) { return "", errors.New("unknown") }
	r.want(exitcode.MustAct, "status", "--json")
	if e := r.envelope().Error; e == nil || e.Hint != "sudo -u '#"+me.Uid+"' pcb …" {
		t.Errorf("unnamed owner: error = %+v", e)
	}
	// Not concerned: a command that does not open the state (pcb install,
	// uninstall and service as root: TestUninstallSystemDeleteData).
	r.want(exitcode.OK, "version")
}
