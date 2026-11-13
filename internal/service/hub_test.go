package service_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/filelock"
	"github.com/Barben360/proton-calendar-bridge/internal/loginguard"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/api"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// The fake's users of the Hub tests, and the password of b (a's is the
// fake's own).
const (
	userA     = "a@x"
	userB     = "b@x"
	userC     = "c@x"
	passwordB = "hub-test-password-b"
	// aliasA is another address of a's Proton user: SkipSRP signs it in
	// as a.
	aliasA = "second-a@x"
)

type hubEnv struct {
	f    *protontest.Fake
	clk  *clock
	dirs paths.Dirs
	cfg  config.Config

	amu    sync.Mutex
	expiry []func() // the pending logins' expiry timers, fired by hand
	// sleep, when set before open, is the services' Sleep (else the
	// clock's).
	sleep func(ctx context.Context, d time.Duration) error
	// sessions records the tokens SkipSRP handed out, by username.
	smu      sync.Mutex
	sessions map[string]api.Tokens
	// http, when set before open, is the services' HTTP client.
	http *http.Client
}

func newHubEnv(t *testing.T) *hubEnv {
	t.Helper()
	e := &hubEnv{clk: &clock{now: t0}, sessions: map[string]api.Tokens{}}
	e.f = protontest.New(t, protontest.Options{Now: e.clk.Now, Username: userA})
	e.f.AddUser(userB, passwordB)
	e.f.AddUser(userC, passwordB)
	root := t.TempDir()
	e.dirs = paths.Dirs{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state")}
	e.cfg = config.Defaults(e.dirs)
	e.cfg.Proton.BaseURL = e.f.URL
	return e
}

// afterFunc records each expiry timer instead of arming it.
func (e *hubEnv) afterFunc(_ time.Duration, f func()) func() bool {
	e.amu.Lock()
	defer e.amu.Unlock()
	e.expiry = append(e.expiry, f)
	return func() bool { return true }
}

// expireAll fires every recorded expiry timer, as 5 minutes passing would.
func (e *hubEnv) expireAll() {
	e.amu.Lock()
	fs := slices.Clone(e.expiry)
	e.amu.Unlock()
	for _, f := range fs {
		f()
	}
}

func (e *hubEnv) options() service.HubOptions {
	sleep := e.clk.Sleep
	if e.sleep != nil {
		sleep = e.sleep
	}
	return service.HubOptions{Options: service.Options{
		Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: sleep, AfterFunc: e.afterFunc, HTTP: e.http,
	}}
}

// open opens a Hub on the env's state, closed with the test.
func (e *hubEnv) open(t *testing.T) *service.Hub {
	t.Helper()
	h, err := service.OpenHub(e.options())
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// skip makes h's logins take their session from the fake's pool instead of
// SRP; aliasA signs in as a's user.
func (e *hubEnv) skip(t *testing.T, h *service.Hub) {
	t.Helper()
	h.SkipSRP(func(username string) (api.Tokens, string) {
		u := strings.ToLower(username)
		if u == aliasA {
			u = userA
		}
		tok := e.f.Login(t, u)
		e.smu.Lock()
		e.sessions[strings.ToLower(username)] = tok
		e.smu.Unlock()
		return tok, e.f.UserID(u)
	})
}

func (e *hubEnv) session(username string) api.Tokens {
	e.smu.Lock()
	defer e.smu.Unlock()
	return e.sessions[username]
}

func (e *hubEnv) password(username string) string {
	if strings.EqualFold(username, userA) || username == aliasA {
		return e.f.Password
	}
	return passwordB
}

// login logs in as username on h after the login guard's interval.
func (e *hubEnv) login(t *testing.T, h *service.Hub, username string) (service.LoginResult, error) {
	t.Helper()
	e.clk.advance(loginguard.MinInterval + time.Second)
	return h.Login(t.Context(), service.LoginRequest{Username: username, Password: e.password(username)})
}

func (e *hubEnv) mustLogin(t *testing.T, h *service.Hub, username string) service.LoginResult {
	t.Helper()
	res, err := e.login(t, h, username)
	if err != nil {
		t.Fatalf("Login(%s): %v", username, err)
	}
	return res
}

// accountDirs lists the directories under accounts/.
func (e *hubEnv) accountDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(store.AccountsDir(e.dirs.State))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		if en.IsDir() {
			out = append(out, en.Name())
		}
	}
	return out
}

