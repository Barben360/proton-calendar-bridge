package cli_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/dbustest"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/install"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// mcpRun is a pcb mcp run of the test: its stdin and stdout are pipes the
// agent of the test holds; stdout is also recorded raw.
type mcpRun struct {
	t      *testing.T
	code   chan exitcode.Code
	errb   *syncBuffer
	stdout *syncBuffer
	agent  sdkmcp.Transport
	stop   context.CancelFunc
}

type readCloser struct {
	io.Reader
	io.Closer
}

// startMCP runs pcb mcp with args on r's directories, its accounts opened
// as a daemon holds them. kr, when set, is the desktop keyring pcb mcp
// probes (IO.Keyring).
func startMCP(t *testing.T, r *rig, kr service.Keyring, args ...string) *mcpRun {
	t.Helper()
	if err := os.MkdirAll(r.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	m := &mcpRun{t: t, code: make(chan exitcode.Code, 1), errb: &syncBuffer{}, stdout: &syncBuffer{}}
	m.agent = &sdkmcp.IOTransport{Reader: readCloser{io.TeeReader(outR, m.stdout), outR}, Writer: inW}
	pio := cli.IO{Stdin: inR, Stdout: outW, Stderr: m.errb, Term: r.term, Env: func(k string) string { return r.env[k] }}
	pio.OpenHub = r.openHub(true)
	pio.Host = noSystemHost()
	if r.keyring != nil {
		pio.Keyring = r.keyring
	}
	if kr != nil {
		pio.Keyring = kr
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	m.stop = cancel
	go func() {
		m.code <- cli.Main(ctx, append([]string{"mcp"}, args...), pio)
		_ = outW.Close()
		_ = inR.Close()
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-m.code:
		case <-time.After(30 * time.Second):
			t.Error("pcb mcp did not stop within 30 s of the test's end")
		}
	})
	return m
}

// connect is the agent's session with pcb mcp on protocol version.
func (m *mcpRun) connect(version string) *sdkmcp.ClientSession {
	m.t.Helper()
	ctx, cancel := context.WithTimeout(m.t.Context(), 20*time.Second)
	defer cancel()
	cs, err := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "agent", Version: "0"}, nil).
		Connect(ctx, m.agent, &sdkmcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		m.t.Fatalf("connect: %v\nstderr: %s", err, m.errb.String())
	}
	m.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// exit waits for pcb mcp to end and returns its exit code.
func (m *mcpRun) exit() exitcode.Code {
	m.t.Helper()
	select {
	case c := <-m.code:
		m.code <- c
		return c
	case <-time.After(30 * time.Second):
		m.t.Fatalf("pcb mcp still runs\nstderr: %s", m.errb.String())
	}
	return 0
}

// mcpStatus calls the status tool and returns the addresses it lists.
func mcpStatus(t *testing.T, cs *sdkmcp.ClientSession) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &sdkmcp.CallToolParams{Name: "status", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("status: %v %+v", err, res)
	}
	var out []string
	for _, a := range res.StructuredContent.(map[string]any)["accounts"].([]any) {
		out = append(out, a.(map[string]any)["account"].(string))
	}
	return out
}

