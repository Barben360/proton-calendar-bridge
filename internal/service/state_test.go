package service_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store/cache"
)

// A state.json a newer pcb wrote stays refused, and every command says to
// upgrade pcb, never to log out (state-I2): a logout revokes the session for
// a file that is only newer.
func TestNewerStateSaysUpgrade(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.signIn(t)
	path := filepath.Join(e.dirs.State, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(string(raw), "{", `{"added_by_a_newer_pcb": true,`, 1)
	if err := os.WriteFile(path, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	_, statusErr := e.svc.Status(t.Context())
	_, loginErr := e.svc.Login(t.Context(), e.req())
	_, syncErr := e.svc.Sync(t.Context(), service.SyncRequest{})
	for name, err := range map[string]error{"status": statusErr, "login": loginErr, "sync": syncErr} {
		ee := wantExit(t, err, exitcode.MustAct)
		if ee.Error() != "state.json was written by a newer pcb" || ee.Hint != "upgrade pcb" {
			t.Errorf("%s: %q hint %q", name, ee.Error(), ee.Hint)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != newer {
		t.Error("state.json was rewritten")
	}
}

// A login over a store this pcb cannot read says to upgrade pcb (exit 10),
// not to log out (state-I1: the hint loop is gone).
func TestLoginOverAStoreThisPcbCannotRead(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.signIn(t)
	if err := os.WriteFile(e.secrets().Path, []byte(`{"version":3,"sealer":"keyfile","data":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Login(t.Context(), e.req())
	ee := wantExit(t, err, exitcode.MustAct)
	if ee.Error() != "secrets.sealed was written by a newer pcb" || ee.Hint != "upgrade pcb" {
		t.Errorf("login: %q hint %q", ee.Error(), ee.Hint)
	}
}

// A logout while another process holds the sync lock (pcb sync, its watch,
// pcb serve) still logs out, and says that a pass of it may write the event
// cache again (state Minor); with the lock free it says nothing.
func TestLogoutSaysWhenAnotherProcessSyncs(t *testing.T) {
	t.Parallel()
	for _, held := range []bool{false, true} {
		e := newEnv(t, protontest.Options{})
		e.signIn(t)
		if _, err := e.svc.Sync(t.Context(), service.SyncRequest{}); err != nil {
			t.Fatal(err)
		}
		if held {
			unlock, err := filelock.Lock(t.Context(), filepath.Join(e.dirs.State, "sync.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
		}
		res, err := e.svc.Logout(t.Context(), service.LogoutRequest{})
		if err != nil || !res.WasLoggedIn || res.SyncRunning != held {
			t.Errorf("held %v: Logout = %+v, %v; want SyncRunning %v", held, res, err, held)
		}
		if _, err := os.Stat(cache.Path(e.dirs.State)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("held %v: pcb.db after the logout: %v", held, err)
		}
	}
}

// Opening the service removes the temporary files a crash left, older than
// an hour, in the state, config and key file directories (state Minor).
func TestOpenRemovesStaleTemporaryFiles(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	old := t0.Add(-2 * time.Hour)
	var planted []string
	for _, p := range []string{
		filepath.Join(e.dirs.State, ".tmp-1"),
		filepath.Join(e.dirs.Config, ".tmp-2"),
		filepath.Join(filepath.Dir(e.cfg.Secrets.KeyFile), ".key-3"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		planted = append(planted, p)
	}
	_ = e.open(t)
	for _, p := range planted {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after the open: %v, want removed", p, err)
		}
	}
}

// The key file's directory may be shared (a home directory): only .key-*
// is swept there, never .tmp-* (another tool's file).
func TestOpenSweepsOnlyKeyFilesInTheKeyDirectory(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	keyDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.cfg.Secrets.KeyFile = filepath.Join(keyDir, "pcb.key")
	old := t0.Add(-2 * time.Hour)
	for _, name := range []string{".tmp-other-tool", ".key-1"} {
		p := filepath.Join(keyDir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	_ = e.open(t)
	if _, err := os.Stat(filepath.Join(keyDir, ".tmp-other-tool")); err != nil {
		t.Errorf("a .tmp-* file in the key file's directory was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(keyDir, ".key-1")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".key-1 after the open: %v, want removed", err)
	}
}

// The refusal to discard a store this pcb cannot read touches nothing: a
// login waiting for its 2FA code is still waiting after it (state-I1).
func TestLogoutRefusalKeepsAPendingLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	res, err := e.svc.Login(t.Context(), e.req())
	if err != nil || res.State != service.StateNeedsTOTP {
		t.Fatalf("Login = %+v, %v; want needs-totp", res, err)
	}
	if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.secrets().Path, []byte(`{"version":3,"sealer":"keyfile","data":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Logout(t.Context(), service.LogoutRequest{})
	if ee := wantExit(t, err, exitcode.MustAct); ee.Hint != "end the session in the Proton web app (Settings → Security → Sessions), then run pcb logout --yes" {
		t.Errorf("refusal hint = %q", ee.Hint)
	}
	st, err := e.svc.Status(t.Context())
	if err != nil || st.Phase != string(service.StateNeedsTOTP) || st.LoginExpires == nil {
		t.Errorf("status after the refusal = %+v, %v; want the login still waiting for its code", st, err)
	}
}
