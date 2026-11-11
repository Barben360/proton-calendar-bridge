package service_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
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
	if _, err := e.svc.Logout(t.Context()); err != nil {
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
