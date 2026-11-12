package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
	"github.com/Barben360/proton-calendar-bridge/internal/store/seal"
)

// clientsEnv is a service with no Proton behind it (an unreachable base URL,
// no login): client management must work logged out and offline.
type clientsEnv struct {
	svc  *service.Local
	dirs paths.Dirs
	clk  *clock
}

func newClientsEnv(t *testing.T) *clientsEnv {
	t.Helper()
	root := t.TempDir()
	e := &clientsEnv{clk: &clock{now: t0}, dirs: paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}}
	cfg := config.Defaults(e.dirs)
	cfg.Proton.BaseURL = "http://127.0.0.1:1"
	s, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: cfg, Now: e.clk.Now, Sleep: e.clk.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	e.svc = s
	return e
}

func wantCode(t *testing.T, err error, code exitcode.Code) {
	t.Helper()
	var ee *exitcode.Error
	if !errors.As(err, &ee) || ee.Code != code {
		t.Fatalf("error = %v, want an exit code %d error", err, code)
	}
}

func TestClientLifecycleWorksLoggedOut(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	sec, err := e.svc.AddClient(t.Context(), "Thunderbird")
	if err != nil {
		t.Fatal(err)
	}
	if sec.Name != "Thunderbird" || sec.ID == "" || len(sec.Password) != 32 {
		t.Fatalf("secret = %#v-ish (id %q, name %q, %d password chars), want a 24-byte base64url password", "redacted", sec.ID, sec.Name, len(sec.Password))
	}
	list, err := e.svc.Clients(t.Context())
	if err != nil || len(list) != 1 || list[0].ID != sec.ID || list[0].Name != "Thunderbird" || list[0].LastUsed != nil {
		t.Fatalf("Clients = %+v, %v", list, err)
	}
	if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:5000"); !ok {
		t.Fatal("the added password was refused")
	}
	if _, err := e.svc.RemoveClient(t.Context(), "thunderbird"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:5000"); ok {
		t.Fatal("a removed client's password was accepted")
	}
	if list, _ := e.svc.Clients(t.Context()); len(list) != 0 {
		t.Errorf("Clients after remove = %+v", list)
	}
}

func TestClientNamesAreUniqueAndValidatedAsUsageErrors(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	if _, err := e.svc.AddClient(t.Context(), "Phone"); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.AddClient(t.Context(), "PHONE")
	wantCode(t, err, exitcode.Usage)
	for _, bad := range []string{"", " x", "x ", "a\tb", strings.Repeat("n", 65)} {
		_, err := e.svc.AddClient(t.Context(), bad)
		wantCode(t, err, exitcode.Usage)
	}
	_, err = e.svc.RemoveClient(t.Context(), "nobody")
	wantCode(t, err, exitcode.Usage)
	if list, _ := e.svc.Clients(t.Context()); len(list) != 1 {
		t.Errorf("%d clients, want 1", len(list))
	}
}

func TestAuthFailuresAreChallengedAndLimited(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	sec, err := e.svc.AddClient(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		ok, retry := e.svc.CheckClient(t.Context(), "wrong", fmt.Sprintf("203.0.113.5:%d", 50000+i))
		if ok || retry != 0 {
			t.Fatalf("failure %d: ok=%v retry=%v, want a plain refusal (401)", i+1, ok, retry)
		}
	}
	// Blocked: even the right password is not tried (429).
	ok, retry := e.svc.CheckClient(t.Context(), sec.Password, "203.0.113.5:1")
	if ok || retry <= 0 || retry > time.Minute {
		t.Fatalf("after 5 failures: ok=%v retry=%v, want blocked for under a minute", ok, retry)
	}
	if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "203.0.113.6:1"); !ok {
		t.Error("another IP is blocked")
	}
	e.clk.advance(retry)
	if ok, r := e.svc.CheckClient(t.Context(), sec.Password, "203.0.113.5:1"); !ok || r != 0 {
		t.Errorf("after the window: ok=%v retry=%v, want accepted", ok, r)
	}
}

