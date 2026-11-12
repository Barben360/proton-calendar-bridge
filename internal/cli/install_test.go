package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// noSystemHost is the host of the tests that are not about the system: a
// Root that does not exist (no systemd, no unit) and a runner that runs
// nothing.
func noSystemHost() install.Host {
	return install.Host{Root: filepath.Join(os.TempDir(), "pcb-tests-no-such-root"), Run: refusingRunner{},
		Euid:       func() int { return 1000 },
		Executable: func() (string, error) { return "", errors.New("no binary in tests") },
		LookupUser: func(string) (int, int, bool, error) { return 0, 0, false, nil },
		UserName:   func(int) (string, error) { return "", errors.New("no users in tests") },
		Chown:      func(string, int, int) error { return errors.New("no chown in tests") }}
}

type refusingRunner struct{}

func (refusingRunner) Output(context.Context, string, ...string) ([]byte, int, error) {
	return nil, -1, errors.New("tests run no system command")
}

func (refusingRunner) Stream(context.Context, io.Writer, io.Writer, string, ...string) (int, error) {
	return -1, errors.New("tests run no system command")
}

// sysRig runs pcb install, uninstall and service on a fake host: a temp Root
// with systemd "running", a binary at /opt/pcb/pcb and a runner that records
// every command line (and the logout hook) in events.
type sysRig struct {
	t    *testing.T
	root string
	euid int
	env  map[string]string
	term *fakeTerm

	mu     sync.Mutex
	users  map[string]bool
	out    map[string]string // stdout of a command line
	stderr map[string]string // stderr of a command line
	codes  map[string]int    // exit code of a command line (default 0)
	events []string

	// logout is what the logout hook's service answers.
	logout func() (service.LogoutResult, error)
	// logoutEnv is the directories the hook's service was opened on.
	logoutEnv [2]string
	// ids are the uid and gid LookupUser gives a user (default: the test
	// process's, so a rerun finds its own files owned right).
	ids map[string][2]int
	// chowns are the Chown calls, by file identity: a temporary file
	// linked or renamed in place counts for its final path.
	chowns []chownCall

	stdout, errb bytes.Buffer
}

func newSysRig(t *testing.T) *sysRig {
	t.Helper()
	s := &sysRig{t: t, root: t.TempDir(), euid: 1000, term: &fakeTerm{},
		// $USER is not the effective user (su without -): the linger
		// hint must name alice, the euid's user (M7).
		env:   map[string]string{"HOME": "/home/alice", "USER": "mallory"},
		users: map[string]bool{"root": true}, out: map[string]string{}, stderr: map[string]string{}, codes: map[string]int{}, ids: map[string][2]int{},
		logout: func() (service.LogoutResult, error) {
			return service.LogoutResult{WasLoggedIn: true, Revoked: true}, nil
		}}
	s.out["loginctl show-user alice -p Linger"] = "Linger=no\n"
	s.write("/opt/pcb/pcb", "binary v1", 0o755)
	s.mkdir("/run/systemd/system")
	return s
}

type chownCall struct {
	fi       os.FileInfo
	uid, gid int
}

// ownerSet is the owner the last Chown gave the file at p ("" if none).
func (s *sysRig) ownerSet(p string) string {
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

func (s *sysRig) host() install.Host {
	return install.Host{Root: s.root, Run: s, Euid: func() int { return s.euid },
		Executable: func() (string, error) { return "/opt/pcb/pcb", nil },
		LookupUser: func(name string) (int, int, bool, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if id, ok := s.ids[name]; ok {
				return id[0], id[1], s.users[name], nil
			}
			return os.Getuid(), os.Getgid(), s.users[name], nil
		},
		UserName: func(uid int) (string, error) {
			if uid == 1000 {
				return "alice", nil
			}
			return "", errors.New("unknown uid")
		},
		Chown: func(p string, uid, gid int) error {
			fi, err := os.Lstat(p)
			if err != nil {
				return err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			s.chowns = append(s.chowns, chownCall{fi, uid, gid})
			return nil
		}}
}

// showEnvironment is what systemd answers to show -p Environment: the
// Environment= lines of the user unit on disk.
func (s *sysRig) showEnvironment() string {
	b, _ := os.ReadFile(filepath.Join(s.root, userUnit))
	var vals []string
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "Environment="); ok {
			vals = append(vals, v)
		}
	}
	return "Environment=" + strings.Join(vals, " ") + "\n"
}

