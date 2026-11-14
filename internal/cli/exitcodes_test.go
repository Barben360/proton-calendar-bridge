package cli_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
)

// copyTree copies the regular files and directories of src to dst, modes
// kept (os.CopyFS widens them, and pcb refuses a loose key file).
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		to := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(to, 0o700); err != nil {
				return err
			}
			return os.Chmod(to, info.Mode().Perm())
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(to, b, 0o600); err != nil {
				return err
			}
			return os.Chmod(to, info.Mode().Perm())
		}
		return nil // a socket or a lock left behind is not state
	})
	if err != nil {
		t.Fatal(err)
	}
}

// ux-I3: a lost, unreadable or corrupt key file, or an unreadable
// state.json, is a fault the person repairs: exit 10 in every command that
// meets it, never 20 (scripts and systemd would retry it forever). pcb
// logout is the way out of a lost or corrupt key and of a damaged
// state.json (exit 0); pcb status reports a key fault (exit 0) and fails
// on a state.json it cannot read. One login for the whole table: each case
// starts from a copy of the logged-in directories.
func TestKeyAndStateFaultsAreExit10InEveryCommand(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.f.AddCalendar(r.f.Username, protontest.Calendar{Name: "Work"})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	acct := accountDir(t, r)
	key := config.Defaults(r.dirs).Secrets.KeyFile
	snap := t.TempDir()
	copyTree(t, filepath.Dir(r.dirs.Config), filepath.Join(snap, "cfg"))
	copyTree(t, filepath.Dir(r.dirs.State), filepath.Join(snap, "state"))
	restore := func(t *testing.T) {
		for _, d := range []struct{ live, saved string }{
			{filepath.Dir(r.dirs.Config), filepath.Join(snap, "cfg")},
			{filepath.Dir(r.dirs.State), filepath.Join(snap, "state")},
		} {
			if err := os.RemoveAll(d.live); err != nil {
				t.Fatal(err)
			}
			copyTree(t, d.saved, d.live)
		}
		// The fixed clock would hold every later login back (one per minute).
		if err := os.Remove(filepath.Join(r.dirs.State, "last-login")); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	type fault struct {
		name  string
		apply func(t *testing.T)
		// hint is the start of the hint every failing command gives.
		hint string
	}
	missing := fault{"key file missing", func(t *testing.T) {
		if err := os.Remove(key); err != nil {
			t.Fatal(err)
		}
	}, "restore"}
	short := fault{"key of a wrong size", func(t *testing.T) {
		if err := os.WriteFile(key, []byte("short"), 0o600); err != nil {
			t.Fatal(err)
		}
	}, ""}
	unreadable := fault{"key file unreadable", func(t *testing.T) {
		if err := os.Chmod(key, 0o000); err != nil {
			t.Fatal(err)
		}
	}, ""}
	badState := fault{"state.json unreadable", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(acct, "state.json"), []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
	}, "run pcb logout to reset it"}
	cases := []struct {
		f    fault
		args []string
		code exitcode.Code
	}{
		{missing, []string{"login", "--non-interactive"}, exitcode.MustAct},
		{short, []string{"login", "--non-interactive"}, exitcode.MustAct},
		{unreadable, []string{"login", "--non-interactive"}, exitcode.MustAct},
		{badState, []string{"login", "--non-interactive"}, exitcode.MustAct},
		{missing, []string{"client", "add", "laptop"}, exitcode.MustAct},
		{short, []string{"client", "add", "laptop"}, exitcode.MustAct},
		{unreadable, []string{"client", "add", "laptop"}, exitcode.MustAct},
		{missing, []string{"status"}, exitcode.OK},
		{short, []string{"status"}, exitcode.OK},
		{unreadable, []string{"status"}, exitcode.OK},
		{badState, []string{"status"}, exitcode.MustAct},
		{missing, []string{"sync"}, exitcode.MustAct},
		{short, []string{"sync"}, exitcode.MustAct},
		{unreadable, []string{"sync"}, exitcode.MustAct},
		{badState, []string{"sync"}, exitcode.MustAct},
		{missing, []string{"serve", "--listen", "127.0.0.1:0"}, exitcode.MustAct},
		{short, []string{"serve", "--listen", "127.0.0.1:0"}, exitcode.MustAct},
		{unreadable, []string{"serve", "--listen", "127.0.0.1:0"}, exitcode.MustAct},
		{badState, []string{"serve", "--listen", "127.0.0.1:0"}, exitcode.MustAct},
		{unreadable, []string{"logout"}, exitcode.MustAct},
		{missing, []string{"logout"}, exitcode.OK},
		{short, []string{"logout"}, exitcode.OK},
		// Last: the logout revokes the fake's session.
		{badState, []string{"logout"}, exitcode.OK},
	}
	parent := t
	for _, c := range cases {
		name := strings.Join(c.args, " ") + ", " + c.f.name
		if !t.Run(name, func(t *testing.T) {
			if c.f.name == unreadable.name && os.Geteuid() == 0 {
				t.Skip("root reads a mode 0000 file")
			}
			r.t = t
			defer func() { r.t = parent }()
			restore(t)
			c.f.apply(t)
			got := r.run(c.args...)
			if got != c.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", got, c.code, r.out.String(), r.errb.String())
			}
			if c.code != exitcode.OK && !strings.Contains(r.errb.String(), "\nHint: "+c.f.hint) {
				t.Errorf("stderr %q, want a hint starting %q", r.errb.String(), c.f.hint)
			}
		}) {
			t.FailNow()
		}
	}
}
