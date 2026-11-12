package install_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
)

// sys is a fake host: a temp Root and a runner that records each command
// and answers from tables.
type sys struct {
	t     *testing.T
	root  string
	euid  int
	env   map[string]string
	users map[string]bool
	out   map[string]string // stdout of a command line
	codes map[string]int    // exit code of a command line (default 0)
	ids   map[string][2]int // uid and gid of a user (default: the test process's)

	mu     sync.Mutex
	calls  []string
	chowns []chownCall
}

// chownCall is one Host.Chown, kept by file identity so a temporary file
// later renamed or linked in place still counts for its final path.
type chownCall struct {
	fi       os.FileInfo
	uid, gid int
}

// ownerSet is the owner the last Chown gave the file at p, or "" if none.
func (s *sys) ownerSet(p string) string {
	s.t.Helper()
	fi, err := os.Stat(filepath.Join(s.root, p))
	if err != nil {
		s.t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	owner := ""
	for _, c := range s.chowns {
		if os.SameFile(c.fi, fi) {
			owner = fmt.Sprintf("%d:%d", c.uid, c.gid)
		}
	}
	return owner
}

func (s *sys) mode(p string) os.FileMode {
	s.t.Helper()
	fi, err := os.Lstat(filepath.Join(s.root, p))
	if err != nil {
		s.t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func newSys(t *testing.T) *sys {
	t.Helper()
	s := &sys{t: t, root: t.TempDir(), euid: 1000,
		env:   map[string]string{"HOME": "/home/alice", "USER": "alice"},
		users: map[string]bool{"root": true}, out: map[string]string{}, codes: map[string]int{}, ids: map[string][2]int{}}
	s.write("/opt/pcb/pcb", "binary v1", 0o755)
	if err := os.MkdirAll(filepath.Join(s.root, "run/systemd/system"), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *sys) host() install.Host {
	return install.Host{Root: s.root, Run: s, Euid: func() int { return s.euid },
		Executable: func() (string, error) { return "/opt/pcb/pcb", nil },
		LookupUser: func(name string) (int, int, bool, error) {
			if id, ok := s.ids[name]; ok {
				return id[0], id[1], s.users[name], nil
			}
			// By default every user owns what the test process creates.
			return os.Getuid(), os.Getgid(), s.users[name], nil
		},
		UserName: func(int) (string, error) { return "alice", nil },
		Chown: func(f *os.File, uid, gid int) error {
			fi, err := f.Stat()
			if err != nil {
				return err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.chowns = append(s.chowns, chownCall{fi, uid, gid})
			return nil
		},
		Env: func(k string) string { return s.env[k] }}
}

// showEnvironment is what systemd answers to show -p Environment: the
// Environment= lines of the user unit on disk.
func (s *sys) showEnvironment() string {
	unit, err := install.UnitPath(s.host(), install.ScopeUser)
	if err != nil {
		return "Environment=\n"
	}
	b, _ := os.ReadFile(filepath.Join(s.root, unit))
	var vals []string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Environment="); ok {
			vals = append(vals, v)
		}
	}
	return "Environment=" + strings.Join(vals, " ") + "\n"
}

func (s *sys) record(name string, args []string) string {
	line := strings.Join(append([]string{name}, args...), " ")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, line)
	if strings.HasPrefix(line, "useradd ") {
		s.users["pcb"] = true
	}
	return line
}

func (s *sys) Output(_ context.Context, name string, args ...string) ([]byte, int, error) {
	line := s.record(name, args)
	return []byte(s.out[line]), s.codes[line], nil
}

func (s *sys) Stream(_ context.Context, stdout, _ io.Writer, name string, args ...string) (int, error) {
	line := s.record(name, args)
	out, ok := s.out[line]
	if !ok && line == "systemctl --user show pcb.service -p Environment" {
		out = s.showEnvironment()
	}
	_, _ = io.WriteString(stdout, out)
	return s.codes[line], nil
}

func (s *sys) write(p, content string, mode os.FileMode) {
	s.t.Helper()
	full := filepath.Join(s.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		s.t.Fatal(err)
	}
}

func lines(steps []install.Step) string {
	var b strings.Builder
	for _, st := range steps {
		fmt.Fprintln(&b, st)
	}
	return b.String()
}

func TestPlanUserOnAFreshHost(t *testing.T) {
	s := newSys(t)
	steps, err := install.Plan(context.Background(), s.host(), install.Request{Scope: install.ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	want := `create dir /home/alice/.config/pcb 0700
create dir /home/alice/.local/state/pcb 0700
create file /home/alice/.config/pcb/config.toml 0600
create dir /home/alice/.config/systemd/user
create file /home/alice/.config/systemd/user/pcb.service 0644
run command systemctl --user daemon-reload
run command systemctl --user enable --now pcb.service
skip command systemctl --user restart pcb.service — the unit was not updated
`
	if got := lines(steps); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
	if len(s.calls) != 0 {
		t.Fatalf("planning ran %q", s.calls)
	}
}

func TestPlanSystemOnAFreshHost(t *testing.T) {
	s := newSys(t)
	s.euid = 0
	steps, err := install.Plan(context.Background(), s.host(), install.Request{Scope: install.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	want := `run user useradd --system --user-group --home-dir /var/lib/pcb --no-create-home --shell /usr/sbin/nologin pcb
create dir /etc/pcb 0700 pcb:pcb
create dir /var/lib/pcb 0700 pcb:pcb
create file /etc/pcb/config.toml 0600 pcb:pcb
create file /usr/local/bin/pcb 0755 root
create file /etc/systemd/system/pcb.service 0644
run command systemctl daemon-reload
run command systemctl enable --now pcb.service
skip command systemctl restart pcb.service — neither the unit nor the binary was updated
`
	if got := lines(steps); got != want {
		t.Fatalf("plan:\n%s\nwant:\n%s", got, want)
	}
}

// systemd splits unit values on spaces and expands % and $: such a path is
// refused before anything is written.
func TestPlanRefusesAPathAUnitCannotHold(t *testing.T) {
	for _, home := range []string{"/home/alice smith", "/home/100%", "/home/a$b", "/home/josé"} {
		s := newSys(t)
		s.env["HOME"] = home
		_, err := install.Plan(context.Background(), s.host(), install.Request{Scope: install.ScopeUser})
		if exitcode.From(err) != exitcode.Blocked || !strings.Contains(fmt.Sprint(err), "only paths made of ASCII letters, digits and /._-+@,:=~") {
			t.Errorf("HOME=%q: err = %v, want a refusal (exit 20)", home, err)
		}
	}
	s := newSys(t)
	s.env["HOME"] = "/home/josé"
	_, err := install.Plan(context.Background(), s.host(), install.Request{Scope: install.ScopeUser})
	if want := `the config directory, /home/josé/.config/pcb, holds 'é' (U+00E9)`; !strings.Contains(fmt.Sprint(err), want) {
		t.Errorf("err = %v, want it to name the character: %q", err, want)
	}
}

func TestApplyCreateNeverReplacesAnExistingFile(t *testing.T) {
	s := newSys(t)
	h := s.host()
	steps, err := install.Plan(context.Background(), h, install.Request{Scope: install.ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	// config.toml appears between the plan and its application.
	s.write("/home/alice/.config/pcb/config.toml", "[log]\nlevel = \"debug\"\n", 0o600)
	err = install.Apply(context.Background(), h, steps, install.Hooks{})
	var se *install.StepError
	if !errors.As(err, &se) || se.Index != 2 {
		t.Fatalf("Apply = %v, want a failure at step 3 (config.toml)", err)
	}
	b, _ := os.ReadFile(filepath.Join(s.root, "home/alice/.config/pcb/config.toml"))
	if string(b) != "[log]\nlevel = \"debug\"\n" {
		t.Fatalf("config.toml = %q: the create replaced it", b)
	}
}

func TestApplyLogoutFailureIsANoteAndGoesOn(t *testing.T) {
	s := newSys(t)
	s.write("/home/alice/.local/state/pcb/pcb.db", "x", 0o600)
	s.write("/home/alice/.config/pcb/config.toml", "", 0o600)
	s.write("/home/alice/.config/systemd/user/pcb.service", string(install.UserUnit(install.UnitParams{
		Exec: "/bin/pcb", ConfigDir: "/home/alice/.config/pcb", StateDir: "/home/alice/.local/state/pcb"})), 0o644)
	h := s.host()
	steps, err := install.PlanUninstall(context.Background(), h, install.UninstallRequest{Scope: install.ScopeUser, DeleteData: true})
	if err != nil {
		t.Fatal(err)
	}
	err = install.Apply(context.Background(), h, steps, install.Hooks{Logout: func(context.Context, paths.Dirs) error {
		return errors.New("proton unreachable")
	}})
	if err != nil {
		t.Fatal(err)
	}
	var note string
	for _, st := range steps {
		if st.Kind == install.KindLogout {
			note = st.Note
		}
	}
	if !strings.Contains(note, "proton unreachable") || !strings.Contains(note, "revoke the session") {
		t.Fatalf("logout note = %q", note)
	}
	if _, err := os.Stat(filepath.Join(s.root, "home/alice/.local/state/pcb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state directory survived a failed logout: %v", err)
	}
}

// M5-R5: --delete-data removes a directory only when it is named exactly
// pcb; any other one is kept and the rm command to delete it printed.
func TestDeleteDataRemovesOnlyDirectoriesNamedPcb(t *testing.T) {
	for _, c := range []struct{ config, state string }{
		{"/home/alice", "/home/alice/.local/state/pcb"},
		{"/home/alice/.config/pcb", "/srv/pcb-state"},
	} {
		s := newSys(t)
		unit := install.UserUnit(install.UnitParams{Exec: "/bin/pcb", ConfigDir: c.config, StateDir: c.state})
		s.write("/home/alice/.config/systemd/user/pcb.service", string(unit), 0o644)
		s.write(c.config+"/notes.txt", "mine", 0o600)
		s.write(c.state+"/pcb.db", "x", 0o600)
		h := s.host()
		steps, err := install.PlanUninstall(context.Background(), h, install.UninstallRequest{Scope: install.ScopeUser, DeleteData: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := install.Apply(context.Background(), h, steps, install.Hooks{Logout: func(context.Context, paths.Dirs) error { return nil }}); err != nil {
			t.Fatal(err)
		}
		for _, d := range []string{c.config, c.state} {
			_, err := os.Stat(filepath.Join(s.root, d))
			if filepath.Base(d) == "pcb" {
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s was kept: %v", d, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s was deleted: %v\nplan:\n%s", d, err, lines(steps))
			}
			if want := "keep dir " + d + " — not removed: only a directory named pcb is deleted; to delete it: rm -r -- " + d + "\n"; !strings.Contains(lines(steps), want) {
				t.Errorf("plan lacks %q:\n%s", want, lines(steps))
			}
		}
	}
}

// The second guard, in Apply itself: a hand-made step removing a directory
// not named pcb, or a relative one, fails and deletes nothing.
func TestApplyRefusesToRemoveADirectoryNotNamedPcb(t *testing.T) {
	s := newSys(t)
	s.write("/home/alice/notes.txt", "mine", 0o600)
	s.write("/srv/pcbuild/board.kicad", "x", 0o600)
	for _, dir := range []string{"/home/alice", "/srv/pcbuild", "pcb"} {
		steps := []install.Step{{Action: install.ActionRemove, Kind: install.KindDir, Path: dir}}
		var se *install.StepError
		if err := install.Apply(context.Background(), s.host(), steps, install.Hooks{}); !errors.As(err, &se) {
			t.Errorf("remove dir %s: err = %v, want a StepError", dir, err)
		}
	}
	for _, p := range []string{"/home/alice/notes.txt", "/srv/pcbuild/board.kicad"} {
		if _, err := os.Stat(filepath.Join(s.root, p)); err != nil {
			t.Errorf("%s was deleted: %v", p, err)
		}
	}
}

// A system directory owned by someone else than pcb is planned as an update
// and handed back to pcb.
func TestPlanSystemFixesAWrongOwner(t *testing.T) {
	s := newSys(t)
	s.euid = 0
	s.users["pcb"] = true
	s.write("/etc/pcb/config.toml", "", 0o600)
	if err := os.Chmod(filepath.Join(s.root, "etc/pcb"), 0o700); err != nil {
		t.Fatal(err)
	}
	pcbUID, pcbGID := os.Getuid()+1, os.Getgid()+1
	s.ids["pcb"] = [2]int{pcbUID, pcbGID}
	h := s.host()
	steps, err := install.Plan(context.Background(), h, install.Request{Scope: install.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	if got := steps[1].String(); got != "update dir /etc/pcb 0700 pcb:pcb" {
		t.Fatalf("step = %q, want an update of /etc/pcb", got)
	}
	if err := install.Apply(context.Background(), h, steps, install.Hooks{}); err != nil {
		t.Fatal(err)
	}
	if got, want := s.ownerSet("/etc/pcb"), fmt.Sprintf("%d:%d", pcbUID, pcbGID); got != want {
		t.Fatalf("owner set = %q, want %q", got, want)
	}
}

// M5-R4: a system plan needs no root. What only root can read is an
// "unknown" step, and applying such a plan fails at it.
func TestPlanSystemWithoutRootMarksWhatItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory")
	}
	s := newSys(t)
	s.users["pcb"] = true
	s.write("/etc/pcb/config.toml", "", 0o600)
	locked := filepath.Join(s.root, "etc/pcb")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	h := s.host()
	steps, err := install.Plan(context.Background(), h, install.Request{Scope: install.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	if want := "unknown file /etc/pcb/config.toml 0600 pcb:pcb — needs root to inspect\n"; !strings.Contains(lines(steps), want) {
		t.Fatalf("plan lacks %q:\n%s", want, lines(steps))
	}
	unSteps, err := install.PlanUninstall(context.Background(), h, install.UninstallRequest{Scope: install.ScopeSystem, DeleteData: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := "remove dir /etc/pcb — config.toml needs root to inspect: a key_file it names elsewhere is not removed\n"; !strings.Contains(lines(unSteps), want) {
		t.Fatalf("uninstall plan lacks %q:\n%s", want, lines(unSteps))
	}
	var unknown []install.Step
	for _, st := range steps {
		if st.Action == install.ActionUnknown {
			unknown = append(unknown, st)
		}
	}
	var se *install.StepError
	if err := install.Apply(context.Background(), h, unknown, install.Hooks{}); len(unknown) != 1 || !errors.As(err, &se) {
		t.Fatalf("Apply(%v) = %v, want a refusal of the unknown step", unknown, err)
	}
}

// M8: an existing system config.toml keeps its content but gets pcb's owner
// and 0600; a symlink in its place is left alone, never followed.
func TestInstallPutsAnExistingSystemConfigRight(t *testing.T) {
	s := newSys(t)
	s.euid = 0
	s.users["pcb"] = true
	s.ids["pcb"] = [2]int{os.Getuid() + 1, os.Getgid() + 1}
	s.write("/etc/pcb/config.toml", "[log]\nlevel = \"debug\"\n", 0o644)
	h := s.host()
	steps, err := install.Plan(context.Background(), h, install.Request{Scope: install.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	if want := "update file /etc/pcb/config.toml 0600 pcb:pcb — content kept: owner and mode put right\n"; !strings.Contains(lines(steps), want) {
		t.Fatalf("plan lacks %q:\n%s", want, lines(steps))
	}
	if err := install.Apply(context.Background(), h, steps, install.Hooks{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "etc/pcb/config.toml")); string(b) != "[log]\nlevel = \"debug\"\n" {
		t.Errorf("content = %q, want it kept", b)
	}
	if got := s.mode("/etc/pcb/config.toml"); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	if got, want := s.ownerSet("/etc/pcb/config.toml"), fmt.Sprintf("%d:%d", os.Getuid()+1, os.Getgid()+1); got != want {
		t.Errorf("owner set = %q, want pcb's %q", got, want)
	}

	// A symlink planted as config.toml.
	s2 := newSys(t)
	s2.euid = 0
	s2.users["pcb"] = true
	s2.write("/etc/shadow", "secret", 0o640)
	s2.write("/etc/pcb/.keep", "", 0o600)
	if err := os.Symlink(filepath.Join(s2.root, "etc/shadow"), filepath.Join(s2.root, "etc/pcb/config.toml")); err != nil {
		t.Fatal(err)
	}
	h2 := s2.host()
	steps, err = install.Plan(context.Background(), h2, install.Request{Scope: install.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	if err := install.Apply(context.Background(), h2, steps, install.Hooks{}); err != nil {
		t.Fatal(err)
	}
	if got := s2.mode("/etc/shadow"); got != 0o640 {
		t.Errorf("the symlink's target got mode %o", got)
	}
	if got := s2.ownerSet("/etc/shadow"); got != "" {
		t.Errorf("the symlink's target was chowned to %s", got)
	}
}

// M10: where the filesystem has no hard links, a create falls back to a
// rename, still never replacing a file that appeared meanwhile.
func TestCreateWorksWithoutHardLinks(t *testing.T) {
	defer install.SetLink(func(string, string) error {
		return &os.LinkError{Op: "link", Err: errors.New("operation not permitted")}
	})()
	s := newSys(t)
	h := s.host()
	steps, err := install.Plan(context.Background(), h, install.Request{Scope: install.ScopeUser})
	if err != nil {
		t.Fatal(err)
	}
	if err := install.Apply(context.Background(), h, steps, install.Hooks{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "home/alice/.config/pcb/config.toml")); string(b) != string(install.DefaultConfig()) {
		t.Fatalf("config.toml = %q", b)
	}

	s = newSys(t)
	h = s.host()
	if steps, err = install.Plan(context.Background(), h, install.Request{Scope: install.ScopeUser}); err != nil {
		t.Fatal(err)
	}
	s.write("/home/alice/.config/pcb/config.toml", "mine", 0o600)
	var se *install.StepError
	if err := install.Apply(context.Background(), h, steps, install.Hooks{}); !errors.As(err, &se) || se.Index != 2 {
		t.Fatalf("Apply = %v, want a failure at config.toml", err)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, "home/alice/.config/pcb/config.toml")); string(b) != "mine" {
		t.Fatalf("config.toml = %q: the fallback replaced it", b)
	}
}

// N2: an existing system config.toml with another hard link is refused at
// plan time (exit 20, named), and if one appears before Apply, Apply
// refuses it too, changing nothing.
func TestInstallRefusesAHardLinkedSystemConfig(t *testing.T) {
	s := newSys(t)
	s.euid = 0
	s.users["pcb"] = true
	s.ids["pcb"] = [2]int{os.Getuid() + 1, os.Getgid() + 1}
	s.write("/etc/pcb/config.toml", "x", 0o644)
	s.write("/etc/elsewhere/.keep", "", 0o600)
	cfg := filepath.Join(s.root, "etc/pcb/config.toml")
	other := filepath.Join(s.root, "etc/elsewhere/file")
	if err := os.Link(cfg, other); err != nil {
		t.Fatal(err)
	}
	h := s.host()
	_, err := install.Plan(context.Background(), h, install.Request{Scope: install.ScopeSystem})
	if exitcode.From(err) != exitcode.Blocked || !strings.Contains(fmt.Sprint(err), "/etc/pcb/config.toml has 2 hard links") {
		t.Fatalf("Plan = %v, want a refusal (exit 20) naming config.toml", err)
	}

	if err := os.Remove(other); err != nil {
		t.Fatal(err)
	}
	steps, err := install.Plan(context.Background(), h, install.Request{Scope: install.ScopeSystem})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(cfg, other); err != nil {
		t.Fatal(err)
	}
	var se *install.StepError
	if err := install.Apply(context.Background(), h, steps, install.Hooks{}); !errors.As(err, &se) || se.Step.Path != "/etc/pcb/config.toml" {
		t.Fatalf("Apply = %v, want a failure at config.toml", err)
	}
	if got := s.mode("/etc/pcb/config.toml"); got != 0o644 {
		t.Errorf("mode = %o: the linked file was changed", got)
	}
	if got := s.ownerSet("/etc/pcb/config.toml"); got != "" {
		t.Errorf("the linked file was chowned to %s", got)
	}
}