func status(t *testing.T, h *service.Hub, selector string) service.Status {
	t.Helper()
	a, _, err := h.Account(selector)
	if err != nil {
		t.Fatalf("Account(%q): %v", selector, err)
	}
	st, err := a.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func ids(list []service.AccountInfo) []string {
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	slices.Sort(out)
	return out
}

// writeM5 makes an M5 state at the root: a Local on it logs in as a, which
// creates its default CalDAV client. It returns that client's password.
func (e *hubEnv) writeM5(t *testing.T) string {
	t.Helper()
	l, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	l.SkipSRP(func(username string) (api.Tokens, string) { return e.f.Login(t, username), e.f.UserID(username) })
	if _, err := l.Login(t.Context(), service.LoginRequest{Username: userA, Password: e.f.Password}); err != nil {
		t.Fatal(err)
	}
	creds, err := l.ShowClient(t.Context(), "")
	if err != nil || creds.Password == "" {
		t.Fatalf("ShowClient = %v, %v", creds.Password != "", err)
	}
	return creds.Password
}

func TestOpenHubMigratesM5State(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	pw := e.writeM5(t)
	h := e.open(t)
	list := h.Accounts()
	if len(list) != 1 || list[0].Address != userA || list[0].Phase != string(service.StateReady) || list[0].Pending || !list[0].CalDAV {
		t.Fatalf("Accounts = %+v, want a, ready, CalDAV on", list)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{list[0].ID}) {
		t.Fatalf("accounts/ = %v, want [%s]", got, list[0].ID)
	}
	if _, err := os.Stat(filepath.Join(e.dirs.State, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("root state.json still there: %v", err)
	}
	a, _, err := h.Account("")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := a.Status(t.Context()); err != nil || st.Phase != string(service.StateReady) || st.Secrets != "sealed" {
		t.Fatalf("Status = %+v, %v, want ready and sealed", st, err)
	}
	if ok, _ := a.CheckClient(t.Context(), pw, "192.0.2.1:1"); !ok {
		t.Error("the M5 CalDAV password no longer verifies")
	}
}

func TestOpenHubRefusesWhileM5SyncLockHeld(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	if err := store.SaveState(e.dirs.State, store.State{Phase: store.PhaseLoggedOut, Address: userA}); err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Lock(t.Context(), filepath.Join(e.dirs.State, "sync.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = service.OpenHub(e.options())
	ee := wantExit(t, err, exitcode.Blocked)
	if ee.Error() != "another pcb holds the state" || ee.Hint != "stop the running pcb first" {
		t.Errorf("error = %q, hint %q", ee.Error(), ee.Hint)
	}
	if _, err := os.Stat(filepath.Join(e.dirs.State, "state.json")); err != nil {
		t.Errorf("the M5 state moved while its lock was held: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.dirs.State, "layout.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("layout.json written while the lock was held: %v", err)
	}
}

// With layout v2 the root sync.lock is the serving process's: a Hub opens
// while it is held (ruling R2).
func TestOpenHubIgnoresSyncLockOnLayoutV2(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	_ = e.open(t).Close()
	unlock, err := filelock.Lock(t.Context(), filepath.Join(e.dirs.State, "sync.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	h, err := service.OpenHub(e.options())
	if err != nil {
		t.Fatalf("OpenHub on layout v2 with the root sync.lock held: %v", err)
	}
	_ = h.Close()
}

func TestOpenHubRefusesLegacyBesideLayout(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	_ = e.open(t).Close()
	if err := store.SaveState(e.dirs.State, store.State{Phase: store.PhaseLoggedOut}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dirs.State, "secrets.sealed"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Ruling R36: a person must act (10), so a unit that does not restart
	// on 10 stops instead of looping.
	_, err := service.OpenHub(e.options())
	ee := wantExit(t, err, exitcode.MustAct)
	if msg := ee.Error(); !strings.Contains(msg, "state.json, secrets.sealed") || !strings.Contains(msg, e.dirs.State) {
		t.Errorf("message %q does not name the files and the directory", msg)
	}
	if ee.Hint != "remove them or move the state directory aside: this pcb version cannot be downgraded" {
		t.Errorf("hint = %q", ee.Hint)
	}
}

func TestOpenHubRefusesNewerLayout(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dirs.State, "layout.json"), []byte(`{"version": 3}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := service.OpenHub(e.options())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Hint == "" || !strings.Contains(ee.Error(), "newer pcb") {
		t.Errorf("error = %q, hint %q, want a newer pcb named and a hint", ee.Error(), ee.Hint)
	}
}

// Ruling R36: a damaged layout.json needs a person (10), as a damaged
// state.json does.
func TestOpenHubRefusesDamagedLayout(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dirs.State, "layout.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := service.OpenHub(e.options())
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "damaged") || ee.Hint != "run pcb doctor" {
		t.Errorf("error = %q, hint %q, want the damaged layout and pcb doctor", ee.Error(), ee.Hint)
	}
}

// Ruling R36: a layout.json pcb cannot read (permissions) needs a person
// too: no restart fixes it.
func TestOpenHubRefusesUnreadableLayout(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("file modes do not stop root, nor apply on Windows")
	}
	e := newHubEnv(t)
	_ = e.open(t).Close()
	path := filepath.Join(e.dirs.State, "layout.json")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	_, err := service.OpenHub(e.options())
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), "cannot use the state layout") || !strings.Contains(ee.Error(), path) {
		t.Errorf("error = %q, want the unreadable layout.json named", ee.Error())
	}
}

// Ruling R36: another pcb moving the state (layout.lock held) may pass by
// itself: Blocked (20), unlike the layout errors.
func TestOpenHubRefusesWhileLayoutLockHeld(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	if err := os.MkdirAll(e.dirs.State, 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := filelock.Lock(t.Context(), filepath.Join(e.dirs.State, "layout.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = service.OpenHub(e.options())
	if ee := wantExit(t, err, exitcode.Blocked); !strings.Contains(ee.Error(), "another pcb is moving the state") {
		t.Errorf("error = %q, want another pcb moving the state", ee.Error())
	}
}

func TestLoginAddsSecondAccount(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	rb := e.mustLogin(t, h, userB)
	if ra.State != service.StateReady || rb.State != service.StateReady {
		t.Fatalf("states = %q, %q, want ready", ra.State, rb.State)
	}
	if ra.Account == "" || ra.Account == rb.Account || !store.ValidLocalID(ra.Account) || !store.ValidLocalID(rb.Account) {
		t.Fatalf("local IDs = %q, %q, want two distinct ones", ra.Account, rb.Account)
	}
	if got, want := ids(h.Accounts()), ids([]service.AccountInfo{{ID: ra.Account}, {ID: rb.Account}}); !slices.Equal(got, want) {
		t.Fatalf("Accounts = %v, want %v", got, want)
	}
	if got := e.accountDirs(t); len(got) != 2 {
		t.Fatalf("accounts/ = %v, want two directories", got)
	}
	for id, want := range map[string]string{ra.Account: userA, rb.Account: userB} {
		if st := status(t, h, id); st.Address != want || st.Phase != string(service.StateReady) {
			t.Errorf("account %s: address %q phase %q, want %q ready", id, st.Address, st.Phase, want)
		}
	}
	list := h.Accounts()
	if list[0].Address != userA || list[1].Address != userB {
		t.Errorf("Accounts not sorted by address: %+v", list)
	}
	// One login guard for every account, at the root.
	if _, err := os.Stat(filepath.Join(e.dirs.State, "login.lock")); err != nil {
		t.Errorf("no login guard at the root: %v", err)
	}
	for _, id := range []string{ra.Account, rb.Account} {
		if _, err := os.Stat(filepath.Join(store.AccountDir(e.dirs.State, id), "login.lock")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("account %s has its own login guard: %v", id, err)
		}
	}
}

func TestHubSharesTheLimiter(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	e.mustLogin(t, h, userA)
	e.mustLogin(t, h, userB)
	a, _, err := h.Account(userA)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := h.Account(userB)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := b.AddClient(t.Context(), "Phone")
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if ok, _ := a.CheckClient(t.Context(), "wrong", "203.0.113.5:1"); ok {
			t.Fatal("a wrong password was accepted")
		}
	}
	if ok, retry := b.CheckClient(t.Context(), sec.Password, "203.0.113.5:2"); ok || retry <= 0 {
		t.Errorf("b after 5 failures on a: ok=%v retry=%v, want refused with a wait", ok, retry)
	}
}

// The 2FA path of an add: the account joins once its code is in (ruling R10).
func TestLoginAddsSecondAccountWithTOTP(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	uri := e.f.SetTwoFA(userB, true)
	h.SkipSRP(nil) // SkipSRP opens sessions without 2FA
	rb := e.mustLogin(t, h, userB)
	if rb.State != service.StateNeedsTOTP || rb.Account == "" || rb.Account == ra.Account {
		t.Fatalf("Login(b) = %+v, want needs-totp on a new account", rb)
	}
	if info := find(h.Accounts(), rb.Account); info == nil || !info.Pending || info.Address != userB || info.Phase != string(service.StateNeedsTOTP) {
		t.Fatalf("b while its code is awaited = %+v, want pending, needs-totp", info)
	}
	res, err := h.SubmitTOTP(t.Context(), rb.Account, code(t, uri, e.clk.Now()))
	if err != nil {
		t.Fatalf("SubmitTOTP: %v", err)
	}
	if res.State != service.StateReady || res.Account != rb.Account {
		t.Fatalf("SubmitTOTP = %+v, want ready on %s", res, rb.Account)
	}
	if info := find(h.Accounts(), rb.Account); info == nil || info.Pending || info.Phase != string(service.StateReady) {
		t.Fatalf("b after its code = %+v, want ready, not pending", info)
	}
	if st := status(t, h, userB); st.Address != userB {
		t.Errorf("b's address = %q", st.Address)
	}
}

func TestSubmitTOTPWrongCodesLeaveNothing(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	uri := e.f.SetTwoFA(userB, true)
	h.SkipSRP(nil)
	rb := e.mustLogin(t, h, userB)
	wrong := wrongCode(t, uri, e.clk.Now())
	for try := 1; try <= 3; try++ {
		_, err := h.SubmitTOTP(t.Context(), userB, wrong)
		_ = wantExit(t, err, exitcode.MustAct)
		dirs := e.accountDirs(t)
		if try < 3 && !slices.Contains(dirs, rb.Account) {
			t.Fatalf("after %d wrong codes b's directory is gone, want it kept for the next try", try)
		}
		if try == 3 && !slices.Equal(dirs, []string{ra.Account}) {
			t.Fatalf("after 3 wrong codes accounts/ = %v, want only a's %s", dirs, ra.Account)
		}
	}
	if got := ids(h.Accounts()); !slices.Equal(got, []string{ra.Account}) {
		t.Errorf("Accounts = %v, want only a", got)
	}
}

func wrongCode(t *testing.T, uri string, at time.Time) string {
	t.Helper()
	right := map[string]bool{code(t, uri, at): true, code(t, uri, at.Add(-30*time.Second)): true, code(t, uri, at.Add(30*time.Second)): true}
	for _, c := range []string{"000000", "111111", "222222", "333333"} {
		if !right[c] {
			return c
		}
	}
	t.Fatal("no wrong code")
	return ""
}

func find(list []service.AccountInfo, id string) *service.AccountInfo {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func TestLoginExistingAddressRelogsThatAccount(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	if _, err := h.SignOut(t.Context(), userA, service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	if info := find(h.Accounts(), ra.Account); info == nil || info.Phase != string(store.PhaseLoggedOut) || info.Pending || info.Address != userA {
		t.Fatalf("a after sign out = %+v, want listed, logged-out, not pending", info)
	}
	again := e.mustLogin(t, h, strings.ToUpper(userA))
	if again.Account != ra.Account {
		t.Fatalf("relogin went to %q, want %q", again.Account, ra.Account)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{ra.Account}) {
		t.Fatalf("accounts/ = %v, want only %s", got, ra.Account)
	}
	if st := status(t, h, ra.Account); st.Phase != string(service.StateReady) {
		t.Errorf("phase = %q, want ready", st.Phase)
	}
}

func TestFailedAddLeavesNothing(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	h.SkipSRP(nil) // the password is checked by SRP
	e.clk.advance(loginguard.MinInterval + time.Second)
	_, err := h.Login(t.Context(), service.LoginRequest{Username: userC, Password: "not-the-password"})
	_ = wantExit(t, err, exitcode.MustAct)
	if got := e.accountDirs(t); !slices.Equal(got, []string{ra.Account}) {
		t.Fatalf("accounts/ = %v, want only a's %s", got, ra.Account)
	}
	if got := ids(h.Accounts()); !slices.Equal(got, []string{ra.Account}) {
		t.Errorf("Accounts = %v, want only a", got)
	}
}

func TestCaptchaAddIsPending(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	e.f.SetCaptchaFor(userB, "tok")
	h.SkipSRP(nil) // SkipSRP skips the CAPTCHA too
	rb := e.mustLogin(t, h, userB)
	if rb.State != service.StateNeedsCaptcha || rb.Account == "" || rb.Account == ra.Account {
		t.Fatalf("Login(b) = %+v, want needs-captcha on a new account", rb)
	}
	info := find(h.Accounts(), rb.Account)
	if info == nil || !info.Pending || info.Address != userB || info.Phase != string(store.PhaseNeedsCaptcha) {
		t.Fatalf("b = %+v, want pending, needs-captcha", info)
	}
	if a := find(h.Accounts(), ra.Account); a == nil || a.Pending {
		t.Fatalf("a = %+v, want not pending", a)
	}
	_, got, err := h.Account("")
	if err != nil || got.ID != ra.Account {
		t.Fatalf(`Account("") = %+v, %v, want a (b is pending)`, got, err)
	}
	if _, got, err := h.Account(userB); err != nil || got.ID != rb.Account {
		t.Errorf("Account(b) = %+v, %v, want the pending b", got, err)
	}
}

func TestLoginSameAddressReusesPendingAccount(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.f.SetTwoFA(userB, true)
	first := e.mustLogin(t, h, userB)
	second := e.mustLogin(t, h, userB)
	if first.State != service.StateNeedsTOTP || second.State != service.StateNeedsTOTP {
		t.Fatalf("states = %q, %q, want needs-totp twice", first.State, second.State)
	}
	if second.Account != first.Account {
		t.Fatalf("second login on %q, want the pending %q", second.Account, first.Account)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{first.Account}) {
		t.Fatalf("accounts/ = %v, want one directory", got)
	}
}

func TestPendingExpiresAfter24h(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.f.SetCaptchaFor(userB, "tok")
	rb := e.mustLogin(t, h, userB)
	if rb.State != service.StateNeedsCaptcha {
		t.Fatalf("state = %q", rb.State)
	}
	created := e.clk.Now()
	_ = h.Close()

	e.clk.set(created.Add(23 * time.Hour))
	h = e.open(t)
	if find(h.Accounts(), rb.Account) == nil || !slices.Equal(e.accountDirs(t), []string{rb.Account}) {
		t.Fatalf("pending b gone after 23 h; accounts/ = %v", e.accountDirs(t))
	}
	_ = h.Close()

	e.clk.set(created.Add(24*time.Hour + time.Second))
	h = e.open(t)
	if len(h.Accounts()) != 0 || len(e.accountDirs(t)) != 0 {
		t.Fatalf("pending b kept after 24 h: %+v, accounts/ = %v", h.Accounts(), e.accountDirs(t))
	}
}

// A signed-out account whose relogin met a CAPTCHA is not a pending add
// (it lost its Proton user ID with the sign-out, not its history): it is
// counted by the selector and never expires.
func TestSignedOutAccountStoppedByCaptchaDoesNotExpire(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	if _, err := h.SignOut(t.Context(), ra.Account, service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	e.f.SetCaptchaFor(userA, "tok")
	h.SkipSRP(nil) // SkipSRP skips the CAPTCHA too
	if res := e.mustLogin(t, h, userA); res.State != service.StateNeedsCaptcha || res.Account != ra.Account {
		t.Fatalf("relogin = %+v, want needs-captcha on %s", res, ra.Account)
	}
	if info := find(h.Accounts(), ra.Account); info == nil || info.Pending {
		t.Fatalf("a = %+v, want listed, not pending", info)
	}
	_ = h.Close()
	e.clk.advance(48 * time.Hour)
	h = e.open(t)
	if find(h.Accounts(), ra.Account) == nil {
		t.Fatalf("signed-out a gone after 48 h; accounts/ = %v", e.accountDirs(t))
	}
	if _, got, err := h.Account(""); err != nil || got.ID != ra.Account {
		t.Errorf(`Account("") = %+v, %v, want a`, got, err)
	}
}

func TestSameUserOtherAddressIsRefused(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	_, err := e.login(t, h, aliasA)
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Error(), userA) {
		t.Errorf("message %q does not name %s", ee.Error(), userA)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{ra.Account}) {
		t.Fatalf("accounts/ = %v, want only a's %s", got, ra.Account)
	}
	// Ruling R12: the session the refused login opened is revoked.
	if tok := e.session(aliasA); tok.AccessToken == "" || sessionAliveOn(t, e.f.URL, tok) {
		t.Errorf("the refused login's session is still alive (tokens recorded: %t)", tok.AccessToken != "")
	}
	if !sessionAliveOn(t, e.f.URL, e.session(userA)) {
		t.Error("a's own session was revoked")
	}
	if st := status(t, h, ra.Account); st.Phase != string(service.StateReady) {
		t.Errorf("a's phase = %q, want ready", st.Phase)
	}
}

// sessionAliveOn reports whether tok still authenticates on the fake at url.
func sessionAliveOn(t *testing.T, url string, tok api.Tokens) bool {
	t.Helper()
	c := api.New(tok, api.Options{BaseURL: url, HTTP: &http.Client{Timeout: 5 * time.Second}})
	return c.Get(t.Context(), "/core/v4/users", nil, nil) == nil
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

func TestSelectorRules(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	_, _, err := h.Account("")
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "no account" || !errors.Is(err, service.ErrNoAccount) {
		t.Errorf("zero accounts: %q, want no account", ee.Error())
	}
	_, _, err = h.Account("nobody@x")
	if ee := wantExit(t, err, exitcode.Usage); ee.Error() != "no account nobody@x" {
		t.Errorf("unknown: %q", ee.Error())
	}

	ra := e.mustLogin(t, h, userA)
	if _, got, err := h.Account(""); err != nil || got.ID != ra.Account {
		t.Fatalf(`one account: Account("") = %+v, %v`, got, err)
	}

	rb := e.mustLogin(t, h, userB)
	_, _, err = h.Account("")
	ee := wantExit(t, err, exitcode.Usage)
	if ee.Error() != "several accounts: name one with --account" {
		t.Errorf("two accounts: %q", ee.Error())
	}
	var several *service.SeveralAccountsError
	if !errors.As(err, &several) || !slices.Equal(ids(several.Accounts), ids([]service.AccountInfo{{ID: ra.Account}, {ID: rb.Account}})) {
		t.Errorf("two accounts: the error does not list both: %+v", several)
	}
	for sel, want := range map[string]string{rb.Account: rb.Account, "B@X": rb.Account, "A@x": ra.Account, ra.Account: ra.Account} {
		if _, got, err := h.Account(sel); err != nil || got.ID != want {
			t.Errorf("Account(%q) = %+v, %v, want %s", sel, got, err, want)
		}
	}
}

func TestRemoveDeletesOnlyThatAccount(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	rb := e.mustLogin(t, h, userB)
	info, err := h.Remove(t.Context(), userA, service.LogoutRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID != ra.Account {
		t.Errorf("Remove returned %+v, want a", info)
	}
	if _, err := os.Stat(store.AccountDir(e.dirs.State, ra.Account)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a's directory is still there: %v", err)
	}
	if sessionAliveOn(t, e.f.URL, e.session(userA)) {
		t.Error("a's session was not revoked")
	}
	if got := ids(h.Accounts()); !slices.Equal(got, []string{rb.Account}) {
		t.Fatalf("Accounts = %v, want only b", got)
	}
	if st := status(t, h, ""); st.Phase != string(service.StateReady) || st.Address != userB {
		t.Errorf("b = %+v, want ready", st)
	}
}

func TestSetCalDAVPersists(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	info, err := h.SetCalDAV(t.Context(), userA, false)
	if err != nil || info.CalDAV {
		t.Fatalf("SetCalDAV(off) = %+v, %v", info, err)
	}
	_ = h.Close()
	h = e.open(t)
	if got := find(h.Accounts(), ra.Account); got == nil || got.CalDAV {
		t.Fatalf("after reopening: %+v, want CalDAV off", got)
	}
	if info, err := h.SetCalDAV(t.Context(), ra.Account, true); err != nil || !info.CalDAV {
		t.Fatalf("SetCalDAV(on) = %+v, %v", info, err)
	}
	if got := find(h.Accounts(), ra.Account); got == nil || !got.CalDAV {
		t.Fatalf("after on: %+v", got)
	}
}

// Ruling R11, lazily: an add whose 2FA code never came is gone at the next
// listing once its login expired.
func TestExpiredTOTPAddIsRemovedLazily(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.f.SetTwoFA(userB, true)
	rb := e.mustLogin(t, h, userB)
	if rb.State != service.StateNeedsTOTP || find(h.Accounts(), rb.Account) == nil {
		t.Fatalf("Login(b) = %+v, want a listed needs-totp add", rb)
	}
	e.expireAll()
	if got := h.Accounts(); len(got) != 0 {
		t.Fatalf("Accounts after the login expired = %+v, want none", got)
	}
	if got := e.accountDirs(t); len(got) != 0 {
		t.Errorf("accounts/ = %v, want empty", got)
	}
}

// Ruling R11, at open: an abandoned add's directory goes once it is older
// than the login TTL and a margin; a younger one may be another process's
// add in flight and stays (unlisted).
func TestOpenHubRemovesOldAbandonedAdds(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	_ = e.open(t).Close()
	old, young := "0000000a", "0000000b"
	for id, age := range map[string]time.Duration{old: 7 * time.Minute, young: time.Minute} {
		dir := store.AccountDir(e.dirs.State, id)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		at := e.clk.Now().Add(-age)
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
	}
	h := e.open(t)
	if got := h.Accounts(); len(got) != 0 {
		t.Errorf("Accounts = %+v, want none listed", got)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{young}) {
		t.Fatalf("accounts/ = %v, want only the young %s", got, young)
	}
}

func TestSecondLoginWhileOneRunsIsRefused(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	e.f.StallNext("GET", "/core/v4/keys/salts", 2*time.Second)
	done := make(chan error, 1)
	go func() {
		_, err := e.login(t, h, userA)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.f.Calls("GET", "/core/v4/keys/salts") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first login never reached its key calls")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := h.Login(t.Context(), service.LoginRequest{Username: userB, Password: passwordB})
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != "a login is already in progress" {
		t.Errorf("second login: %q", ee.Error())
	}
	if err := <-done; err != nil {
		t.Fatalf("first login: %v", err)
	}
}

func TestOnChangeRunsOutsideTheHubLock(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	var calls atomic.Int32
	var seen atomic.Int32
	h.OnChange(func() {
		calls.Add(1)
		seen.Store(int32(len(h.Accounts()))) // deadlocks if called under the Hub's lock
	})
	ra := e.mustLogin(t, h, userA)
	if calls.Load() != 1 || seen.Load() != 1 {
		t.Fatalf("after an add: %d calls, %d accounts seen", calls.Load(), seen.Load())
	}
	if _, err := h.SetCalDAV(t.Context(), ra.Account, false); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("after a switch: %d calls, want 2", calls.Load())
	}
	// Ruling R17: a sign-out is a change too.
	if _, err := h.SignOut(t.Context(), ra.Account, service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || seen.Load() != 1 {
		t.Fatalf("after a sign-out: %d calls, %d accounts seen, want 3 and 1", calls.Load(), seen.Load())
	}
	if _, err := h.Remove(t.Context(), ra.Account, service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 || seen.Load() != 0 {
		t.Fatalf("after a remove: %d calls, %d accounts seen, want 4 and 0", calls.Load(), seen.Load())
	}
}

// stripAddress removes the address from the root state.json, as an M5
// pcb wrote it (M5 had no such field).
func (e *hubEnv) stripAddress(t *testing.T) { t.Helper(); stripAddressIn(t, e.dirs.State) }

// stripAddressIn removes the address from the state.json of dir.
func stripAddressIn(t *testing.T, dir string) {
	t.Helper()
	if err := store.UpdateState(dir, func(s *store.State) error {
		s.Address = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"address"`) {
		t.Fatalf("state.json still has an address: %s", raw)
	}
}

// Ruling R16: an M5 state.json has no address; the Hub takes it from the
// sealed account, so the next login of that address finds the account.
func TestOpenHubBackfillsTheM5Address(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	e.writeM5(t)
	e.stripAddress(t)
	h := e.open(t)
	list := h.Accounts()
	if len(list) != 1 || list[0].Address != userA {
		t.Fatalf("Accounts = %+v, want a@x", list)
	}
	e.skip(t, h)
	if res := e.mustLogin(t, h, userA); res.Account != list[0].ID {
		t.Fatalf("Login(a) went to %q, want the migrated %q", res.Account, list[0].ID)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{list[0].ID}) {
		t.Errorf("accounts/ = %v, want only the migrated one", got)
	}
}

// Ruling R16: an M5 account migrated signed out has neither a session nor
// an address: the first login of an unknown address adopts it, keeping its
// CalDAV clients.
func TestLoginAdoptsTheSignedOutM5Account(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	pw := e.writeM5(t)
	l, err := service.NewLocal(service.Options{Dirs: e.dirs, Config: e.cfg, Now: e.clk.Now, Sleep: e.clk.Sleep})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Logout(t.Context(), service.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
	e.stripAddress(t)
	h := e.open(t)
	list := h.Accounts()
	if len(list) != 1 || list[0].Address != "" || list[0].Phase != string(store.PhaseLoggedOut) {
		t.Fatalf("Accounts = %+v, want one signed out with no address", list)
	}
	e.skip(t, h)
	if res := e.mustLogin(t, h, userA); res.Account != list[0].ID {
		t.Fatalf("Login(a) went to %q, want the migrated %q", res.Account, list[0].ID)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{list[0].ID}) {
		t.Fatalf("accounts/ = %v, want only the migrated one", got)
	}
	a, info, err := h.Account(userA)
	if err != nil || info.ID != list[0].ID {
		t.Fatalf("Account(a) = %+v, %v", info, err)
	}
	if ok, _ := a.CheckClient(t.Context(), pw, "192.0.2.1:1"); !ok {
		t.Error("the M5 CalDAV client was lost")
	}
}

// Ruling R37: only a signed-out account whose state.json reads is adopted
// by the login of an unknown address. One whose state.json cannot be read
// has no address to show either: the login adds a new account and leaves
// the broken one as it was.
func TestLoginDoesNotAdoptAnUnreadableAccount(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	rb := e.mustLogin(t, h, userB)
	broken := filepath.Join(store.AccountDir(e.dirs.State, rb.Account), "state.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc, err := e.login(t, h, userC)
	if err != nil {
		t.Fatalf("Login(c) with b's state.json broken: %v", err)
	}
	if rc.Account == ra.Account || rc.Account == rb.Account {
		t.Fatalf("Login(c) went to %q, want a new account (a %q, b %q)", rc.Account, ra.Account, rb.Account)
	}
	if got, want := e.accountDirs(t), ids([]service.AccountInfo{{ID: ra.Account}, {ID: rb.Account}, {ID: rc.Account}}); !slices.Equal(got, want) {
		t.Errorf("accounts/ = %v, want %v", got, want)
	}
	if raw, err := os.ReadFile(broken); err != nil || string(raw) != "{not json" {
		t.Errorf("b's state.json = %q, %v; want it untouched", raw, err)
	}
	if st := status(t, h, userC); st.Phase != string(service.StateReady) || st.Address != userC {
		t.Errorf("c = %+v, want ready", st)
	}
}

// Ruling R37: an account whose state.json cannot be read is still found by
// the address of its sealed account: the login of that address meets the
// account's fault (exit 10, the way out named) instead of adding a second
// account for the same Proton user; another address of that user is
// refused as a duplicate.
func TestLoginOfAnUnreadableAccountsUserAddsNothing(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	broken := filepath.Join(store.AccountDir(e.dirs.State, ra.Account), "state.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := e.login(t, h, userA)
	if ee := wantExit(t, err, exitcode.MustAct); ee.Hint != "run pcb logout to reset it" {
		t.Errorf("Login(a): %q, hint %q; want a's state fault", ee.Error(), ee.Hint)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{ra.Account}) {
		t.Fatalf("accounts/ after Login(a) = %v, want only a's", got)
	}
	_, err = e.login(t, h, aliasA)
	if ee := wantExit(t, err, exitcode.MustAct); !strings.Contains(ee.Error(), ra.Account) {
		t.Errorf("Login(alias of a): %q, want the duplicate of %s refused", ee.Error(), ra.Account)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{ra.Account}) {
		t.Errorf("accounts/ after Login(alias of a) = %v, want only a's", got)
	}
	if sessionAliveOn(t, e.f.URL, e.session(aliasA)) {
		t.Error("the duplicate login's session was not revoked")
	}
}

// Ruling R37: a logged-in account with no address in its state.json is
// not adopted either: the login of an unknown address adds an account
// instead of colliding with that account's session.
func TestLoginDoesNotAdoptALoggedInAddresslessAccount(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	dirA := store.AccountDir(e.dirs.State, ra.Account)
	stripAddressIn(t, dirA)
	rc, err := e.login(t, h, userC)
	if err != nil {
		t.Fatalf("Login(c) beside a logged-in account with no address: %v", err)
	}
	if rc.Account == ra.Account {
		t.Fatalf("Login(c) adopted a's account %q", ra.Account)
	}
	st, err := store.LoadState(dirA)
	if err != nil || st.AccountID != e.f.UserID(userA) {
		t.Errorf("a's state = %+v, %v; want still a's user", st, err)
	}
	if got, want := e.accountDirs(t), ids([]service.AccountInfo{{ID: ra.Account}, {ID: rc.Account}}); !slices.Equal(got, want) {
		t.Errorf("accounts/ = %v, want %v", got, want)
	}
}

// Ruling R12: a refused duplicate whose session pcb could not revoke says
// where to end it.
func TestDuplicateRefusalNamesTheSessionsPageWhenTheRevokeFails(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	e.f.FailNext("DELETE", "/auth/v4", 400, 2001, 0)
	_, err := e.login(t, h, aliasA)
	ee := wantExit(t, err, exitcode.MustAct)
	if !strings.Contains(ee.Hint, service.SessionsPage) {
		t.Errorf("hint %q does not name %s", ee.Hint, service.SessionsPage)
	}
	if !strings.Contains(ee.Error(), userA) {
		t.Errorf("message %q does not name %s", ee.Error(), userA)
	}
	if got := e.accountDirs(t); !slices.Equal(got, []string{ra.Account}) {
		t.Errorf("accounts/ = %v, want only a's", got)
	}
}

// A call in flight on the account's API when Remove begins ends before the
// directory goes: it cannot bring the directory back.
func TestRemoveWaitsForTheCallsInFlight(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	a, _, err := h.Account(userA)
	if err != nil {
		t.Fatal(err)
	}
	dir := store.AccountDir(e.dirs.State, ra.Account)
	// Two client adds in flight: the first waits for clients.lock (held
	// here, as another process would), the second for the first.
	unlock, err := filelock.Lock(t.Context(), filepath.Join(dir, "clients.lock"))
	if err != nil {
		t.Fatal(err)
	}
	adds := make(chan error, 2)
	for _, name := range []string{"one", "two"} {
		go func() {
			_, err := a.AddClient(context.WithoutCancel(t.Context()), name)
			adds <- err
		}()
		time.Sleep(100 * time.Millisecond)
	}
	removed := make(chan error, 1)
	go func() {
		_, err := h.Remove(t.Context(), ra.Account, service.LogoutRequest{})
		removed <- err
	}()
	select {
	case err := <-removed:
		unlock()
		t.Fatalf("Remove returned while calls were in flight: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	for range 2 {
		if err := <-adds; err != nil {
			t.Errorf("a client add in flight before Remove failed: %v", err)
		}
	}
	if err := <-removed; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a's directory is back after Remove: %v", err)
	}
	if _, err := a.Clients(t.Context()); err == nil {
		t.Error("a call after Remove succeeded")
	}
}

// A call that comes while Remove runs waits for it, then finds no account.
func TestCallDuringRemoveFindsNoAccount(t *testing.T) {
	t.Parallel()
	e := newHubEnv(t)
	h := e.open(t)
	e.skip(t, h)
	ra := e.mustLogin(t, h, userA)
	a, _, err := h.Account(userA)
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	h.LocalOf(ra.Account).OnWipe(func() {
		close(entered)
		<-release
	})
	removed := make(chan error, 1)
	go func() {
		_, err := h.Remove(t.Context(), ra.Account, service.LogoutRequest{})
		removed <- err
	}()
	<-entered
	added := make(chan error, 1)
	go func() {
		_, err := a.AddClient(t.Context(), "late")
		added <- err
	}()
	select {
	case err := <-added:
		close(release)
		t.Fatalf("AddClient returned during Remove: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-removed; err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if ee := wantExit(t, <-added, exitcode.Usage); !strings.HasPrefix(ee.Error(), "no account") {
		t.Errorf("AddClient after Remove: %q, want no account", ee.Error())
	}
	if _, err := os.Stat(store.AccountDir(e.dirs.State, ra.Account)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a's directory is there after Remove: %v", err)
	}
}