// touches records the service's background touches: started and finished
// ids, delivered on channels so tests wait for them without sleeping.
type touches struct{ started, done chan string }

// next waits for an event of ch; a touch that never happens fails the test
// instead of hanging it (the timeout is a guard, never a synchronisation).
func next(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(10 * time.Second):
		t.Fatalf("no background touch %s", what)
		return ""
	}
}

func hookTouches(l *service.Local) *touches {
	h := &touches{started: make(chan string, 16), done: make(chan string, 16)}
	l.SetTouchHook(func(phase, id string) {
		if phase == "start" {
			h.started <- id
		} else {
			h.done <- id
		}
	})
	return h
}

func TestCheckClientRecordsLastUseOffTheRequestPathAtMostOncePerMinute(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	h := hookTouches(e.svc)
	sec, _ := e.svc.AddClient(t.Context(), "Phone")
	if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1"); !ok {
		t.Fatal("refused")
	}
	first := e.clk.Now()
	next(t, h.done, "finished")
	list, _ := e.svc.Clients(t.Context())
	if list[0].LastUsed == nil || !list[0].LastUsed.Equal(first) {
		t.Fatalf("LastUsed = %v, want %v", list[0].LastUsed, first)
	}
	e.clk.advance(30 * time.Second)
	e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1") //nolint:errcheck // result not under test
	next(t, h.done, "finished")                                 // the touch ran (and was throttled by the store)
	if list, _ := e.svc.Clients(t.Context()); !list[0].LastUsed.Equal(first) {
		t.Errorf("LastUsed moved within a minute: %v", list[0].LastUsed)
	}
	// Finished touches free the client: a due one is recorded.
	e.clk.advance(time.Minute)
	e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1") //nolint:errcheck // result not under test
	next(t, h.done, "finished once more")
	if list, _ := e.svc.Clients(t.Context()); !list[0].LastUsed.Equal(e.clk.Now()) {
		t.Errorf("LastUsed = %v, want %v after a minute", list[0].LastUsed, e.clk.Now())
	}
}

// A CLI holding clients.lock must not stall requests (ruling M3-R21), and a
// client already being touched is not touched again.
func TestCheckClientDoesNotWaitForTheClientsLockAndTouchesOnceInFlight(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	h := hookTouches(e.svc)
	sec, _ := e.svc.AddClient(t.Context(), "Phone")
	unlock, held, err := filelock.TryLock(filepath.Join(e.dirs.State, "clients.lock"))
	if err != nil || !held {
		t.Skip("no file locks on this system")
	}
	returned := make(chan bool)
	go func() {
		ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1")
		returned <- ok
	}()
	select {
	case ok := <-returned:
		if !ok {
			t.Fatal("refused")
		}
	case <-time.After(10 * time.Second): // a guard, not a synchronisation
		t.Fatal("CheckClient waited for the clients lock")
	}
	next(t, h.started, "started") // in flight, blocked on the lock
	e.clk.advance(2 * time.Minute)
	for range 3 {
		if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:2"); !ok {
			t.Fatal("refused")
		}
	}
	unlock()
	next(t, h.done, "finished after the lock was released")
	// Close waits for every touch there is, so after it the events are complete:
	// the first touch finished, and no second one ever started.
	if err := e.svc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-h.started:
		t.Fatalf("a second touch of %s started while one was in flight", id)
	case id := <-h.done:
		t.Fatalf("a second touch of %s finished", id)
	default:
	}
}