func toolNames(t *testing.T, cs *sdkmcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// socketStatus asks the daemon on the rig's control socket for its status.
func socketStatus(t *testing.T, r *rig) ctl.Status {
	t.Helper()
	c, err := ctl.Dial(t.Context(), r.dirs.Socket())
	if err != nil {
		t.Fatalf("the control socket does not answer: %v", err)
	}
	defer func() { _ = c.Close() }()
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// With no daemon, pcb mcp becomes one for the session: the control socket
// reports kind mcp and no listener, its tools answer over stdio under the
// read-only profile, and the end of stdin ends it with exit 0, the socket
// removed.
func TestMCPStdioBecomesTheDaemon(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	m := startMCP(t, r, nil)
	cs := m.connect("2025-11-25")
	if got := mcpStatus(t, cs); !slices.Equal(got, []string{r.f.Username}) {
		t.Errorf("status accounts = %v, want %s", got, r.f.Username)
	}
	names := toolNames(t, cs)
	if !slices.Contains(names, "events_list") || slices.Contains(names, "event_create") {
		t.Errorf("tools = %v, want the read-only profile's", names)
	}
	st := socketStatus(t, r)
	if st.Daemon == nil || st.Daemon.Kind != "mcp" || st.Listener != nil {
		t.Errorf("daemon %+v listener %+v, want kind mcp without a listener", st.Daemon, st.Listener)
	}
	if st.MCP == nil || st.MCP.Sessions["stdio"] != 1 || st.MCP.Remote {
		t.Errorf("status mcp = %+v, want one stdio session", st.MCP)
	}
	_ = cs.Close()
	if code := m.exit(); code != exitcode.OK {
		t.Fatalf("exit %d after the end of stdin\nstderr: %s", code, m.errb.String())
	}
	if _, err := ctl.Dial(t.Context(), r.dirs.Socket()); !errors.Is(err, ctl.ErrNoDaemon) {
		t.Errorf("the control socket still answers after the session: %v", err)
	}
}

// With a daemon running (pcb serve), pcb mcp relays to it: same accounts,
// the daemon counts a relay session, and serve.lock stays the daemon's.
func TestMCPStdioRelaysToARunningDaemon(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	s.listening()
	m := startMCP(t, r, nil, "--profile", "full")
	cs := m.connect("2026-07-28")
	if got := mcpStatus(t, cs); !slices.Equal(got, []string{r.f.Username}) {
		t.Errorf("status accounts = %v, want %s", got, r.f.Username)
	}
	if names := toolNames(t, cs); !slices.Contains(names, "event_delete") {
		t.Errorf("tools = %v, want the full profile's", names)
	}
	st := socketStatus(t, r)
	if st.Daemon == nil || st.Daemon.Kind != "serve" || st.MCP == nil || st.MCP.Sessions["relay"] != 1 || st.MCP.Sessions["stdio"] != 0 {
		t.Errorf("daemon %+v mcp %+v, want pcb serve with one relay session", st.Daemon, st.MCP)
	}
	if lock, _ := os.ReadFile(filepath.Join(r.dirs.State, "serve.lock")); !strings.HasSuffix(strings.TrimSpace(string(lock)), " serve") {
		t.Errorf("serve.lock = %q, want pcb serve's", lock)
	}
	_ = cs.Close()
	if code := m.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, m.errb.String())
	}
	if st := socketStatus(t, r); st.Daemon.Kind != "serve" {
		t.Errorf("after the relay ended, the daemon is %+v", st.Daemon)
	}
}

// Two pcb mcp started together: one becomes the session daemon, the other
// loses the race for serve.lock, backs off and relays to it.
func TestTwoMCPStdioEndAsDaemonAndRelay(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	a, b := startMCP(t, r, nil), startMCP(t, r, nil)
	var wg sync.WaitGroup
	sessions := make([]*sdkmcp.ClientSession, 2)
	for i, m := range []*mcpRun{a, b} {
		wg.Go(func() { sessions[i] = m.connect("2025-11-25") })
	}
	wg.Wait()
	for i, cs := range sessions {
		if cs == nil {
			t.FailNow()
		}
		if got := mcpStatus(t, cs); !slices.Equal(got, []string{r.f.Username}) {
			t.Errorf("agent %d: status accounts = %v", i, got)
		}
	}
	st := socketStatus(t, r)
	if st.Daemon == nil || st.Daemon.Kind != "mcp" || st.MCP == nil || st.MCP.Sessions["stdio"] != 1 || st.MCP.Sessions["relay"] != 1 {
		t.Errorf("daemon %+v mcp %+v, want one session daemon serving one stdio and one relay session", st.Daemon, st.MCP)
	}
	for _, cs := range sessions {
		_ = cs.Close()
	}
	for i, m := range []*mcpRun{a, b} {
		if code := m.exit(); code != exitcode.OK {
			t.Errorf("pcb mcp %d: exit %d\nstderr: %s", i, code, m.errb.String())
		}
	}
}

