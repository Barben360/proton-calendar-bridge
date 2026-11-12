package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/otp"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// fakeTerm answers prompts from queues and records them.
type fakeTerm struct {
	interactive    bool
	lines, secrets []string
	linePrompts    []string
	secretPrompts  []string
	// block makes every read wait for ctx to end, like a person not answering.
	block bool
	// onBlock runs when a read starts waiting (the test cancels there).
	onBlock func()
}

func (f *fakeTerm) Interactive() bool { return f.interactive }

func (f *fakeTerm) ReadLine(ctx context.Context, prompt string) (string, error) {
	f.linePrompts = append(f.linePrompts, prompt)
	if f.block {
		if f.onBlock != nil {
			f.onBlock()
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	if len(f.lines) == 0 {
		return "", io.EOF
	}
	l := f.lines[0]
	f.lines = f.lines[1:]
	return l, nil
}

func (f *fakeTerm) ReadSecret(ctx context.Context, prompt string) (string, error) {
	f.secretPrompts = append(f.secretPrompts, prompt)
	if f.block {
		if f.onBlock != nil {
			f.onBlock()
		}
		<-ctx.Done()
		return "", ctx.Err()
	}
	if len(f.secrets) == 0 {
		return "", io.EOF
	}
	s := f.secrets[0]
	f.secrets = f.secrets[1:]
	return s, nil
}

// stubAPI is a service.API that records what the CLI asks of it.
type stubAPI struct {
	login       func(service.LoginRequest) (service.LoginResult, error)
	logins      []service.LoginRequest
	totps       []string
	submit      func(string) (service.LoginResult, error)
	closed      int
	closeErr    error
	statusPhase string
}

func (s *stubAPI) Login(_ context.Context, r service.LoginRequest) (service.LoginResult, error) {
	s.logins = append(s.logins, r)
	return s.login(r)
}

func (s *stubAPI) SubmitTOTP(_ context.Context, c string) (service.LoginResult, error) {
	s.totps = append(s.totps, c)
	return s.submit(c)
}

func (s *stubAPI) Logout(context.Context) (service.LogoutResult, error) {
	return service.LogoutResult{}, nil
}

func (s *stubAPI) Status(context.Context) (service.Status, error) {
	return service.Status{Phase: s.statusPhase}, nil
}

func (*stubAPI) Sync(context.Context, service.SyncRequest) (service.SyncResult, error) {
	return service.SyncResult{}, nil
}

func (*stubAPI) WatchSync(context.Context, service.SyncRequest, time.Duration, func(service.WatchPass)) error {
	return nil
}

func (*stubAPI) Serve(context.Context, service.ServeOptions) error { return nil }

func (*stubAPI) Events(context.Context, service.EventsRequest) ([]service.Event, error) {
	return nil, nil
}

func (*stubAPI) Collections(context.Context) ([]service.Collection, error) { return nil, nil }

func (*stubAPI) Objects(context.Context, string) ([]service.ObjectInfo, string, error) {
	return nil, "", nil
}

func (*stubAPI) ObjectsByUID(context.Context, string, []string) ([]service.Object, error) {
	return nil, nil
}

func (*stubAPI) Changes(context.Context, string, string) (service.ChangeSet, error) {
	return service.ChangeSet{}, nil
}

func (*stubAPI) AddClient(context.Context, string) (service.ClientSecret, error) {
	return service.ClientSecret{}, nil
}
func (*stubAPI) Clients(context.Context) ([]service.Client, error) { return nil, nil }
func (*stubAPI) RemoveClient(context.Context, string) (service.Client, error) {
	return service.Client{}, nil
}
func (*stubAPI) CheckClient(context.Context, string, string) (bool, time.Duration) { return false, 0 }

func (*stubAPI) PutObject(context.Context, string, string, []byte, service.Precondition) (service.PutResult, error) {
	return service.PutResult{}, nil
}
func (*stubAPI) DeleteObject(context.Context, string, string, service.Precondition) error { return nil }

func (s *stubAPI) Calendars(context.Context) ([]service.Calendar, error) { return nil, nil }
func (s *stubAPI) Close() error                                          { s.closed++; return s.closeErr }

func ready() (service.LoginResult, error) {
	return service.LoginResult{State: service.StateReady, Username: "u", AccountID: "id"}, nil
}

// t0 is the fixed clock of every rig: the service and the fake's TOTP agree
// on it, mid-period, so codes are deterministic and nothing ever waits.
var t0 = time.Date(2026, 11, 11, 10, 0, 7, 0, time.UTC)

// rig runs the CLI against a fake Proton, an isolated directory tree and an
// environment map.
type rig struct {
	t    *testing.T
	f    *protontest.Fake
	dirs paths.Dirs
	env  map[string]string
	term *fakeTerm
	svc  service.API // injected service, or nil for the default one
	mk   func() (service.API, error)
	// ctrlC cancels the command like Ctrl-C when it first waits on the terminal.
	ctrlC bool
	// cancelled is when ctrlC fired during the last run; zero if it did not.
	cancelled time.Time
	out       bytes.Buffer
	errb      bytes.Buffer
}

// stubRig is a rig on a stub service, without a Proton: nothing can reach one.
func stubRig(t *testing.T, s *stubAPI) *rig {
	t.Helper()
	r := newRig(t, nil)
	r.svc = s
	return r
}

// newRig is a rig on a fake Proton built with o; with nil o, a rig without
// Proton whose service the caller injects.
func newRig(t *testing.T, o *protontest.Options) *rig {
	t.Helper()
	var f *protontest.Fake
	user, password, baseURL := "user", "pw", "http://127.0.0.1:1"
	if o != nil {
		o.Now = func() time.Time { return t0 }
		f = protontest.New(t, *o)
		user, password, baseURL = f.Username, f.Password, f.URL
	}
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "cfg", "pcb"), State: filepath.Join(root, "state", "pcb")}
	if err := os.MkdirAll(dirs.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	toml := "[proton]\nbase_url = \"" + baseURL + "\"\n"
	if err := os.WriteFile(config.Path(dirs), []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, f: f, dirs: dirs, term: &fakeTerm{}, env: map[string]string{
		"XDG_CONFIG_HOME": filepath.Dir(dirs.Config),
		"XDG_STATE_HOME":  filepath.Dir(dirs.State),
		"PCB_USERNAME":    user,
		"PCB_PASSWORD":    password,
	}}
	if f != nil {
		r.useLocal()
	}
	return r
}

// useLocal makes the rig open the real service on its directories with the
// fixed clock and no sleeping (the way the default one does, config.toml
// included).
func (r *rig) useLocal() {
	r.mk = func() (service.API, error) {
		cfg, err := config.Load(config.Path(r.dirs), r.dirs)
		if err != nil {
			return nil, err
		}
		return service.NewLocal(service.Options{Dirs: r.dirs, Config: cfg,
			Now:   func() time.Time { return t0 },
			Sleep: func(context.Context, time.Duration) error { return nil }})
	}
}

func (r *rig) run(args ...string) exitcode.Code {
	r.t.Helper()
	r.out.Reset()
	r.errb.Reset()
	r.cancelled = time.Time{}
	io := cli.IO{Stdout: &r.out, Stderr: &r.errb, Term: r.term, Env: func(k string) string { return r.env[k] }}
	switch {
	case r.svc != nil:
		io.NewService = func(context.Context, func(string) string) (service.API, error) { return r.svc, nil }
	case r.mk != nil:
		io.NewService = func(context.Context, func(string) string) (service.API, error) { return r.mk() }
	}
	ctx, cancel := context.WithTimeout(r.t.Context(), 60*time.Second)
	defer cancel()
	if r.ctrlC {
		r.term.onBlock = func() {
			r.cancelled = time.Now()
			cancel()
		}
	}
	return cli.Main(ctx, args, io)
}

// totpCode is a current code for the fake's account.
func (r *rig) totpCode() string {
	r.t.Helper()
	o, err := otp.Parse(r.f.TOTPURI)
	if err != nil {
		r.t.Fatal(err)
	}
	return o.Code(t0)
}

type envelope struct {
	Command string          `json:"command"`
	OK      bool            `json:"ok"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Hint    string `json:"hint"`
	} `json:"error"`
}

// envelope decodes the one envelope the last run printed on stdout.
func (r *rig) envelope() envelope {
	r.t.Helper()
	var e envelope
	dec := json.NewDecoder(bytes.NewReader(r.out.Bytes()))
	if err := dec.Decode(&e); err != nil {
		r.t.Fatalf("stdout is not an envelope: %v\n%q", err, r.out.String())
	}
	if dec.More() {
		r.t.Fatalf("more than one JSON value on stdout: %q", r.out.String())
	}
	return e
}

func (r *rig) want(code exitcode.Code, args ...string) {
	r.t.Helper()
	if got := r.run(args...); got != code {
		r.t.Fatalf("pcb %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), got, code, r.out.String(), r.errb.String())
	}
}