func TestCloseWaitsForBackgroundTouches(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	sec, _ := e.svc.AddClient(t.Context(), "Phone")
	unlock, held, err := filelock.TryLock(filepath.Join(e.dirs.State, "clients.lock"))
	if err != nil || !held {
		t.Skip("no file locks on this system")
	}
	defer unlock()
	h := hookTouches(e.svc)
	e.svc.SetTouchTimeout(time.Hour)                               // only Close can end the blocked touch in time
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second) // a synchronous touch ends by this
	defer cancel()
	e.svc.CheckClient(ctx, sec.Password, "127.0.0.1:1") //nolint:errcheck // result not under test
	next(t, h.started, "started")
	closed := make(chan error, 1)
	go func() { closed <- e.svc.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second): // a guard, not a synchronisation
		t.Fatal("Close did not cancel the touch blocked on the clients lock")
	}
	select {
	case <-h.done:
	default:
		t.Error("Close returned while a touch was still running")
	}
}

func TestClientsSurviveLogout(t *testing.T) { // ruling M3-R17
	t.Parallel()
	e := newClientsEnv(t)
	sec, _ := e.svc.AddClient(t.Context(), "Phone")
	if _, err := e.svc.Logout(t.Context(), service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1"); !ok {
		t.Error("logout removed the CalDAV clients")
	}
	if _, err := os.Stat(filepath.Join(e.dirs.State, "clients.json")); err != nil {
		t.Errorf("clients.json after logout: %v", err)
	}
}

func TestClientSecretRedactsItself(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	sec, _ := e.svc.AddClient(t.Context(), "Phone")
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("added", "secret", sec)
	slog.New(slog.NewTextHandler(&buf, nil)).Info("added", slog.Any("secret", sec))
	for name, got := range map[string]string{
		"%v": fmt.Sprintf("%v", sec), "%+v": fmt.Sprintf("%+v", sec), "%s": fmt.Sprintf("%s", sec), //nolint:staticcheck // the verb is the point
		"%#v": fmt.Sprintf("%#v", sec), "&%v": fmt.Sprintf("%v", &sec), "slog": buf.String(),
	} {
		if strings.Contains(got, sec.Password) {
			t.Errorf("%s leaks the password: %s", name, got)
		}
	}
	if !strings.Contains(fmt.Sprint(sec), "redacted") {
		t.Errorf("String() = %q, want a redaction marker", fmt.Sprint(sec))
	}
}

// loggedEnv is newEnv on a fake Proton with the service's log in buf.
func loggedEnv(t *testing.T, fo protontest.Options) (*env, *bytes.Buffer) {
	t.Helper()
	e := newEnv(t, fo)
	buf := &syncBuffer{}
	s, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
		Logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_ = e.svc.Close()
	e.svc = s
	return e, &buf.b
}

// syncBuffer is a bytes.Buffer the service's goroutines may write to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (e *env) show(t *testing.T, name string) service.ClientCredentials {
	t.Helper()
	cr, err := e.svc.ShowClient(t.Context(), name)
	if err != nil {
		t.Fatalf("ShowClient(%q): %v", name, err)
	}
	return cr
}

const createdLog = `created the CalDAV client \"default\": see pcb client show`

func TestFirstLoginCreatesTheDefaultClient(t *testing.T) {
	t.Parallel()
	for _, twoFA := range []bool{false, true} { // Login, and SubmitTOTP (the path a held login resumes on)
		t.Run(fmt.Sprint("2FA=", twoFA), func(t *testing.T) {
			t.Parallel()
			e, logs := loggedEnv(t, protontest.Options{TwoFA: twoFA})
			res := e.login(t, e.req())
			if twoFA {
				if res.DefaultClientCreated {
					t.Fatal("a login waiting for its code created the client")
				}
				if list, _ := e.svc.Clients(t.Context()); len(list) != 0 {
					t.Fatalf("clients before the code = %+v", list)
				}
				var err error
				if res, err = e.svc.SubmitTOTP(t.Context(), code(t, e.f.TOTPURI, t0)); err != nil {
					t.Fatal(err)
				}
			}
			if res.State != service.StateReady || !res.DefaultClientCreated {
				t.Fatalf("result = %+v, want ready with the default client created", res)
			}
			list, err := e.svc.Clients(t.Context())
			if err != nil || len(list) != 1 || list[0].Name != "default" {
				t.Fatalf("Clients = %+v, %v; want default", list, err)
			}
			cr := e.show(t, "")
			if cr.ID != list[0].ID || cr.Username != "default" || !cr.Recoverable || len(cr.Password) != 32 {
				t.Fatalf("ShowClient(\"\") = id %q username %q recoverable %v, %d password chars", cr.ID, cr.Username, cr.Recoverable, len(cr.Password))
			}
			if ok, _ := e.svc.CheckClient(t.Context(), cr.Password, "127.0.0.1:1"); !ok {
				t.Error("the shown password does not pass the check")
			}
			if !strings.Contains(logs.String(), createdLog) {
				t.Errorf("log = %q, want %q", logs.String(), createdLog)
			}
		})
	}
}

