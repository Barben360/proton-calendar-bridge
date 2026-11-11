package service_test

import (
	"bytes"
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

func TestCheckClientRecordsLastUseAtMostOncePerMinute(t *testing.T) {
	t.Parallel()
	e := newClientsEnv(t)
	sec, _ := e.svc.AddClient(t.Context(), "Phone")
	e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1") //nolint:errcheck // result not under test
	first := e.clk.Now()
	list, _ := e.svc.Clients(t.Context())
	if list[0].LastUsed == nil || !list[0].LastUsed.Equal(first) {
		t.Fatalf("LastUsed = %v, want %v", list[0].LastUsed, first)
	}
	e.clk.advance(30 * time.Second)
	e.svc.CheckClient(t.Context(), sec.Password, "127.0.0.1:1") //nolint:errcheck // result not under test
	if list, _ := e.svc.Clients(t.Context()); !list[0].LastUsed.Equal(first) {
		t.Errorf("LastUsed moved within a minute: %v", list[0].LastUsed)
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
