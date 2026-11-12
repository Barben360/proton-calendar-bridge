package service_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// loginUnattended logs in with unattended relogin (and the TOTP URI when the
// fake has 2FA).
func (e *env) loginUnattended(t *testing.T, twoFA bool) {
	t.Helper()
	r := e.req()
	r.UnattendedRelogin = true
	if twoFA {
		r.TOTP = e.f.TOTPURI
	}
	e.login(t, r)
}

// editAccount changes the sealed account in place.
func (e *env) editAccount(t *testing.T, fn func(a *store.Account)) {
	t.Helper()
	id := e.f.UserID(e.f.Username)
	err := e.secrets().Update(t.Context(), func(m map[string]store.Account) error {
		a := m[id]
		fn(&a)
		m[id] = a
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) files(t *testing.T) (state, sealed []byte) {
	t.Helper()
	state, err := os.ReadFile(filepath.Join(e.dirs.State, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if sealed, err = os.ReadFile(e.secrets().Path); err != nil {
		t.Fatal(err)
	}
	return state, sealed
}

func (e *env) phase(t *testing.T) string {
	t.Helper()
	s, err := e.svc.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return s.Phase
}

// logins counts the SRP attempts (POST /auth/v4/info and POST /auth/v4).
func (e *env) logins() int {
	return e.f.Calls("POST", "/auth/v4/info") + e.f.Calls("POST", "/auth/v4")
}

func TestExpiredAccessTokenRefreshesAndPersists(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.login(t, e.req())
	before := e.account(t)
	e.f.ExpireAccessTokens()
	if got := e.calendars(t); len(got) != 1 {
		t.Fatalf("calendars = %+v", got)
	}
	if n := e.f.Calls("POST", "/auth/v4/refresh"); n != 1 {
		t.Fatalf("%d refreshes, want 1", n)
	}
	after := e.account(t)
	if after.UID != before.UID || after.AccessToken == before.AccessToken || after.RefreshToken == before.RefreshToken {
		t.Fatal("the rotated tokens were not sealed")
	}
	// Another process starts from the sealed tokens: no refresh needed.
	other := e.open(t)
	if _, err := other.Calendars(t.Context()); err != nil {
		t.Fatalf("Calendars from a new service: %v", err)
	}
	if n := e.f.Calls("POST", "/auth/v4/refresh"); n != 1 {
		t.Errorf("%d refreshes after the restart, want still 1", n)
	}
}

// Two processes share the state directory and the expired token: the refresh
// runs once, under the secrets lock, and the second adopts its tokens.
func TestConcurrentRefreshAcrossProcessesRefreshesOnce(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.login(t, e.req())
	e.f.ExpireAccessTokens()
	a, b := e.open(t), e.open(t) // separate fds, so separate flocks
	// Hold the secrets lock until both have been refused with the old token,
	// so both reach the refresh with the same stale tokens.
	unlock, err := filelock.Lock(t.Context(), e.secrets().LockPath)
	if err != nil {
		t.Fatal(err)
	}
	users := e.f.Calls("GET", "/core/v4/users")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []*service.Local{a, b} {
		wg.Go(func() { _, errs[i] = s.Calendars(ctx) })
	}
	deadline := time.Now().Add(10 * time.Second)
	for e.f.Calls("GET", "/core/v4/users") < users+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let both answers (401) arrive
	unlock()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("process %d: %v", i, err)
		}
	}
	if n := e.f.Calls("POST", "/auth/v4/refresh"); n != 1 {
		t.Errorf("%d refreshes, want 1", n)
	}
	minted := e.f.AccessTokens()
	acc := e.account(t)
	if acc.AccessToken != minted[len(minted)-1] || !sessionAlive(t, e, tokensOf(acc)) {
		t.Error("the store does not hold the newest tokens")
	}
}

func TestTransientRefreshFailureKeepsSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.loginUnattended(t, false)
	e.clk.advance(2 * time.Minute) // a relogin would be allowed
	e.f.ExpireAccessTokens()
	e.f.FailNext("POST", "/auth/v4/refresh", 503, 0, 0)
	state, sealed := e.files(t)
	logins := e.logins()
	_, err := e.svc.Calendars(t.Context())
	_ = wantExit(t, err, exitcode.Blocked)
	if s, x := e.files(t); !bytes.Equal(s, state) || !bytes.Equal(x, sealed) {
		t.Error("state.json or secrets.sealed changed after a transient failure")
	}
	if n := e.logins() - logins; n != 0 {
		t.Errorf("%d login requests, want 0 (no relogin)", n)
	}
	if n := e.f.Calls("DELETE", "/auth/v4"); n != 0 {
		t.Errorf("%d logouts, want 0", n)
	}
}

func TestSessionEndedWithoutReloginNeedsLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.clk.advance(2 * time.Minute)
	e.f.RevokeSessions()
	logins := e.logins()
	_, err := e.svc.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Hint != "run pcb login" {
		t.Errorf("hint = %q, want run pcb login", ee.Hint)
	}
	if p := e.phase(t); p != "needs-login" {
		t.Errorf("phase = %q, want needs-login", p)
	}
	if n := e.logins() - logins; n != 0 {
		t.Errorf("%d login requests without unattended relogin", n)
	}
}

func TestUnattendedReloginRepairsSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.loginUnattended(t, true)
	old := e.account(t)
	e.f.RevokeSessions()
	e.clk.advance(61 * time.Second)
	at := e.clk.Now()
	srp := e.f.Calls("POST", "/auth/v4")
	if got := e.calendars(t); len(got) != 1 || got[0].Keys != "ok" {
		t.Fatalf("calendars = %+v", got)
	}
	if n := e.f.Calls("POST", "/auth/v4") - srp; n != 1 {
		t.Errorf("%d new POST /auth/v4, want 1", n)
	}
	// The ended session is not revoked: one refresh (the refused one), no logout.
	if r, d := e.f.Calls("POST", "/auth/v4/refresh"), e.f.Calls("DELETE", "/auth/v4"); r != 1 || d != 0 {
		t.Errorf("%d refreshes and %d logouts, want 1 and 0", r, d)
	}
	codes := e.f.TOTPCodes()
	if want := code(t, e.f.TOTPURI, at); len(codes) == 0 || codes[len(codes)-1] != want {
		t.Errorf("relogin 2FA code = %q, want the seed's code at Now", codes)
	}
	st := e.loadState(t)
	if st.Phase != store.PhaseReady || !st.LastRelogin.Equal(at) || !st.LastLogin.Equal(t0) || !st.Unattended {
		t.Errorf("state = %+v, want ready, relogin at %v, login at %v", st, at, t0)
	}
	acc := e.account(t)
	if acc.UID == old.UID || !sessionAlive(t, e, tokensOf(acc)) || acc.Relogin == nil {
		t.Error("the new session is not what is sealed, or the relogin credentials were dropped")
	}
}

func TestReloginStopsAfterHumanNeeded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, phase, hint string
		human             func(e *env, t *testing.T)
	}{
		{"captcha", "needs-captcha", "pcb login resume --captcha-token T9", func(e *env, _ *testing.T) { e.f.SetCaptcha("T9") }},
		{"changed password", "needs-login", "pcb login", func(e *env, t *testing.T) {
			e.editAccount(t, func(a *store.Account) { a.Relogin.Password = "wrong-" + a.Relogin.Password })
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, protontest.Options{})
			e.loginUnattended(t, false)
			e.f.RevokeSessions()
			c.human(e, t)
			e.clk.advance(61 * time.Second)
			srp := e.f.Calls("POST", "/auth/v4")
			_, err := e.svc.Calendars(t.Context())
			if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, c.hint) {
				t.Errorf("hint = %q, want %q", ee.Hint, c.hint)
			}
			if p := e.phase(t); p != c.phase {
				t.Errorf("phase = %q, want %q", p, c.phase)
			}
			e.clk.advance(2 * time.Minute)
			_, err = e.svc.Calendars(t.Context())
			if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Hint, c.hint) {
				t.Errorf("second call: hint = %q, want %q", ee.Hint, c.hint)
			}
			if n := e.f.Calls("POST", "/auth/v4") - srp; n != 1 {
				t.Errorf("%d login attempts, want exactly 1", n)
			}
		})
	}
}