// A relay whose daemon ends takes over: the agent's next call re-probes,
// finds no daemon, and pcb mcp becomes the session daemon itself; the
// session goes on.
func TestRelayTakesOverAfterItsDaemonEnds(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	r.want(exitcode.OK, "login", "--non-interactive", "--json")
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	s.listening()
	m := startMCP(t, r, nil)
	cs := m.connect("2025-11-25")
	mcpStatus(t, cs)
	if st := socketStatus(t, r); st.Daemon.Kind != "serve" {
		t.Fatalf("daemon %+v, want pcb serve first", st.Daemon)
	}
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("pcb serve: exit %d", code)
	}
	if got := mcpStatus(t, cs); !slices.Equal(got, []string{r.f.Username}) {
		t.Errorf("status after the takeover = %v", got)
	}
	st := socketStatus(t, r)
	if st.Daemon == nil || st.Daemon.Kind != "mcp" || st.MCP == nil || st.MCP.Sessions["stdio"] != 1 {
		t.Errorf("daemon %+v mcp %+v, want the session daemon serving this session", st.Daemon, st.MCP)
	}
	if !strings.Contains(m.errb.String(), "the daemon went away") {
		t.Errorf("stderr does not say the daemon went away:\n%s", m.errb.String())
	}
}

// With a system-scope pcb unit installed and no daemon of this user, pcb
// mcp refuses to serve the session itself (design §5.3): exit 20 with the
// hint to use the service's remote /mcp. Nothing is created.
func TestMCPStdioRefusesNextToASystemUnit(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.hostRoot = t.TempDir()
	unit := filepath.Join(r.hostRoot, install.SystemUnitPath)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.want(exitcode.Blocked, "mcp")
	want := "Error: pcb runs as a system service here\nHint: use its remote /mcp with a token ([mcp] remote = true; a loopback listen is enough)\n"
	if !strings.Contains(r.errb.String(), want) {
		t.Errorf("stderr = %q, want %q", r.errb.String(), want)
	}
	if r.out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing (it carries MCP only)", r.out.String())
	}
	if _, err := os.Stat(filepath.Join(r.dirs.State, "layout.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("layout.json exists (%v): pcb mcp created the state", err)
	}
}

// pcb mcp speaks MCP on stdout: --json is a bad invocation (exit 2), as
// are an unknown profile, a capability ParseCaps refuses, and --profile
// with --allow; help does not offer --json.
func TestMCPStdioRefusesJSON(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	r.want(exitcode.Usage, "mcp", "--json")
	if e := r.envelope(); e.OK || e.Command != "mcp" || e.Error == nil || !strings.Contains(e.Error.Message, "--json") {
		t.Errorf("envelope = %+v", e)
	}
	for _, args := range [][]string{
		{"--profile", "admin"},
		{"--allow", "delete"},
		{"--allow", "read,root"},
		{"--allow", ""},
		{"--profile", "full", "--allow", "read"},
	} {
		r.want(exitcode.Usage, append([]string{"mcp"}, args...)...)
		if r.out.Len() != 0 || !strings.HasPrefix(r.errb.String(), "Error: ") {
			t.Errorf("%v: stdout %q stderr %q", args, r.out.String(), r.errb.String())
		}
	}
	r.want(exitcode.OK, "mcp", "--help")
	if strings.Contains(r.out.String(), "--json") || !strings.Contains(r.out.String(), "--profile") {
		t.Errorf("pcb mcp --help:\n%s", r.out.String())
	}
}

// Whatever the mode, stdout carries JSON-RPC messages only, one per line;
// the logs go to stderr.
func TestMCPStdioWritesNothingButProtocolOnStdout(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"session daemon", "relay"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, &protontest.Options{})
			r.want(exitcode.OK, "login", "--non-interactive", "--json")
			if mode == "relay" {
				startServe(t, r, nil, "--listen", "127.0.0.1:0").listening()
			}
			m := startMCP(t, r, nil, "--profile", "full")
			cs := m.connect("2025-11-25")
			mcpStatus(t, cs)
			toolNames(t, cs)
			_, _ = cs.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: "event_get", Arguments: map[string]any{"event_id": "nope"}})
			_ = cs.Close()
			if code := m.exit(); code != exitcode.OK {
				t.Fatalf("exit %d\nstderr: %s", code, m.errb.String())
			}
			out := m.stdout.String()
			sc := bufio.NewScanner(strings.NewReader(out))
			sc.Buffer(nil, 1<<20)
			lines := 0
			for sc.Scan() {
				lines++
				if _, err := jsonrpc.DecodeMessage(bytes.TrimSpace(sc.Bytes())); err != nil {
					t.Errorf("stdout line %d is not a JSON-RPC message (%v): %q", lines, err, sc.Text())
				}
			}
			if lines < 4 || !strings.HasSuffix(out, "\n") {
				t.Errorf("stdout holds %d lines, want the session's answers:\n%s", lines, out)
			}
			want := "serving this agent session"
			if mode == "relay" {
				want = "relaying to the daemon"
			}
			if !strings.Contains(m.errb.String(), want) {
				t.Errorf("stderr lacks %q:\n%s", want, m.errb.String())
			}
		})
	}
}