func (s *sysRig) record(name string, args []string) string {
	line := strings.Join(append([]string{name}, args...), " ")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, line)
	switch line {
	case "useradd --system --user-group --home-dir /var/lib/pcb --no-create-home --shell /usr/sbin/nologin pcb":
		s.users["pcb"] = true
	case "userdel pcb":
		delete(s.users, "pcb")
	}
	return line
}

func (s *sysRig) Output(_ context.Context, name string, args ...string) ([]byte, int, error) {
	line := s.record(name, args)
	return []byte(s.out[line]), s.codes[line], nil
}

func (s *sysRig) Stream(_ context.Context, stdout, stderr io.Writer, name string, args ...string) (int, error) {
	line := s.record(name, args)
	out, ok := s.out[line]
	if !ok && line == "systemctl --user show pcb.service -p Environment" {
		out = s.showEnvironment()
	}
	_, _ = io.WriteString(stdout, out)
	_, _ = io.WriteString(stderr, s.stderr[line])
	return s.codes[line], nil
}

// logoutAPI is the service the logout hook opens.
type logoutAPI struct {
	stubAPI
	s *sysRig
}

func (a *logoutAPI) Logout(context.Context) (service.LogoutResult, error) {
	_, err := os.Stat(a.s.logoutEnv[1])
	a.s.mu.Lock()
	a.s.events = append(a.s.events, "logout (state present: "+map[bool]string{true: "yes", false: "no"}[err == nil]+")")
	a.s.mu.Unlock()
	return a.s.logout()
}

func (s *sysRig) run(args ...string) exitcode.Code {
	s.t.Helper()
	s.stdout.Reset()
	s.errb.Reset()
	io := cli.IO{Stdout: &s.stdout, Stderr: &s.errb, Term: s.term, Env: func(k string) string { return s.env[k] }, Host: s.host(),
		NewService: func(_ context.Context, env func(string) string) (service.API, error) {
			s.logoutEnv = [2]string{env("PCB_CONFIG_DIR"), env("PCB_STATE_DIR")}
			return &logoutAPI{s: s}, nil
		}}
	return cli.Main(s.t.Context(), args, io)
}

func (s *sysRig) want(code exitcode.Code, args ...string) {
	s.t.Helper()
	if got := s.run(args...); got != code {
		s.t.Fatalf("pcb %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), got, code, s.stdout.String(), s.errb.String())
	}
}

// failure decodes the failure envelope of the last --json run.
func (s *sysRig) failure() (code int, message, hint string) {
	s.t.Helper()
	var env envelope
	if err := json.Unmarshal(s.stdout.Bytes(), &env); err != nil || env.Error == nil {
		s.t.Fatalf("no failure envelope (%v): %s", err, s.stdout.String())
	}
	return env.Error.Code, env.Error.Message, env.Error.Hint
}

type planData struct {
	Scope string         `json:"scope"`
	Plan  bool           `json:"plan"`
	Steps []install.Step `json:"steps"`
	Next  []string       `json:"next"`
}

func (s *sysRig) data() planData {
	s.t.Helper()
	var env struct {
		OK   bool     `json:"ok"`
		Data planData `json:"data"`
	}
	if err := json.Unmarshal(s.stdout.Bytes(), &env); err != nil || !env.OK {
		s.t.Fatalf("no success envelope (%v): %s", err, s.stdout.String())
	}
	return env.Data
}

func (s *sysRig) write(p, content string, mode os.FileMode) {
	s.t.Helper()
	full := filepath.Join(s.root, p)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sysRig) mkdir(p string) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Join(s.root, p), 0o755); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sysRig) read(p string) string {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.root, p))
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

func (s *sysRig) exists(p string) bool {
	_, err := os.Lstat(filepath.Join(s.root, p))
	return err == nil
}

