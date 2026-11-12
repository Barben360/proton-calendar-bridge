package install_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
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

	mu    sync.Mutex
	calls []string
}

func newSys(t *testing.T) *sys {
	t.Helper()
	s := &sys{t: t, root: t.TempDir(), euid: 1000,
		env:   map[string]string{"HOME": "/home/alice", "USER": "alice"},
		users: map[string]bool{"root": true}, out: map[string]string{}, codes: map[string]int{}}
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
			// Every user owns what the test process creates.
			return os.Getuid(), os.Getgid(), s.users[name], nil
		},
		Chown: func(string, int, int) error { return nil },
		Env:   func(k string) string { return s.env[k] }}
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
	_, _ = io.WriteString(stdout, s.out[line])
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
	for _, home := range []string{"/home/alice smith", "/home/100%", "/home/a$b"} {
		s := newSys(t)
		s.env["HOME"] = home
		_, err := install.Plan(context.Background(), s.host(), install.Request{Scope: install.ScopeUser})
		if exitcode.From(err) != exitcode.Blocked || !strings.Contains(fmt.Sprint(err), "systemd unit cannot hold") {
			t.Errorf("HOME=%q: err = %v, want a refusal (exit 20)", home, err)
		}
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
	h := s.host()
	steps, err := install.PlanUninstall(context.Background(), h, install.UninstallRequest{Scope: install.ScopeUser, DeleteData: true})
	if err != nil {
		t.Fatal(err)
	}
	err = install.Apply(context.Background(), h, steps, install.Hooks{Logout: func(context.Context) error {
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

// PCB_*_DIR are used verbatim: --delete-data never wipes a directory whose
// name does not say pcb (it could be a home or /srv).
func TestDeleteDataNeverRemovesADirWithoutPcbInItsName(t *testing.T) {
	s := newSys(t)
	unit := install.UserUnit(install.UnitParams{Exec: "/bin/pcb", ConfigDir: "/home/alice", StateDir: "/home/alice/.local/state/pcb"})
	s.write("/home/alice/.config/systemd/user/pcb.service", string(unit), 0o644)
	s.write("/home/alice/notes.txt", "mine", 0o600)
	s.write("/home/alice/.local/state/pcb/pcb.db", "x", 0o600)
	h := s.host()
	steps, err := install.PlanUninstall(context.Background(), h, install.UninstallRequest{Scope: install.ScopeUser, DeleteData: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := install.Apply(context.Background(), h, steps, install.Hooks{Logout: func(context.Context) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.root, "home/alice/notes.txt")); err != nil {
		t.Fatalf("a file of the home was deleted: %v\nplan:\n%s", err, lines(steps))
	}
	if !strings.Contains(lines(steps), "keep dir /home/alice — not removed") {
		t.Fatalf("plan does not say the home is kept:\n%s", lines(steps))
	}
	if _, err := os.Stat(filepath.Join(s.root, "home/alice/.local/state/pcb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state directory was kept: %v", err)
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
	var chowns []string
	h := s.host()
	h.LookupUser = func(name string) (int, int, bool, error) {
		if name == "pcb" {
			return pcbUID, pcbGID, true, nil
		}
		return os.Getuid(), os.Getgid(), true, nil
	}
	h.Chown = func(p string, uid, gid int) error {
		chowns = append(chowns, fmt.Sprintf("%s %d:%d", p, uid, gid))
		return nil
	}
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
	if want := fmt.Sprintf("%s %d:%d", filepath.Join(s.root, "etc/pcb"), pcbUID, pcbGID); !slices.Contains(chowns, want) {
		t.Fatalf("chowns = %q, want %q", chowns, want)
	}
}