// The sealer guard (design §5.3): on a new state (no layout.json) under
// [secrets] backend = auto with no Secret Service answering (here a
// session bus with none, then no bus at all), pcb mcp will not choose the
// sealer: must-act naming the configuration file. pcb mcp checks it
// before it would create the state: exit 10, nothing written.
func TestSealerGuardRefusesWithoutAKeyring(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	want := "pcb mcp will not choose how secrets are sealed: set [secrets] backend in " + config.Path(r.dirs) +
		", or run the first pcb login in a terminal"
	env := func(k string) string { return r.env[k] }
	for name, kr := range map[string]service.Keyring{
		"a bus without a Secret Service": seal.DBusKeyring(dbustest.Start(t)),
		"no bus":                         seal.DBusKeyring(""),
	} {
		err := cli.SealerGuard(t.Context(), env, kr)
		var ee *exitcode.Error
		if !errors.As(err, &ee) || ee.Code != exitcode.MustAct || err.Error() != want {
			t.Errorf("%s: guard = %v, want must-act %q", name, err, want)
		}
	}

	m := startMCP(t, r, seal.DBusKeyring(dbustest.Start(t)))
	if code := m.exit(); code != exitcode.MustAct {
		t.Fatalf("pcb mcp on a new state without a keyring: exit %d, want 10\nstderr: %s", code, m.errb.String())
	}
	if !strings.Contains(m.errb.String(), "Error: "+want+"\n") {
		t.Errorf("stderr = %q", m.errb.String())
	}
	if _, err := os.Stat(filepath.Join(r.dirs.State, "layout.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("layout.json exists (%v): the sealer was chosen", err)
	}
}

// An explicit [secrets] backend is honoured as by every command, a state
// that has its layout is never probed, and a keyring that answers lets
// auto choose it: the guard passes. pcb mcp then serves a new keyfile
// state.
func TestSealerGuardAllowsAnExplicitBackend(t *testing.T) {
	t.Parallel()
	noBus := seal.DBusKeyring("")
	for _, backend := range []string{"keyfile", "secret-service"} {
		r := newRig(t, nil)
		writeConfig(t, r, "[secrets]\nbackend = \""+backend+"\"\n")
		if err := cli.SealerGuard(t.Context(), func(k string) string { return r.env[k] }, noBus); err != nil {
			t.Errorf("backend %s: guard = %v, want nil", backend, err)
		}
	}
	unlocked := newRig(t, nil)
	if err := cli.SealerGuard(t.Context(), func(k string) string { return unlocked.env[k] }, &memKeyring{}); err != nil {
		t.Errorf("auto with a keyring that answers: guard = %v, want nil", err)
	}
	existing := newRig(t, &protontest.Options{})
	existing.want(exitcode.OK, "login", "--non-interactive", "--json")
	if err := cli.SealerGuard(t.Context(), func(k string) string { return existing.env[k] }, noBus); err != nil {
		t.Errorf("auto on a state with its layout: guard = %v, want nil", err)
	}

	r := newRig(t, nil)
	writeConfig(t, r, "[secrets]\nbackend = \"keyfile\"\n")
	m := startMCP(t, r, noBus)
	cs := m.connect("2025-11-25")
	if got := mcpStatus(t, cs); len(got) != 0 {
		t.Errorf("status accounts on a new state = %v", got)
	}
	layout, err := os.ReadFile(filepath.Join(r.dirs.State, "layout.json"))
	if err != nil || !strings.Contains(string(layout), `"keyfile"`) {
		t.Errorf("layout.json = %q (%v), want the keyfile sealer", layout, err)
	}
}

// The serve.lock race backs off from 250 ms, doubling, 10 s in all.
func TestMCPLockRaceBacksOffAtMostTenSeconds(t *testing.T) {
	t.Parallel()
	got := cli.MCPBackoff()
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 2250 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Errorf("back-off = %v, want %v", got, want)
	}
}