func (s *sysRig) mode(p string) fs.FileMode {
	s.t.Helper()
	fi, err := os.Stat(filepath.Join(s.root, p))
	if err != nil {
		s.t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// tree lists every path under Root.
func (s *sysRig) tree() []string {
	s.t.Helper()
	var out []string
	err := filepath.WalkDir(s.root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(s.root, p)
		out = append(out, "/"+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		s.t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// takeEvents returns the events since the last call.
func (s *sysRig) takeEvents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.events
	s.events = nil
	return e
}

const (
	showEnv       = "systemctl --user show pcb.service -p Environment"
	showState     = "systemctl --user show pcb.service --property=ActiveState,SubState,UnitFileState,MainPID"
	userConfigDir = "/home/alice/.config/pcb"
	userStateDir  = "/home/alice/.local/state/pcb"
	userUnit      = "/home/alice/.config/systemd/user/pcb.service"
)

func userUnitContent() string {
	return string(install.UserUnit(install.UnitParams{Exec: "/opt/pcb/pcb", ConfigDir: userConfigDir, StateDir: userStateDir}))
}

func TestInstallUserPlanChangesNothing(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	before := s.tree()
	s.want(exitcode.OK, "install", "--plan")
	for _, want := range []string{
		"create dir /home/alice/.config/pcb 0700\n",
		"create file /home/alice/.config/systemd/user/pcb.service 0644\n",
		"run command systemctl --user enable --now pcb.service\n",
		"Plan only: nothing was changed.\n",
	} {
		if !strings.Contains(s.stdout.String(), want) {
			t.Errorf("plan lacks %q:\n%s", want, s.stdout.String())
		}
	}
	s.want(exitcode.OK, "install", "--user", "--plan", "--json")
	d := s.data()
	if d.Scope != "user" || !d.Plan || len(d.Steps) != 8 || !slices.Contains(d.Next, "loginctl enable-linger alice") {
		t.Errorf("plan data = %+v", d)
	}
	if after := s.tree(); !slices.Equal(before, after) {
		t.Errorf("--plan changed the tree:\nbefore %q\nafter  %q", before, after)
	}
	for _, e := range s.takeEvents() {
		if !strings.HasPrefix(e, "loginctl show-user ") && !strings.HasPrefix(e, "systemctl --user show ") {
			t.Errorf("--plan ran %q, which is not a read-only probe", e)
		}
	}
}

func TestInstallUserCreatesThenKeeps(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	if got := s.read(userUnit); got != userUnitContent() {
		t.Errorf("unit =\n%s\nwant\n%s", got, userUnitContent())
	}
	if got := s.read(userConfigDir + "/config.toml"); got != string(install.DefaultConfig()) {
		t.Errorf("config.toml = %q", got)
	}
	for p, want := range map[string]fs.FileMode{userConfigDir: 0o700, userStateDir: 0o700, userConfigDir + "/config.toml": 0o600, userUnit: 0o644} {
		if got := s.mode(p); got != want {
			t.Errorf("mode of %s = %o, want %o", p, got, want)
		}
	}
	if got, want := s.takeEvents(), []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now pcb.service",
		"loginctl show-user alice -p Linger",
	}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
	for _, want := range []string{"\nNext:\n", "  loginctl enable-linger alice\n", "  pcb login\n", "  pcb client add NAME\n"} {
		if !strings.Contains(s.stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, s.stdout.String())
		}
	}

	// Review Focus 4: a second run keeps every file and reloads nothing.
	s.out["loginctl show-user alice -p Linger"] = "Linger=yes\n"
	s.want(exitcode.OK, "install", "--json")
	d := s.data()
	for _, st := range d.Steps {
		if (st.Kind == "dir" || st.Kind == "file") && st.Action != "keep" {
			t.Errorf("second run: %s, want keep", st)
		}
	}
	if got, want := s.takeEvents(), []string{"systemctl --user enable --now pcb.service", showState, "loginctl show-user alice -p Linger"}; !slices.Equal(got, want) {
		t.Errorf("second run commands = %q, want %q", got, want)
	}
	if want := []string{"pcb login", "pcb client add NAME"}; !slices.Equal(d.Next, want) {
		t.Errorf("with linger on, next = %q, want %q", d.Next, want)
	}
}

func TestInstallKeepsAnEditedConfig(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	const edited = "[log]\nlevel = \"debug\"\n"
	s.write(userConfigDir+"/config.toml", edited, 0o600)
	s.want(exitcode.OK, "install", "--json")
	if got := s.read(userConfigDir + "/config.toml"); got != edited {
		t.Fatalf("config.toml = %q, want the edited one", got)
	}
	for _, st := range s.data().Steps {
		if st.Path == userConfigDir+"/config.toml" && st.Action != "keep" {
			t.Fatalf("config step = %s, want keep", st)
		}
	}
}

func TestInstallUpdatesAChangedUnitAndRestarts(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	s.write(userUnit, userUnitContent()+"Nice=5\n", 0o644)
	s.takeEvents()
	s.want(exitcode.OK, "install")
	if got := s.read(userUnit); got != userUnitContent() {
		t.Errorf("unit not rewritten:\n%s", got)
	}
	if !strings.Contains(s.stdout.String(), "update file "+userUnit+" 0644\n") {
		t.Errorf("output lacks the unit update:\n%s", s.stdout.String())
	}
	if got, want := s.takeEvents(), []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now pcb.service",
		"systemctl --user restart pcb.service",
		"loginctl show-user alice -p Linger",
	}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

// M5-R4: applying a system install or uninstall needs root (exit 10, before
// anything runs); planning one does not: the plan ends with the note.
func TestInstallSystemNeedsRoot(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.write(install.SystemUnitPath, "[Unit]\n", 0o644)
	before := s.tree()
	for _, args := range [][]string{{"install", "--system"}, {"uninstall", "--system"}, {"uninstall", "--system", "--delete-data", "--yes"}} {
		s.want(exitcode.MustAct, append(args, "--json")...)
		cmd := args[0]
		if _, _, hint := s.failure(); hint != "sudo pcb "+cmd+" --system" {
			t.Errorf("%v: hint = %q", args, hint)
		}
		if e := s.takeEvents(); len(e) != 0 {
			t.Errorf("%v: the refusal ran %q", args, e)
		}
	}
	for _, cmd := range []string{"install", "uninstall"} {
		s.want(exitcode.OK, cmd, "--system", "--plan")
		if want := "Note: applying it needs root (sudo pcb " + cmd + " --system)\nPlan only: nothing was changed.\n"; !strings.HasSuffix(s.stdout.String(), want) {
			t.Errorf("pcb %s --system --plan does not end with %q:\n%s", cmd, want, s.stdout.String())
		}
		s.want(exitcode.OK, cmd, "--system", "--plan", "--json")
		if d := s.data(); !d.Plan || !slices.Contains(d.Next, "applying it needs root (sudo pcb "+cmd+" --system)") {
			t.Errorf("pcb %s --system --plan --json: %+v", cmd, d)
		}
	}
	if after := s.tree(); !slices.Equal(before, after) {
		t.Errorf("the tree changed: %q", after)
	}
	if e := s.takeEvents(); len(e) != 0 {
		t.Errorf("the system plans ran %q", e)
	}
}

func TestInstallSystemCreatesTheUserOnce(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.euid = 0
	const useradd = "useradd --system --user-group --home-dir /var/lib/pcb --no-create-home --shell /usr/sbin/nologin pcb"
	s.want(exitcode.OK, "install", "--system")
	if got, want := s.takeEvents(), []string{useradd, "systemctl daemon-reload", "systemctl enable --now pcb.service"}; !slices.Equal(got, want) {
		t.Errorf("first run commands = %q, want %q", got, want)
	}
	for _, p := range []string{"/etc/pcb", "/var/lib/pcb"} {
		if got := s.mode(p); got != 0o700 {
			t.Errorf("mode of %s = %o", p, got)
		}
	}
	if !strings.Contains(s.stdout.String(), "  sudo -u pcb env PCB_CONFIG_DIR=/etc/pcb PCB_STATE_DIR=/var/lib/pcb /usr/local/bin/pcb login\n") {
		t.Errorf("output lacks the login line:\n%s", s.stdout.String())
	}
	s.want(exitcode.OK, "install", "--system", "--json")
	if got, want := s.takeEvents(), []string{"systemctl enable --now pcb.service"}; !slices.Equal(got, want) {
		t.Errorf("second run commands = %q, want %q", got, want)
	}
	for _, st := range s.data().Steps {
		if st.Kind == "user" && st.Action != "skip" {
			t.Errorf("second run user step = %s, want skip", st)
		}
		if (st.Kind == "dir" || st.Kind == "file") && st.Action != "keep" {
			t.Errorf("second run: %s, want keep", st)
		}
	}
}

func TestInstallSystemCopiesTheBinary(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.euid = 0
	s.want(exitcode.OK, "install", "--system")
	if got := s.read("/usr/local/bin/pcb"); got != "binary v1" {
		t.Fatalf("/usr/local/bin/pcb = %q", got)
	}
	if got := s.mode("/usr/local/bin/pcb"); got != 0o755 {
		t.Errorf("mode = %o, want 755", got)
	}
	unit := s.read(install.SystemUnitPath)
	if !strings.Contains(unit, "\nExecStart=/usr/local/bin/pcb serve --wait-for-login\n") || !strings.Contains(unit, "\nUser=pcb\n") {
		t.Errorf("unit does not run the copy as pcb:\n%s", unit)
	}
	// A new build: the copy is replaced and the service restarted on it.
	s.write("/opt/pcb/pcb", "binary v2", 0o755)
	s.takeEvents()
	s.want(exitcode.OK, "install", "--system")
	if got := s.read("/usr/local/bin/pcb"); got != "binary v2" {
		t.Fatalf("/usr/local/bin/pcb = %q after a new build", got)
	}
	if !strings.Contains(s.stdout.String(), "update file /usr/local/bin/pcb 0755 root\n") {
		t.Errorf("output lacks the binary update:\n%s", s.stdout.String())
	}
	if got, want := s.takeEvents(), []string{"systemctl enable --now pcb.service", "systemctl restart pcb.service"}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
}

func TestInstallUserAsRootIsRefused(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.euid = 0
	before := s.tree()
	for _, args := range [][]string{{"install"}, {"install", "--user"}, {"install", "--user", "--plan"}} {
		s.want(exitcode.Usage, append(args, "--json")...)
		if _, _, hint := s.failure(); hint != "use --system" {
			t.Errorf("%v: hint = %q", args, hint)
		}
	}
	s.want(exitcode.Usage, "install", "--user", "--system")
	if after := s.tree(); !slices.Equal(before, after) {
		t.Errorf("the refusal changed the tree: %q", after)
	}
}

func TestInstallWithoutSystemdIsBlocked(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	if err := os.RemoveAll(filepath.Join(s.root, "run")); err != nil {
		t.Fatal(err)
	}
	before := s.tree()
	s.want(exitcode.OK, "install", "--plan")
	if !strings.Contains(s.stdout.String(), "create file "+userUnit) || !strings.Contains(s.stdout.String(), "systemd is not running here") {
		t.Errorf("plan lacks its steps or the note:\n%s", s.stdout.String())
	}
	s.want(exitcode.Blocked, "install", "--json")
	if _, msg, hint := s.failure(); msg != "systemd is not running here" || hint != "in a container, run pcb serve as the entrypoint (README)" {
		t.Errorf("failure = %q, hint %q", msg, hint)
	}
	if after := s.tree(); !slices.Equal(before, after) {
		t.Errorf("the refusal changed the tree: %q", after)
	}
	for _, e := range s.takeEvents() {
		if strings.HasPrefix(e, "systemctl") {
			t.Errorf("ran %q without systemd", e)
		}
	}
}

func TestInstallFailureMidwayIsPartial(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	const enable = "systemctl --user enable --now pcb.service"
	s.codes[enable] = 1
	s.stderr[enable] = "Created symlink.\nFailed to start pcb.service: Unit pcb.service has a bad setting.\n"
	s.want(exitcode.Partial, "install", "--json")
	code, msg, hint := s.failure()
	if code != 40 || !strings.Contains(msg, enable) || !strings.Contains(msg, "Unit pcb.service has a bad setting.") {
		t.Errorf("failure = %d %q", code, msg)
	}
	if hint != "fix it, then run the command again: finished steps are kept" {
		t.Errorf("hint = %q", hint)
	}
	if got := s.read(userUnit); got != userUnitContent() {
		t.Errorf("the unit written before the failure is gone or wrong: %q", got)
	}
	// Fixed: the rerun keeps the finished steps and redoes the failed one.
	delete(s.codes, enable)
	s.takeEvents()
	s.want(exitcode.OK, "install")
	if got := s.takeEvents(); !slices.Contains(got, enable) {
		t.Errorf("rerun commands = %q, want %q", got, enable)
	}
}

func TestUninstallKeepsDataByDefault(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	s.takeEvents()
	s.want(exitcode.OK, "uninstall")
	if got, want := s.takeEvents(), []string{showEnv, "systemctl --user disable --now pcb.service", "systemctl --user daemon-reload"}; !slices.Equal(got, want) {
		t.Errorf("commands = %q, want %q", got, want)
	}
	if s.exists(userUnit) {
		t.Error("the unit is still there")
	}
	for _, d := range []string{userConfigDir, userStateDir} {
		if !s.exists(d) {
			t.Errorf("%s was deleted without --delete-data", d)
		}
		if want := "keep dir " + d + " — kept: pcb uninstall --delete-data removes it\n"; !strings.Contains(s.stdout.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, s.stdout.String())
		}
	}
	if s.logoutEnv != [2]string{} {
		t.Errorf("logged out without --delete-data")
	}
}

func TestUninstallDeleteDataNeedsConfirmation(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	s.takeEvents()
	untouched := func(what string) {
		t.Helper()
		if !s.exists(userUnit) || !s.exists(userStateDir) {
			t.Errorf("%s: something was removed", what)
		}
		if e := s.takeEvents(); slices.Contains(e, "systemctl --user disable --now pcb.service") {
			t.Errorf("%s: ran %q", what, e)
		}
	}

	s.want(exitcode.MustAct, "uninstall", "--delete-data", "--json")
	if _, msg, _ := s.failure(); !strings.Contains(msg, "--yes") {
		t.Errorf("non-interactive refusal does not name --yes: %q", msg)
	}
	untouched("non-interactive")

	s.term.interactive = true
	s.want(exitcode.MustAct, "uninstall", "--delete-data", "--non-interactive")
	untouched("--non-interactive")

	s.term.lines = []string{"yes"}
	s.want(exitcode.MustAct, "uninstall", "--delete-data")
	if want := "Delete /home/alice/.config/pcb and /home/alice/.local/state/pcb (Proton session, cache, CalDAV clients)? Type delete to confirm: "; !slices.Equal(s.term.linePrompts, []string{want}) {
		t.Errorf("prompts = %q, want %q", s.term.linePrompts, want)
	}
	untouched("answered yes")

	s.want(exitcode.OK, "uninstall", "--delete-data", "--plan")
	if len(s.term.linePrompts) != 1 {
		t.Errorf("--plan asked: %q", s.term.linePrompts)
	}
	untouched("--plan")

	s.term.lines = []string{"delete"}
	s.want(exitcode.OK, "uninstall", "--delete-data")
	if s.exists(userStateDir) || s.exists(userConfigDir) {
		t.Error("confirmed, yet the data is still there")
	}
}

// Review Focus 5: stop the unit, then revoke the session, then delete — on
// the unit's directories, never the caller's own.
func TestUninstallDeleteDataStopsThenLogsOutThenDeletes(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	s.write(userStateDir+"/pcb.db", "cache", 0o600)
	s.write(userStateDir+"/tls/cert.pem", "cert", 0o600)
	s.write(userStateDir+"/clients.json", "{}", 0o600)
	s.takeEvents()
	s.env["PCB_CONFIG_DIR"] = "/elsewhere/pcb-config"
	s.env["PCB_STATE_DIR"] = "/elsewhere/pcb-state"
	s.want(exitcode.OK, "uninstall", "--delete-data", "--yes", "--non-interactive")
	if got, want := s.takeEvents(), []string{
		showEnv,
		"systemctl --user disable --now pcb.service",
		"systemctl --user daemon-reload",
		"logout (state present: yes)",
	}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if want := [2]string{filepath.Join(s.root, userConfigDir), filepath.Join(s.root, userStateDir)}; s.logoutEnv != want {
		t.Errorf("logout ran on %q, want the unit's %q", s.logoutEnv, want)
	}
	for _, d := range []string{userConfigDir, userStateDir} {
		if s.exists(d) {
			t.Errorf("%s survived --delete-data", d)
		}
	}
	if s.exists(userUnit) {
		t.Error("the unit survived")
	}
}

func TestUninstallKeepsAKeyFileOutsideTheConfigDir(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	s.write(userConfigDir+"/config.toml", "[secrets]\nkey_file = \"/home/alice/keys/pcb.key\"\n", 0o600)
	s.write("/home/alice/keys/pcb.key", "key", 0o600)
	s.want(exitcode.OK, "uninstall", "--delete-data", "--yes")
	if !s.exists("/home/alice/keys/pcb.key") {
		t.Fatal("the key file outside the config directory was deleted")
	}
	if s.exists(userConfigDir) {
		t.Error("the config directory survived")
	}
	if want := "keep file /home/alice/keys/pcb.key — the key_file lies outside the config directory: kept, delete it if unused\n"; !strings.Contains(s.stdout.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, s.stdout.String())
	}
}