func TestDefaultIsNotRecreatedOnceAnyClientExists(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	if _, err := e.svc.AddClient(t.Context(), "Phone"); err != nil {
		t.Fatal(err)
	}
	if res := e.signIn(t); res.DefaultClientCreated {
		t.Error("the login reported a default client with Phone there")
	}
	if list, _ := e.svc.Clients(t.Context()); len(list) != 1 || list[0].Name != "Phone" {
		t.Fatalf("clients = %+v, want Phone only", list)
	}
	// Removing every client lets the next login create it; once it exists,
	// a new login keeps it as is.
	if _, err := e.svc.RemoveClient(t.Context(), "Phone"); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(61 * time.Second)
	if res := e.signIn(t); !res.DefaultClientCreated {
		t.Fatal("a login with no client left did not create default")
	}
	first := e.show(t, "default")
	e.clk.advance(61 * time.Second)
	if res := e.signIn(t); res.DefaultClientCreated {
		t.Error("a second login created default again")
	}
	if again := e.show(t, "default"); again.ID != first.ID || again.Password != first.Password {
		t.Error("a second login replaced the default client")
	}
	// Removing default is allowed and sticks while another client exists.
	if _, err := e.svc.AddClient(t.Context(), "Laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RemoveClient(t.Context(), "default"); err != nil {
		t.Fatal(err)
	}
	e.clk.advance(61 * time.Second)
	e.signIn(t)
	if list, _ := e.svc.Clients(t.Context()); len(list) != 1 || list[0].Name != "Laptop" {
		t.Errorf("clients = %+v, want Laptop only", list)
	}
}

// Logout keeps the key file and the clients (ruling M3-R17), so a password
// stays showable across a logout and a new login.
func TestPasswordStaysRecoverableAcrossLogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.signIn(t)
	before := e.show(t, "")
	if _, err := e.svc.Logout(t.Context(), service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	if after := e.show(t, ""); !after.Recoverable || after.Password != before.Password {
		t.Fatalf("after logout: recoverable %v, same password %v", after.Recoverable, after.Password == before.Password)
	}
	e.clk.advance(61 * time.Second)
	e.signIn(t)
	if after := e.show(t, ""); !after.Recoverable || after.Password != before.Password || after.ID != before.ID {
		t.Fatalf("after a new login: recoverable %v, same password %v, same client %v", after.Recoverable, after.Password == before.Password, after.ID == before.ID)
	}
	// A fresh process opens it too: the key is the file's, not the process's.
	if again, err := e.open(t).ShowClient(t.Context(), ""); err != nil || again.Password != before.Password {
		t.Fatalf("a new service: %v, same password %v", err, again.Password == before.Password)
	}
}