func TestReloginThrottled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.loginUnattended(t, false)
	e.clk.advance(30 * time.Second)
	e.f.RevokeSessions()
	state, _ := e.files(t)
	logins := e.logins()
	_, err := e.svc.Calendars(t.Context())
	_ = wantExit(t, err, exitcode.Blocked)
	if n := e.logins() - logins; n != 0 {
		t.Errorf("%d login requests within the guard window", n)
	}
	if s, _ := e.files(t); !bytes.Equal(s, state) {
		t.Error("state.json changed after a throttled relogin")
	}
}

func TestChangedPasswordNeedsLogin(t *testing.T) {
	t.Parallel()
	const reason = "account keys no longer unlock (password changed?)"
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.editAccount(t, func(a *store.Account) { a.KeyPass = []byte("not-the-key-passphrase") })
	_, err := e.svc.Calendars(t.Context())
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Error(), reason) || !strings.Contains(ee.Hint, "pcb login") {
		t.Errorf("message %q hint %q", ee.Error(), ee.Hint)
	}
	s, serr := e.svc.Status(t.Context())
	if serr != nil || s.Phase != "needs-login" || s.Reason != reason {
		t.Errorf("status = %+v, %v", s, serr)
	}
}

// hook runs before every request of a service's API client.
type hook func(r *http.Request)

func (h hook) RoundTrip(r *http.Request) (*http.Response, error) {
	h(r)
	return http.DefaultTransport.RoundTrip(r)
}

// openHooked starts a service on the env's dirs whose API requests run before
// first.
func (e *env) openHooked(t *testing.T, before func(r *http.Request)) *service.Local {
	t.Helper()
	s, err := service.NewLocal(service.Options{
		Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
		HTTP: &http.Client{Timeout: 10 * time.Second, Transport: hook(before)},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A relogged session that ends again during the single retry marks the login
// needs-login: no further automatic relogin.
func TestReloggedSessionEndingAgainNeedsLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.loginUnattended(t, false)
	e.f.RevokeSessions()
	e.clk.advance(61 * time.Second)
	var once sync.Once
	s := e.openHooked(t, func(r *http.Request) {
		if r.URL.Path == "/calendar/v1" && e.f.Calls("POST", "/auth/v4") >= 2 { // after the relogin
			once.Do(e.f.RevokeSessions)
		}
	})
	srp := e.f.Calls("POST", "/auth/v4")
	_, err := s.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Hint != "run pcb login" {
		t.Errorf("hint = %q", ee.Hint)
	}
	if p := e.phase(t); p != "needs-login" {
		t.Errorf("phase = %q, want needs-login", p)
	}
	e.clk.advance(2 * time.Minute)
	_, err = s.Calendars(t.Context())
	_ = wantExit(t, err, exitcode.MustAct)
	if n := e.f.Calls("POST", "/auth/v4") - srp; n != 1 {
		t.Errorf("%d relogins, want exactly 1", n)
	}
}

// A needs-login decision never overwrites a login that finished meanwhile.
func TestNeedsLoginKeepsNewerLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.editAccount(t, func(a *store.Account) { a.KeyPass = []byte("not-the-key-passphrase") })
	var newer []byte
	var once sync.Once
	s := e.openHooked(t, func(r *http.Request) {
		if r.URL.Path != "/core/v4/addresses" {
			return
		}
		once.Do(func() { // a person's login finishes in another process
			st := e.loadState(t)
			st.LastLogin = t0.Add(time.Hour)
			if err := store.SaveState(e.dirs.State, st); err != nil {
				t.Error(err)
			}
			newer, _ = e.files(t)
		})
	})
	_, err := s.Calendars(t.Context())
	_ = wantExit(t, err, exitcode.MustAct)
	if st, _ := e.files(t); !bytes.Equal(st, newer) {
		t.Errorf("state.json was overwritten:\n%s\nwant\n%s", st, newer)
	}
}

// Another process relogs in while this one's request fails with the session
// ended: this one adopts the new session instead of logging in again.
func TestReloginAdoptsSessionOfAnotherProcess(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.loginUnattended(t, false)
	e.clk.advance(61 * time.Second)
	e.f.ExpireAccessTokens()
	other := e.open(t)
	srp := e.f.Calls("POST", "/auth/v4")
	users := 0
	var once sync.Once
	b := e.openHooked(t, func(r *http.Request) {
		if r.URL.Path != "/core/v4/users" {
			return
		}
		// The first request is refused (expired) and refreshed; before the
		// retry, the session ends and the other process relogs in.
		if users++; users == 2 {
			once.Do(func() {
				e.f.RevokeSessions()
				if _, err := other.Calendars(t.Context()); err != nil {
					t.Errorf("the other process: %v", err)
				}
			})
		}
	})
	got, err := b.Calendars(t.Context())
	if err != nil || len(got) != 1 {
		t.Fatalf("Calendars = %+v, %v", got, err)
	}
	if n := e.f.Calls("POST", "/auth/v4") - srp; n != 1 {
		t.Errorf("%d logins, want 1 (the other process's relogin only)", n)
	}
	if !sessionAlive(t, e, tokensOf(e.account(t))) {
		t.Error("the sealed session is not alive")
	}
}

// The loop's limiter and refreshed tokens live in the client: every call on
// the same stored session must get the same one.
func TestSessionClientIsReused(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.login(t, e.req())
	c1, err := e.svc.SessionClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	e.f.ExpireAccessTokens()
	for range 2 {
		if got := e.calendars(t); len(got) != 1 {
			t.Fatalf("calendars = %+v", got)
		}
	}
	if n := e.f.Calls("POST", "/auth/v4/refresh"); n != 1 {
		t.Errorf("%d refreshes, want 1", n)
	}
	c2, err := e.svc.SessionClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatal("a second call built a new client")
	}
	if got, want := c2.Tokens(), tokensOf(e.account(t)); got != want {
		t.Errorf("client tokens differ from the sealed ones")
	}
}