// The system uninstall acts on /etc/pcb and /var/lib/pcb, deletes the user
// only with --delete-data, and keeps the binary.
func TestUninstallSystemDeleteData(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.euid = 0
	s.want(exitcode.OK, "install", "--system")
	s.takeEvents()
	s.want(exitcode.OK, "uninstall", "--delete-data", "--yes")
	if got, want := s.takeEvents(), []string{
		"systemctl disable --now pcb.service",
		"systemctl daemon-reload",
		"logout (state present: yes)",
		"userdel pcb",
	}; !slices.Equal(got, want) {
		t.Errorf("events = %q, want %q", got, want)
	}
	if want := [2]string{filepath.Join(s.root, "/etc/pcb"), filepath.Join(s.root, "/var/lib/pcb")}; s.logoutEnv != want {
		t.Errorf("logout ran on %q, want %q", s.logoutEnv, want)
	}
	if s.exists("/etc/pcb") || s.exists("/var/lib/pcb") || s.exists(install.SystemUnitPath) {
		t.Errorf("left behind: %q", s.tree())
	}
	if !s.exists("/usr/local/bin/pcb") || !strings.HasSuffix(s.stdout.String(), "keep file /usr/local/bin/pcb — kept /usr/local/bin/pcb: rm it if unused\n") {
		t.Errorf("binary not kept or not named last:\n%s", s.stdout.String())
	}
}