func TestShowClientPicksDefaultElseTheOnlyClient(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	_, err := e.svc.ShowClient(t.Context(), "")
	wantCode(t, err, exitcode.MustAct) // no client at all
	phone, _ := e.svc.AddClient(t.Context(), "Phone")
	if cr, err := e.svc.ShowClient(t.Context(), ""); err != nil || cr.ID != phone.ID || cr.Password != phone.Password || cr.Username != "Phone" {
		t.Fatalf("the only client: %v (id %q, username %q)", err, cr.ID, cr.Username)
	}
	if _, err := e.svc.AddClient(t.Context(), "Laptop"); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.ShowClient(t.Context(), "")
	wantCode(t, err, exitcode.MustAct)
	if h := exitcode.HintOf(err); h != "name a client: pcb client show NAME (pcb client list)" {
		t.Errorf("hint = %q", h)
	}
	def, _ := e.svc.AddClient(t.Context(), "Default")
	if cr, err := e.svc.ShowClient(t.Context(), ""); err != nil || cr.ID != def.ID {
		t.Errorf("with default among several: %v, id %q, want default's", err, cr.ID)
	}
	if cr, err := e.svc.ShowClient(t.Context(), phone.ID); err != nil || cr.Name != "Phone" {
		t.Errorf("by ID: %v, %q", err, cr.Name)
	}
	_, err = e.svc.ShowClient(t.Context(), "nobody")
	wantCode(t, err, exitcode.Usage)
}

func TestRotateClientReplacesThePassword(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	old, _ := e.svc.AddClient(t.Context(), "Phone")
	sec, err := e.svc.RotateClient(t.Context(), "phone")
	if err != nil || sec.ID != old.ID || sec.Name != "Phone" || sec.Password == old.Password || len(sec.Password) != 32 {
		t.Fatalf("RotateClient = id %q name %q, new password %v, %v", sec.ID, sec.Name, sec.Password != old.Password, err)
	}
	if ok, _ := e.svc.CheckClient(t.Context(), old.Password, "127.0.0.1:1"); ok {
		t.Error("the old password still passes")
	}
	if cr, _ := e.svc.ShowClient(t.Context(), "Phone"); cr.Password != sec.Password {
		t.Error("show does not give the rotated password")
	}
	_, err = e.svc.RotateClient(t.Context(), "nobody")
	wantCode(t, err, exitcode.Usage)
}

func TestShowClientMadeBeforeSealingIsNotRecoverable(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	sec, _ := e.svc.AddClient(t.Context(), "Old")
	stripSealed(t, e.dirs.State)
	cr, err := e.svc.ShowClient(t.Context(), "Old")
	if err != nil || cr.Recoverable || cr.Password != "" || cr.Username != "Old" {
		t.Fatalf("ShowClient(v1-style) = recoverable %v, %d password chars, username %q, %v; want not recoverable, no error", cr.Recoverable, len(cr.Password), cr.Username, err)
	}
	if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1"); !ok {
		t.Error("an unsealed client no longer passes the check")
	}
}