// Another process rotated and sealed the tokens: the reused client adopts
// them rather than spending its own stale refresh token.
func TestSessionClientAdoptsTokensSealedMeanwhile(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.login(t, e.req())
	c1, err := e.svc.SessionClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	stale := c1.Tokens()
	e.f.ExpireAccessTokens()
	if _, err := e.open(t).Calendars(t.Context()); err != nil {
		t.Fatal(err)
	}
	sealed := tokensOf(e.account(t))
	if sealed == stale {
		t.Fatal("the other process did not rotate the tokens")
	}
	c2, err := e.svc.SessionClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if c2 != c1 {
		t.Error("the client was replaced")
	}
	if c2.Tokens() != sealed {
		t.Error("the reused client kept its stale tokens")
	}
	if got := e.calendars(t); len(got) != 1 {
		t.Fatalf("calendars = %+v", got)
	}
	if n := e.f.Calls("POST", "/auth/v4/refresh"); n != 1 {
		t.Errorf("%d refreshes, want 1 (the other process's)", n)
	}
}

// A new login is a new session (new UID): the client of the old one is gone.
func TestReloginDropsClient(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.login(t, e.req())
	c1, err := e.svc.SessionClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	oldUID := c1.Tokens().UID
	e.clk.advance(time.Hour) // past the login guard
	e.login(t, e.req())
	newUID := e.account(t).UID
	if newUID == oldUID {
		t.Fatal("the second login kept the session")
	}
	c2, err := e.svc.SessionClient(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if c2 == c1 || c2.Tokens().UID != newUID {
		t.Errorf("client uid = %q, want a new client on %q", c2.Tokens().UID, newUID)
	}
	if got := e.calendars(t); len(got) != 1 {
		t.Fatalf("calendars = %+v", got)
	}
}

// Logout and Close each drop the cached client directly.
func TestLogoutDropsClient(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	if _, err := e.svc.SessionClient(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !e.svc.HasCachedClient() {
		t.Fatal("no client cached after a session call")
	}
	if _, err := e.svc.Logout(t.Context()); err != nil {
		t.Fatal(err)
	}
	if e.svc.HasCachedClient() {
		t.Error("the client survived a logout")
	}
}

func TestCloseDropsClient(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	if _, err := e.svc.SessionClient(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Close(); err != nil {
		t.Fatal(err)
	}
	if e.svc.HasCachedClient() {
		t.Error("the client survived Close")
	}
}

// A login that finishes in another process while needs-login is being
// written is kept: the decision and the write are one step under state.lock.
func TestHumanNeededDoesNotOverwriteAFinishedLogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file locks are Unix only")
	}
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.clk.advance(2 * time.Minute)
	e.f.RevokeSessions()
	newer := e.loadState(t)
	newer.LastLogin = t0.Add(time.Hour) // a person's login, in another process
	statePath := filepath.Join(e.dirs.State, "state.json")
	var landed chan error
	e.svc.OnHumanNeeded(func() {
		// needs-login is decided; the other process writes state.json now.
		landed = make(chan error, 1)
		unlock, ok, err := filelock.TryLock(filepath.Join(e.dirs.State, "state.lock"))
		switch {
		case err != nil:
			landed <- err
		case ok: // nothing holds state.lock between the decision and the write
			defer unlock()
			raw, err := json.MarshalIndent(newer, "", "  ")
			if err == nil {
				err = os.WriteFile(statePath, raw, 0o600)
			}
			landed <- err
		default: // it waits for the lock, as a login's SaveState does
			go func() { landed <- store.SaveState(e.dirs.State, newer) }()
		}
	})
	_, err := e.svc.Calendars(t.Context())
	_ = wantExit(t, err, exitcode.MustAct)
	if landed == nil {
		t.Fatal("needs-login was never decided")
	}
	if err := <-landed; err != nil {
		t.Fatal(err)
	}
	if st := e.loadState(t); st.Phase != store.PhaseReady || !st.LastLogin.Equal(newer.LastLogin) {
		t.Errorf("state = %+v, want the finished login kept", st)
	}
}

// A state.lock held past its timeout is reported as the lock, not as an
// unreadable state.json, and nothing is written.
func TestHumanNeededReportsAHeldStateLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file locks are Unix only")
	}
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.clk.advance(2 * time.Minute)
	e.f.RevokeSessions()
	before, _ := e.files(t)
	unlock, ok, err := filelock.TryLock(filepath.Join(e.dirs.State, "state.lock"))
	if err != nil || !ok {
		t.Fatalf("TryLock = %v, %v", ok, err)
	}
	defer unlock()
	e.svc.SetStateTimeout(50 * time.Millisecond)
	_, err = e.svc.Calendars(t.Context())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Error() != "another pcb process holds the login state file's lock (state.lock) too long" ||
		ee.Hint != "run the command again; if it persists, look for a stuck pcb process" {
		t.Errorf("message %q hint %q", ee.Error(), ee.Hint)
	}
	if after, _ := e.files(t); !bytes.Equal(before, after) {
		t.Errorf("state.json written without the lock:\n%s", after)
	}
}

// afterRoundTrip runs after each response, before the client reads it.
type afterRoundTrip func(resp *http.Response)

func (a afterRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		a(resp)
	}
	return resp, err
}

// The needs-login mark is durable: a request cancelled once the session is
// known to have ended still writes it.
func TestHumanNeededMarksEvenWhenTheRequestIsCancelled(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{})
	e.login(t, e.req())
	e.clk.advance(2 * time.Minute)
	e.f.RevokeSessions()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s, err := service.NewLocal(service.Options{
		Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep,
		HTTP: &http.Client{Timeout: 10 * time.Second, Transport: afterRoundTrip(func(resp *http.Response) {
			if resp.StatusCode == http.StatusUnauthorized { // Proton has answered that the session ended
				cancel()
			}
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_, err = s.Calendars(ctx)
	_ = wantExit(t, err, exitcode.MustAct)
	if ctx.Err() == nil {
		t.Fatal("the request was not cancelled: the test proves nothing")
	}
	if st := e.loadState(t); st.Phase != store.PhaseNeedsLogin || st.Reason != "the Proton session has ended" {
		t.Errorf("state = %+v, want needs-login", st)
	}
}