// I2: every directory and written file of a system install ends with its
// owner and mode, existing directories included.
func TestInstallSystemSetsOwnersAndModes(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.euid = 0
	pcb, root := [2]int{os.Getuid() + 1, os.Getgid() + 1}, [2]int{os.Getuid() + 2, os.Getgid() + 2}
	s.ids["pcb"], s.ids["root"] = pcb, root
	for _, d := range []string{"/etc/pcb", "/var/lib/pcb"} {
		s.mkdir(d)
		if err := os.Chmod(filepath.Join(s.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.want(exitcode.OK, "install", "--system")
	id := func(v [2]int) string { return fmt.Sprintf("%d:%d", v[0], v[1]) }
	for p, want := range map[string]struct {
		mode  fs.FileMode
		owner string
	}{
		"/etc/pcb":             {0o700, id(pcb)},
		"/var/lib/pcb":         {0o700, id(pcb)},
		"/etc/pcb/config.toml": {0o600, id(pcb)},
		"/usr/local/bin/pcb":   {0o755, id(root)},
		install.SystemUnitPath: {0o644, ""},
	} {
		if got := s.mode(p); got != want.mode {
			t.Errorf("mode of %s = %o, want %o", p, got, want.mode)
		}
		if got := s.ownerSet(p); got != want.owner {
			t.Errorf("owner set on %s = %q, want %q", p, got, want.owner)
		}
	}
}

// I2: existing user directories with a looser mode end at 0700.
func TestInstallUserPutsExistingDirectoriesRight(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	for _, d := range []string{userConfigDir, userStateDir} {
		s.mkdir(d)
		if err := os.Chmod(filepath.Join(s.root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s.want(exitcode.OK, "install")
	for _, d := range []string{userConfigDir, userStateDir} {
		if got := s.mode(d); got != 0o700 {
			t.Errorf("mode of %s = %o, want 700", d, got)
		}
		if !strings.Contains(s.stdout.String(), "update dir "+d+" 0700\n") {
			t.Errorf("output lacks the update of %s:\n%s", d, s.stdout.String())
		}
	}
}

// M5: a user install that keeps the unit while the service runs reminds
// that the running service keeps its binary.
func TestInstallUserHintsARestartWhenTheServiceRuns(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	const hint = "pcb service restart   (only if you rebuilt pcb: the running service keeps the binary it started with)"
	s.out[showState] = "ActiveState=active\nSubState=running\nUnitFileState=enabled\nMainPID=42\n"
	s.want(exitcode.OK, "install", "--json")
	if slices.Contains(s.data().Next, hint) {
		t.Errorf("a fresh install hints a restart: %q", s.data().Next)
	}
	s.want(exitcode.OK, "install", "--json")
	if d := s.data(); len(d.Next) == 0 || d.Next[0] != hint {
		t.Errorf("next = %q, want the restart hint first", d.Next)
	}
	s.out[showState] = "ActiveState=inactive\nSubState=dead\nUnitFileState=enabled\nMainPID=0\n"
	s.want(exitcode.OK, "install", "--json")
	if slices.Contains(s.data().Next, hint) {
		t.Errorf("an inactive service gets the hint: %q", s.data().Next)
	}
}

// M5-R6: --delete-data acts only on the directories systemd says the unit
// uses; when they cannot be read it refuses (exit 10) and deletes nothing,
// never falling back to the caller's environment.
func TestUninstallDeleteDataNeedsTheUnitsDirectories(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	s.takeEvents()
	s.out[showEnv] = "Environment=\n"
	s.want(exitcode.MustAct, "uninstall", "--delete-data", "--yes", "--json")
	code, msg, hint := s.failure()
	if code != 10 || !strings.Contains(msg, "cannot be read") || !strings.Contains(hint, userConfigDir+" and "+userStateDir) {
		t.Errorf("failure = %d %q, hint %q", code, msg, hint)
	}
	if got := s.takeEvents(); !slices.Equal(got, []string{showEnv}) {
		t.Errorf("ran %q", got)
	}
	for _, p := range []string{userUnit, userConfigDir, userStateDir} {
		if !s.exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	// Without --delete-data the unit goes and no directory is named.
	s.want(exitcode.OK, "uninstall")
	if s.exists(userUnit) || strings.Contains(s.stdout.String(), "keep dir") {
		t.Errorf("uninstall:\n%s", s.stdout.String())
	}
}

// M3: the prompt names only what will be deleted, from the one plan.
func TestUninstallPromptNamesOnlyWhatIsDeleted(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.write(userUnit, string(install.UserUnit(install.UnitParams{Exec: "/opt/pcb/pcb", ConfigDir: "/srv/pcb-config", StateDir: userStateDir})), 0o644)
	s.mkdir("/srv/pcb-config")
	s.mkdir(userStateDir)
	s.term.interactive = true
	s.term.lines = []string{"delete"}
	s.want(exitcode.OK, "uninstall", "--delete-data")
	if want := []string{"Delete " + userStateDir + " (Proton session, cache, CalDAV clients)? Type delete to confirm: "}; !slices.Equal(s.term.linePrompts, want) {
		t.Errorf("prompts = %q, want %q", s.term.linePrompts, want)
	}
	if !s.exists("/srv/pcb-config") || s.exists(userStateDir) {
		t.Errorf("tree after: %q", s.tree())
	}
	if want := "keep dir /srv/pcb-config — not removed: only a directory named pcb is deleted; to delete it: rm -r -- /srv/pcb-config\n"; !strings.Contains(s.stdout.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, s.stdout.String())
	}
}