// stripSealed rewrites clients.json as pcb wrote it before decision 0013.
func stripSealed(t *testing.T, state string) {
	t.Helper()
	path := filepath.Join(state, "clients.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Version int              `json:"version"`
		Clients []map[string]any `json:"clients"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	f.Version = 1
	for _, c := range f.Clients {
		delete(c, "sealed")
	}
	if data, err = json.Marshal(f); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestShowClientWithoutTheKeyNamesItAndCheckStillWorks(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	sec, _ := e.svc.AddClient(t.Context(), "Phone")
	key := config.Defaults(e.dirs).Secrets.KeyFile
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.ShowClient(t.Context(), "Phone")
	wantCode(t, err, exitcode.MustAct)
	if !strings.Contains(err.Error(), key) || !strings.Contains(exitcode.HintOf(err), "pcb client rotate") {
		t.Errorf("error = %q, hint %q; want the key file named and rotate offered", err, exitcode.HintOf(err))
	}
	if strings.Contains(err.Error()+exitcode.HintOf(err), sec.Password) {
		t.Error("the error carries the password")
	}
	if ok, _ := e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1"); !ok {
		t.Error("the check needs the key: it must not")
	}
}

func TestClientCredentialsRedactThemselves(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	e.svc.AddClient(t.Context(), "Phone") //nolint:errcheck // shown below
	cr, err := e.svc.ShowClient(t.Context(), "Phone")
	if err != nil || cr.Password == "" {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("shown", "credentials", cr)
	for name, got := range map[string]string{
		"%v": fmt.Sprintf("%v", cr), "%+v": fmt.Sprintf("%+v", cr), "%#v": fmt.Sprintf("%#v", cr), "slog": buf.String(),
	} {
		if strings.Contains(got, cr.Password) {
			t.Errorf("%s leaks the password: %s", name, got)
		}
	}
}

func TestNoPasswordInLogs(t *testing.T) {
	t.Parallel()
	e, logs := loggedEnv(t, protontest.Options{})
	e.signIn(t)
	var passwords []string
	passwords = append(passwords, e.show(t, "default").Password)
	sec, err := e.svc.AddClient(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	passwords = append(passwords, sec.Password)
	rot, err := e.svc.RotateClient(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	passwords = append(passwords, rot.Password, e.show(t, "Phone").Password)
	for _, pw := range passwords {
		if pw == "" {
			t.Fatal("a password is empty: the scan would be vacuous")
		}
		if strings.Contains(logs.String(), pw) {
			t.Errorf("the log holds a client password:\n%s", logs.String())
		}
	}
	if !strings.Contains(logs.String(), createdLog) {
		t.Fatalf("the log is empty of the default-client line, so the scan proves little:\n%s", logs.String())
	}
}

// M5-R14(2): with secrets.sealed there and its key file lost, add and
// rotate refuse rather than mint a key that would turn "missing" into
// "corrupt" for the stored session; nothing is written, no key created.
func TestClientsRefuseToMintAKeyWhileTheSessionsKeyIsMissing(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.signIn(t)
	before, err := os.ReadFile(filepath.Join(e.dirs.State, "clients.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(e.cfg.Secrets.KeyFile); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.AddClient(t.Context(), "Phone")
	wantCode(t, err, exitcode.MustAct)
	if !strings.Contains(err.Error(), e.cfg.Secrets.KeyFile) || !strings.Contains(exitcode.HintOf(err), "pcb logout, then pcb login") {
		t.Errorf("add: error %q, hint %q; want the key file named and logout then login", err, exitcode.HintOf(err))
	}
	_, err = e.svc.RotateClient(t.Context(), "default")
	wantCode(t, err, exitcode.MustAct)
	if _, err := os.Stat(e.cfg.Secrets.KeyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a key file was created: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(e.dirs.State, "clients.json")); !bytes.Equal(before, after) {
		t.Error("clients.json changed")
	}
	// show names the same way out, not rotate (which is refused).
	_, err = e.svc.ShowClient(t.Context(), "default")
	wantCode(t, err, exitcode.MustAct)
	if h := exitcode.HintOf(err); !strings.Contains(h, "pcb logout, then pcb login") || strings.Contains(h, "rotate") {
		t.Errorf("show hint = %q; want logout then login, not rotate", h)
	}
	// The stored session reads as a missing key still, not as corrupt.
	if st, _ := e.svc.Status(t.Context()); st.Secrets != "unreadable" {
		t.Errorf("secrets = %q", st.Secrets)
	}
	if _, err := e.secrets().Load(); !errors.Is(err, seal.ErrNoKey) {
		t.Errorf("secrets.sealed now loads as %v, want seal.ErrNoKey", err)
	}
}

// Logged out, the lost key holds nothing else: rotate makes a new key and
// the client recoverable again.
func TestRotateRecoversAClientWhenTheKeyIsLostLoggedOut(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	e.svc.AddClient(t.Context(), "Phone") //nolint:errcheck // shown below
	key := config.Defaults(e.dirs).Secrets.KeyFile
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.ShowClient(t.Context(), "Phone")
	wantCode(t, err, exitcode.MustAct)
	if !strings.Contains(exitcode.HintOf(err), "pcb client rotate Phone") {
		t.Errorf("show hint = %q, want rotate offered", exitcode.HintOf(err))
	}
	sec, err := e.svc.RotateClient(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	if cr, err := e.svc.ShowClient(t.Context(), "Phone"); err != nil || !cr.Recoverable || cr.Password != sec.Password {
		t.Errorf("after rotate: %v, recoverable %v", err, cr.Recoverable)
	}
}

func TestShowClientWhoseSealedCopyIsNotItsPasswordSaysSo(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	e.svc.AddClient(t.Context(), "a") //nolint:errcheck // edited below
	e.svc.AddClient(t.Context(), "b") //nolint:errcheck // edited below
	path := filepath.Join(e.dirs.State, "clients.json")
	data, _ := os.ReadFile(path)
	var f map[string]any
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	cs := f["clients"].([]any)
	a, b := cs[0].(map[string]any), cs[1].(map[string]any)
	a["salt"], a["hash"] = b["salt"], b["hash"]
	data, _ = json.Marshal(f)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.ShowClient(t.Context(), "a")
	wantCode(t, err, exitcode.MustAct)
	if !strings.Contains(err.Error(), "does not match its password") || strings.Contains(err.Error(), "does not open") {
		t.Errorf("error = %q; want the mismatch named", err)
	}
}

// M5-R15 C2: a key file this user cannot read (a container secret left
// host-owned) fails with its cause, never a bare "could not be sealed";
// a key other users can read (mode 0444, a compose default) says so too.
func TestAddClientNamesWhyTheKeyCannotBeUsed(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode 0000 file")
	}
	e := newClientsEnv(t)
	if _, err := e.svc.AddClient(t.Context(), "Phone"); err != nil {
		t.Fatal(err)
	}
	key := config.Defaults(e.dirs).Secrets.KeyFile
	for _, c := range []struct {
		mode fs.FileMode
		want string
	}{{0o000, "permission denied"}, {0o444, "mode 0444"}} {
		if err := os.Chmod(key, c.mode); err != nil {
			t.Fatal(err)
		}
		_, err := e.svc.AddClient(t.Context(), "Laptop")
		wantCode(t, err, exitcode.MustAct) // ux-I3: a key fault is the person's to repair
		if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), key) {
			t.Errorf("mode %04o: error %q, want the key file and %q", c.mode, err, c.want)
		}
		// show, too: the key is not lost, so rotate is no way out.
		_, err = e.svc.ShowClient(t.Context(), "Phone")
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(exitcode.HintOf(err), "rotate") {
			t.Errorf("mode %04o: show error %v, hint %q; want %q and no rotate", c.mode, err, exitcode.HintOf(err), c.want)
		}
	}
	// ux-M: a key of the wrong size names its size, as the mode and the
	// permission do.
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.AddClient(t.Context(), "Laptop")
	wantCode(t, err, exitcode.MustAct)
	const want = "the client's password could not be sealed with the key file "
	if got := err.Error(); got != want+key+" (it holds 5 bytes, not a 32-byte key), so nothing was stored" {
		t.Errorf("wrong size: error %q, want the size named", got)
	}
	if h := exitcode.HintOf(err); h != "the key file must be readable by the user pcb runs as, 32 bytes, mode 0600 (pcb doctor)" {
		t.Errorf("wrong size: hint %q", h)
	}
}

// A clients.json that cannot be parsed is a state that can pass, not a wrong
// password: every request, the valid one included, is a 503 wait; it is
// logged once per distinct failure (path and cause, no content) and no
// failure is charged to the remote (audit surface-I2).
func TestCorruptClientsFileAnswersARetryLogsOnceAndChargesNoLimiter(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(dirs)
	cfg.Proton.BaseURL = "http://127.0.0.1:1"
	clk := &clock{now: t0}
	buf := &syncBuffer{}
	svc, err := service.NewLocal(service.Options{Dirs: dirs, Config: cfg, Now: clk.Now, Sleep: clk.Sleep,
		Logger: slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	sec, err := svc.AddClient(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dirs.State, "clients.json")
	if err := os.WriteFile(file, []byte(`{bad SECRET-CONTENT`), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 20 { // far more than the limiter's 5
		ok, wait := svc.CheckClient(t.Context(), sec.Password, "203.0.113.5:1")
		if ok || wait <= 0 {
			t.Fatalf("request %d: ok=%v wait=%v, want refused with a retry wait", i, ok, wait)
		}
	}
	logs := func() string { buf.mu.Lock(); defer buf.mu.Unlock(); return buf.b.String() }
	got := logs()
	if n := strings.Count(got, "clients.json cannot be loaded"); n != 1 {
		t.Fatalf("%d error lines for 20 requests, want 1:\n%s", n, got)
	}
	for _, want := range []string{"level=ERROR", file, "not valid JSON"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "SECRET-CONTENT") || strings.Contains(got, sec.Password) {
		t.Errorf("log leaks content:\n%s", got)
	}
	// Repaired: the same password passes at once, so nothing was charged.
	if err := os.WriteFile(file, []byte(`{"version":2,"clients":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, wait := svc.CheckClient(t.Context(), "wrong", "203.0.113.5:1")
	if ok || wait != 0 {
		t.Fatalf("after the repair: ok=%v wait=%v, want a plain refusal (the limiter was not charged)", ok, wait)
	}
	// A different failure is logged again.
	if err := os.WriteFile(file, []byte(`{"version":99,"clients":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.CheckClient(t.Context(), "x", "203.0.113.5:1") //nolint:errcheck // result not under test
	if n := strings.Count(logs(), "clients.json cannot be loaded"); n != 2 {
		t.Errorf("%d error lines after a second distinct failure, want 2", n)
	}
}

// A clients.json that cannot be loaded shows in the login status, which
// feeds the clients-file-unreadable alert of pcb status and the daemon's
// health (A12); a good or missing file shows nothing.
func TestStatusNamesAClientsFileThatCannotBeLoaded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(dirs)
	cfg.Proton.BaseURL = "http://127.0.0.1:1"
	svc, err := service.NewLocal(service.Options{Dirs: dirs, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	st, err := svc.Status(t.Context())
	if err != nil || st.ClientsFile != "" {
		t.Fatalf("no clients file: %+v, %v", st, err)
	}
	file := filepath.Join(dirs.State, "clients.json")
	if err := os.MkdirAll(dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{bad`), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err = svc.Status(t.Context()); err != nil || st.ClientsFile != file {
		t.Fatalf("corrupt clients file: ClientsFile %q, %v, want %q", st.ClientsFile, err, file)
	}
}

// pcb event list with no cache while logged out says to log in (pcb sync
// would only answer "not logged in"); once logged in it says to sync (A12).
func TestEventsWithNoCacheNameTheLoginWhenLoggedOut(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dirs := paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	cfg := config.Defaults(dirs)
	cfg.Proton.BaseURL = "http://127.0.0.1:1"
	svc, err := service.NewLocal(service.Options{Dirs: dirs, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	_, err = svc.Events(t.Context(), service.EventsRequest{Calendar: "Work"})
	wantCode(t, err, exitcode.MustAct)
	if h := exitcode.HintOf(err); h != "run pcb login" {
		t.Errorf("logged out: %v, hint %q, want run pcb login", err, h)
	}
	if err := store.SaveState(dirs.State, store.State{Phase: store.PhaseReady}); err != nil {
		t.Fatal(err)
	}
	_, err = svc.Events(t.Context(), service.EventsRequest{Calendar: "Work"})
	if h := exitcode.HintOf(err); h != "run pcb sync" {
		t.Errorf("logged in, no cache: %v, hint %q, want run pcb sync", err, h)
	}
}
