package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/cli"
	"github.com/Barben360/proton-calendar-bridge/internal/config"
	"github.com/Barben360/proton-calendar-bridge/internal/ctl"
	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/paths"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
)

// The fake's users of the multi-account tests: a is the fake's own user
// (its password is the fake's), b and c are added with passwordB.
const (
	userA     = "a@x"
	userB     = "b@x"
	userC     = "c@x"
	passwordB = "cli-test-password-b"
)

// accountsRig is a rig on a fake whose own user is a, with b and c as
// further users, and no account added yet.
func accountsRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, &protontest.Options{Username: userA})
	r.f.AddUser(userB, passwordB)
	r.f.AddUser(userC, passwordB)
	return r
}

// password is the fake's password of username.
func (r *rig) password(username string) string {
	if username == userA {
		return r.f.Password
	}
	return passwordB
}

// loginAs runs pcb login for username without a terminal, wanting code,
// then moves the clock past the login guard (one login a minute).
func loginAs(t *testing.T, r *rig, username string, code exitcode.Code) {
	t.Helper()
	r.env["PCB_USERNAME"], r.env["PCB_PASSWORD"] = username, r.password(username)
	r.want(code, "login", "--non-interactive")
	r.advance(2 * time.Minute)
}

// twoAccounts is accountsRig with a then b logged in.
func twoAccounts(t *testing.T) *rig {
	t.Helper()
	r := accountsRig(t)
	loginAs(t, r, userA, exitcode.OK)
	loginAs(t, r, userB, exitcode.OK)
	return r
}

// accountRow is one entry of pcb account list --json.
type accountRow struct {
	ID       string     `json:"id"`
	Address  string     `json:"address"`
	Phase    string     `json:"phase"`
	Pending  bool       `json:"pending"`
	CalDAV   bool       `json:"caldav"`
	LastSync *time.Time `json:"last_sync"`
}

func accountList(t *testing.T, r *rig) []accountRow {
	t.Helper()
	r.want(exitcode.OK, "account", "list", "--json")
	var data struct {
		Accounts []accountRow `json:"accounts"`
	}
	mustUnmarshal(t, r.envelope().Data, &data)
	if data.Accounts == nil {
		t.Fatalf("account list --json: no accounts array: %s", r.out.String())
	}
	return data.Accounts
}

// accountID is the local ID of the account address.
func accountID(t *testing.T, r *rig, address string) string {
	t.Helper()
	for _, a := range accountList(t, r) {
		if a.Address == address {
			return a.ID
		}
	}
	t.Fatalf("no account %s", address)
	return ""
}

var localIDPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)

// Design §4.3: the username decides: a new one adds an account, a known
// one logs that account in again.
func TestLoginAddsAccountByUsername(t *testing.T) {
	t.Parallel()
	r := twoAccounts(t)
	list := accountList(t, r)
	if len(list) != 2 || list[0].Address != userA || list[1].Address != userB || list[0].ID == list[1].ID {
		t.Fatalf("accounts = %+v, want a then b, two IDs", list)
	}
	for _, a := range list {
		if !localIDPattern.MatchString(a.ID) || a.Phase != string(service.StateReady) || a.Pending || !a.CalDAV {
			t.Errorf("account %+v, want an 8-hex ID, ready, not pending, CalDAV on", a)
		}
	}
	loginAs(t, r, userA, exitcode.OK)
	if again := accountList(t, r); len(again) != 2 || again[0].ID != list[0].ID || again[1].ID != list[1].ID {
		t.Errorf("after a's second login: %+v, want the same two accounts", again)
	}
	r.want(exitcode.OK, "account", "list")
	for _, a := range list {
		if !strings.Contains(r.out.String(), a.ID) || !strings.Contains(r.out.String(), a.Address) {
			t.Errorf("account list lacks %s %s:\n%s", a.ID, a.Address, r.out.String())
		}
	}
}

// pcb login takes no selector: the username decides (exit 2).
func TestLoginRefusesAnAccountSelector(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
	r := stubRig(t, s)
	r.want(exitcode.Usage, "login", "--account", "a@x", "--json")
	if e := r.envelope().Error; e == nil || !strings.Contains(e.Message, "--account") || !strings.Contains(e.Message, "username") {
		t.Errorf("error = %+v, want it to say the username decides", e)
	}
	if len(s.logins) != 0 {
		t.Error("the login reached the service")
	}
	// PCB_ACCOUNT, an environment default for the other commands, is not
	// a refusal.
	r.env["PCB_ACCOUNT"] = "a@x"
	r.want(exitcode.OK, "login", "--non-interactive")
}

// Design §4.6: with several accounts and none named, a command exits 2
// and lists them (--json: data.accounts).
func TestSelectorRequiredWithTwoAccounts(t *testing.T) {
	t.Parallel()
	r := twoAccounts(t)
	calls := r.f.Calls("GET", "/calendar/")
	r.want(exitcode.Usage, "calendar", "list", "--json")
	e := r.envelope()
	if e.Error == nil || e.Error.Message != "several accounts: name one with --account" || !strings.Contains(e.Error.Hint, "--account") {
		t.Fatalf("error = %+v", e.Error)
	}
	var data struct {
		Accounts []service.AccountInfo `json:"accounts"`
	}
	mustUnmarshal(t, e.Data, &data)
	if len(data.Accounts) != 2 || data.Accounts[0].Address != userA || data.Accounts[1].Address != userB {
		t.Errorf("data.accounts = %+v, want a and b", data.Accounts)
	}
	r.want(exitcode.Usage, "calendar", "list")
	for _, want := range []string{"Error: several accounts: name one with --account\n", userA, userB} {
		if !strings.Contains(r.errb.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.errb.String())
		}
	}
	if n := r.f.Calls("GET", "/calendar/"); n != calls {
		t.Errorf("the refused commands made %d calendar requests", n-calls)
	}
}

func calendarNames(t *testing.T, r *rig, args ...string) []string {
	t.Helper()
	r.want(exitcode.OK, append([]string{"calendar", "list", "--json"}, args...)...)
	var data calendarList
	mustUnmarshal(t, r.envelope().Data, &data)
	var names []string
	for _, c := range data.Calendars {
		names = append(names, c.Name)
	}
	return names
}

type calendarList struct {
	Calendars []service.Calendar `json:"calendars"`
}

// PCB_ACCOUNT selects as --account does (address case-insensitive, or the
// local ID); the flag wins over it.
func TestPCBAccountEnv(t *testing.T) {
	t.Parallel()
	r := accountsRig(t)
	r.f.AddCalendar(userA, protontest.Calendar{Name: "Work A"})
	r.f.AddCalendar(userB, protontest.Calendar{Name: "Home B"})
	loginAs(t, r, userA, exitcode.OK)
	loginAs(t, r, userB, exitcode.OK)
	idA := accountID(t, r, userA)

	r.env["PCB_ACCOUNT"] = "B@X"
	if got := calendarNames(t, r); !slices.Equal(got, []string{"Home B"}) {
		t.Errorf("PCB_ACCOUNT=B@X: calendars %q, want b's", got)
	}
	if got := calendarNames(t, r, "--account", idA); !slices.Equal(got, []string{"Work A"}) {
		t.Errorf("--account a's ID over PCB_ACCOUNT: calendars %q, want a's", got)
	}
	r.env["PCB_ACCOUNT"] = "nobody@x"
	r.want(exitcode.Usage, "calendar", "list", "--json")
	if e := r.envelope().Error; e == nil || e.Message != "no account nobody@x" {
		t.Errorf("unknown account: error = %+v", e)
	}
}

// Ruling R38: with no account a command that needs one is exit 10 (a
// person must add one), not 2: the invocation itself is sound. A selector
// naming nothing stays 2.
func TestNoAccountIsExit10(t *testing.T) {
	t.Parallel()
	r := accountsRig(t)
	r.want(exitcode.MustAct, "calendar", "list", "--json")
	if e := r.envelope().Error; e == nil || e.Message != "no account" || e.Hint != "add one with pcb login" {
		t.Errorf("no account: error = %+v", e)
	}
	r.want(exitcode.Usage, "calendar", "list", "--json", "--account", "nobody@x")
	if e := r.envelope().Error; e == nil || e.Message != "no account nobody@x" {
		t.Errorf("unknown account: error = %+v", e)
	}
}

// Design §4.6: pcb status shows every account (the ctl.Status shape,
// daemon null offline); --account narrows it to one.
func TestStatusShowsEveryAccount(t *testing.T) {
	t.Parallel()
	r := twoAccounts(t)
	r.want(exitcode.OK, "status", "--json")
	var raw map[string]json.RawMessage
	mustUnmarshal(t, r.envelope().Data, &raw)
	if string(raw["daemon"]) != "null" {
		t.Errorf("daemon = %s, want null offline", raw["daemon"])
	}
	var st ctl.Status
	mustUnmarshal(t, r.envelope().Data, &st)
	if len(st.Accounts) != 2 {
		t.Fatalf("status accounts = %+v, want two", st.Accounts)
	}
	for i, want := range []string{userA, userB} {
		a := st.Accounts[i]
		if a.Address != want || a.Login.Phase != string(service.StateReady) || a.Login.Address != want || !localIDPattern.MatchString(a.ID) {
			t.Errorf("account %d = %+v, want %s ready", i, a, want)
		}
	}
	r.want(exitcode.OK, "status", "--json", "--account", userB)
	var one ctl.Status
	mustUnmarshal(t, r.envelope().Data, &one)
	if len(one.Accounts) != 1 || one.Accounts[0].Address != userB {
		t.Errorf("status --account b = %+v, want b only", one.Accounts)
	}
	r.want(exitcode.OK, "status")
	out := r.out.String()
	for _, want := range []string{"account: " + userA + " (" + st.Accounts[0].ID + ")\n", "account: " + userB + " (" + st.Accounts[1].ID + ")\n"} {
		if strings.Count(out, want) != 1 {
			t.Errorf("status names %q %d times, want once:\n%s", want, strings.Count(out, want), out)
		}
	}
	if strings.Count(out, "phase: ready\n") != 2 || strings.Count(out, "daemon: not running (offline view)") != 1 {
		t.Errorf("status = %s; want two ready logins and the daemon line once", out)
	}
}

// Design §4.4: client show prints the account's own URL and its address
// as the username.
func TestClientShowPerAccountURL(t *testing.T) {
	t.Parallel()
	r := twoAccounts(t)
	idA, idB := accountID(t, r, userA), accountID(t, r, userB)
	r.want(exitcode.OK, "client", "show", "--account", userB, "--json")
	var data map[string]any
	mustUnmarshal(t, r.envelope().Data, &data)
	if data["url"] != "http://localhost:1080/dav/"+idB+"/" || data["username"] != userB || data["name"] != "default" {
		t.Errorf("client show b = %v, want b's URL and address", data)
	}
	got := showText(t, r, "--account", userA)
	if got["URL"] != "http://localhost:1080/dav/"+idA+"/" || got["Username"] != userA {
		t.Errorf("client show a = %q", r.out.String())
	}
}

// Design §4.3: removing an account needs a confirmation: --yes, or the
// word typed at a prompt; --non-interactive without --yes exits 10.
func TestAccountRemoveNeedsYes(t *testing.T) {
	t.Parallel()
	r := accountsRig(t)
	loginAs(t, r, userA, exitcode.OK)
	r.want(exitcode.MustAct, "account", "remove", "--non-interactive", "--json")
	if e := r.envelope().Error; e == nil || !strings.Contains(e.Message+e.Hint, "--yes") {
		t.Errorf("error = %+v, want it to name --yes", e)
	}
	r.term.interactive = true
	r.term.lines = []string{"no"}
	r.want(exitcode.MustAct, "account", "remove")
	if len(r.term.linePrompts) != 1 || !strings.Contains(r.term.linePrompts[0], userA) {
		t.Errorf("prompts = %q, want one naming %s", r.term.linePrompts, userA)
	}
	if list := accountList(t, r); len(list) != 1 {
		t.Fatalf("after the refusals: %+v, want the account kept", list)
	}
	revoked := r.f.Calls("DELETE", "/auth/v4")
	r.term.interactive = false
	r.want(exitcode.OK, "account", "remove", "--yes", "--json")
	var info service.AccountInfo
	mustUnmarshal(t, r.envelope().Data, &info)
	if info.Address != userA {
		t.Errorf("removed = %+v, want a", info)
	}
	if list := accountList(t, r); len(list) != 0 {
		t.Errorf("after the removal: %+v, want none", list)
	}
	if n := r.f.Calls("DELETE", "/auth/v4"); n != revoked+1 {
		t.Errorf("%d DELETE /auth/v4, want 1 (the session revoked)", n-revoked)
	}
}

// Design §4.5: pcb account caldav on|off switches the account's adapter.
func TestAccountCalDAVOff(t *testing.T) {
	t.Parallel()
	r := accountsRig(t)
	loginAs(t, r, userA, exitcode.OK)
	r.want(exitcode.OK, "account", "caldav", "off", "--json")
	var info service.AccountInfo
	mustUnmarshal(t, r.envelope().Data, &info)
	if info.CalDAV || info.Address != userA {
		t.Errorf("caldav off = %+v", info)
	}
	if list := accountList(t, r); len(list) != 1 || list[0].CalDAV {
		t.Errorf("account list after off = %+v, want caldav false", list)
	}
	r.want(exitcode.OK, "account", "caldav", "on")
	if !strings.Contains(r.out.String(), "on") {
		t.Errorf("caldav on: stdout %q", r.out.String())
	}
	if list := accountList(t, r); len(list) != 1 || !list[0].CalDAV {
		t.Errorf("account list after on = %+v, want caldav true", list)
	}
	r.want(exitcode.Usage, "account", "caldav", "maybe", "--json")
}

// Design §4.6: the --json shapes changed: every envelope says schema 2.
func TestSchemaVersionIs2(t *testing.T) {
	t.Parallel()
	s := &stubAPI{login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }, statusPhase: "ready"}
	r := stubRig(t, s)
	for _, args := range [][]string{
		{"version"}, {"status"}, {"account", "list"}, {"calendar", "list"}, {"event", "list", "--calendar", "Work"},
		{"event", "list"}, {"sync"}, {"client", "list"}, {"login", "--non-interactive"}, {"logout"}, {"bogus"},
	} {
		r.run(append(args, "--json")...)
		var env struct {
			SchemaVersion *int `json:"schema_version"`
		}
		if err := json.Unmarshal(r.out.Bytes(), &env); err != nil || env.SchemaVersion == nil || *env.SchemaVersion != 2 {
			t.Errorf("pcb %s --json: %s (%v), want schema_version 2", strings.Join(args, " "), r.out.String(), err)
		}
	}
}

// Design §4.6: doctor runs the process checks once and the account checks
// per account, each named by its account's address.
func TestDoctorNamesTheAccount(t *testing.T) {
	t.Parallel()
	r := twoAccounts(t)
	r.want(exitcode.OK, "doctor", "--json")
	var data struct {
		Results []struct{ Name, Status string } `json:"results"`
	}
	mustUnmarshal(t, r.envelope().Data, &data)
	count := map[string]int{}
	for _, res := range data.Results {
		count[res.Name]++
	}
	for _, name := range []string{"platform", "config file", "key file", "caldav listener", "tls certificate", "service", "daemon"} {
		if count[name] != 1 {
			t.Errorf("%q appears %d times, want once: %+v", name, count[name], data.Results)
		}
	}
	for _, acct := range []string{userA, userB} {
		for _, check := range []string{"sealed secrets", "secrets", "cache", "caldav clients"} {
			if name := acct + ": " + check; count[name] != 1 {
				t.Errorf("%q appears %d times, want once: %+v", name, count[name], data.Results)
			}
		}
	}
	for _, check := range []string{"sealed secrets", "secrets", "cache", "caldav clients"} {
		if count[check] != 0 {
			t.Errorf("%q has no account in its name: %+v", check, data.Results)
		}
	}
}

// Design §4.4: pcb serve prints one Thunderbird URL per account, its own.
func TestServePrintsEveryAccountURL(t *testing.T) {
	t.Parallel()
	r := twoAccounts(t)
	idA, idB := accountID(t, r, userA), accountID(t, r, userB)
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	port := portOf(s.listening())
	out := s.out.String()
	for _, want := range []string{
		"Thunderbird URL (" + userA + "): http://localhost:" + port + "/dav/" + idA + "/\n",
		"Thunderbird URL (" + userB + "): http://localhost:" + port + "/dav/" + idB + "/\n",
	} {
		if strings.Count(out, want) != 1 {
			t.Errorf("stdout has %q %d times, want once:\n%s", want, strings.Count(out, want), out)
		}
	}
	if n := strings.Count(out, "Thunderbird URL ("); n != 2 {
		t.Errorf("%d Thunderbird URL lines, want 2:\n%s", n, out)
	}
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
}

// The intro names no URL for an account CalDAV does not serve: one whose
// adapter is off, or a pending add.
func TestServeIntroSkipsPendingAndCalDAVOffAccounts(t *testing.T) {
	t.Parallel()
	r := accountsRig(t)
	loginAs(t, r, userA, exitcode.OK)
	loginAs(t, r, userB, exitcode.OK)
	r.want(exitcode.OK, "account", "caldav", "off", "--account", userB)
	r.f.SetCaptchaFor(userC, "T1")
	loginAs(t, r, userC, exitcode.MustAct) // a pending add
	idA := accountID(t, r, userA)
	s := startServe(t, r, nil, "--listen", "127.0.0.1:0")
	port := portOf(s.listening())
	out := s.out.String()
	if want := "Thunderbird URL (" + userA + "): http://localhost:" + port + "/dav/" + idA + "/\n"; !strings.Contains(out, want) {
		t.Errorf("stdout lacks %q:\n%s", want, out)
	}
	if n := strings.Count(out, "Thunderbird URL ("); n != 1 || strings.Contains(out, userB) || strings.Contains(out, userC) {
		t.Errorf("stdout = %q; want a's URL only", out)
	}
	s.stop()
	_ = s.exit()
}

// Hub.Serve reports no waiting state without an account: pcb serve
// --wait-for-login says it waits all the same.
func TestServeWaitsForALoginWithNoAccount(t *testing.T) {
	t.Parallel()
	r := newRig(t, &protontest.Options{})
	s := startServe(t, r, nil, "--wait-for-login", "--listen", "127.0.0.1:0")
	s.listening()
	if out := s.out.String(); !strings.HasSuffix(out, "\n"+waitingText+"\n") || strings.Contains(out, "Thunderbird URL") {
		t.Errorf("stdout = %q, want the intro then %q", out, waitingText)
	}
	s.stop()
	if code := s.exit(); code != exitcode.OK {
		t.Fatalf("exit %d\nstderr: %s", code, s.errb.String())
	}
}

const waitingText = "Waiting for a login: run pcb login"

// pcb login resume picks the only login waiting for a CAPTCHA, and logs in
// its address, with no selector and no username.
func TestLoginResumeSelectsThePendingLogin(t *testing.T) {
	t.Parallel()
	r := accountsRig(t)
	loginAs(t, r, userA, exitcode.OK)
	r.f.SetCaptchaFor(userB, "T1")
	loginAs(t, r, userB, exitcode.MustAct)
	delete(r.env, "PCB_USERNAME")
	r.env["PCB_PASSWORD"] = passwordB
	r.want(exitcode.OK, "login", "resume", "--captcha-token", "T1", "--non-interactive", "--json")
	var res service.LoginResult
	mustUnmarshal(t, r.envelope().Data, &res)
	if res.State != service.StateReady || res.Username != userB {
		t.Errorf("resume = %+v, want b ready", res)
	}
	list := accountList(t, r)
	if len(list) != 2 || list[1].Address != userB || list[1].Pending || list[1].Phase != string(service.StateReady) {
		t.Errorf("accounts = %+v, want b ready", list)
	}
}

// Design §4.6: pcb uninstall --delete-data signs out every account of the
// unit's state before deleting it: both sessions revoked at Proton.
func TestUninstallSignsOutEveryAccount(t *testing.T) {
	t.Parallel()
	s := newSysRig(t)
	s.want(exitcode.OK, "install")
	r := accountsRig(t)
	// The rig's accounts live in the unit's directories (its commands
	// still resolve their control socket from the rig's own, short path).
	r.dirs = paths.Dirs{Config: filepath.Join(s.root, userConfigDir), State: filepath.Join(s.root, userStateDir)}
	if err := os.WriteFile(config.Path(r.dirs), []byte("[proton]\nbase_url = \""+r.f.URL+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loginAs(t, r, userA, exitcode.OK)
	loginAs(t, r, userB, exitcode.OK)
	if n := len(accountList(t, r)); n != 2 {
		t.Fatalf("%d accounts in the unit's state, want 2", n)
	}
	s.openHub = func(env func(string) string) (cli.Hub, error) {
		dirs := paths.Dirs{Config: env(paths.EnvConfigDir), State: env(paths.EnvStateDir)}
		cfg, err := config.Load(config.Path(dirs), dirs)
		if err != nil {
			return nil, err
		}
		h, err := service.OpenHub(service.HubOptions{Options: service.Options{Dirs: dirs, Config: cfg, Now: r.now,
			Sleep: func(context.Context, time.Duration) error { return nil }}})
		if err != nil {
			return nil, err
		}
		return h, nil
	}
	revoked := r.f.Calls("DELETE", "/auth/v4")
	s.want(exitcode.OK, "uninstall", "--delete-data", "--yes", "--non-interactive")
	if n := r.f.Calls("DELETE", "/auth/v4") - revoked; n != 2 {
		t.Errorf("%d sessions revoked, want 2 (both accounts)", n)
	}
	if want := [2]string{r.dirs.Config, r.dirs.State}; s.logoutEnv != want {
		t.Errorf("signed out on %q, want the unit's %q", s.logoutEnv, want)
	}
	if s.exists(userStateDir) {
		t.Error("the state survived --delete-data")
	}
}

// pcb login resume with several logins waiting (for a CAPTCHA, or for a
// 2FA code the daemon holds) and none named exits 2 listing them.
func TestLoginResumeNamesTheWaitingLogins(t *testing.T) {
	t.Parallel()
	t.Run("CAPTCHA", func(t *testing.T) {
		t.Parallel()
		a := &stubAPI{statusPhase: "needs-captcha", login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		b := &stubAPI{statusPhase: "needs-captcha", login: func(service.LoginRequest) (service.LoginResult, error) { return ready() }}
		r := newRig(t, nil)
		r.hub = newStubHub(a, b)
		r.hub.accounts[0].address, r.hub.accounts[1].address = userA, userB
		delete(r.env, "PCB_USERNAME")
		r.want(exitcode.Usage, "login", "resume", "--captcha-token", "T1", "--non-interactive", "--json")
		var data struct {
			Accounts []service.AccountInfo `json:"accounts"`
		}
		mustUnmarshal(t, r.envelope().Data, &data)
		if len(data.Accounts) != 2 || data.Accounts[0].Address != userA || data.Accounts[1].Address != userB {
			t.Errorf("data.accounts = %+v, want a and b", data.Accounts)
		}
		if len(a.logins)+len(b.logins) != 0 {
			t.Error("a login ran")
		}
		r.want(exitcode.OK, "login", "resume", "--captcha-token", "T1", "--account", userB, "--non-interactive")
		if len(b.logins) != 1 || b.logins[0].Username != userB || b.logins[0].CaptchaToken != "T1" || len(a.logins) != 0 {
			t.Errorf("logins a %+v, b %+v; want b's, with the token", a.logins, b.logins)
		}
		// A --username that is not the named account's address is refused.
		r.want(exitcode.Usage, "login", "resume", "--captcha-token", "T1", "--account", userB, "--username", userA, "--non-interactive")
	})
	t.Run("2FA", func(t *testing.T) {
		t.Parallel()
		a := &stubAPI{statusPhase: "needs-totp", submit: func(string) (service.LoginResult, error) { return ready() }}
		b := &stubAPI{statusPhase: "needs-totp", submit: func(string) (service.LoginResult, error) { return ready() }}
		r := newRig(t, nil)
		hub := newStubHub(a, b)
		hub.accounts[0].address, hub.accounts[1].address = userA, userB
		hub.accounts[0].pending, hub.accounts[1].pending = true, true
		startDaemon(t, r, hub, nil)
		noInProcess(t, r)
		r.env["PCB_TOTP"] = "123456"
		r.want(exitcode.Usage, "login", "resume", "--totp", "--json")
		var data struct {
			Accounts []service.AccountInfo `json:"accounts"`
		}
		mustUnmarshal(t, r.envelope().Data, &data)
		if len(data.Accounts) != 2 || len(a.totps)+len(b.totps) != 0 {
			t.Errorf("data.accounts = %+v, codes a %v b %v; want both listed, no code sent", data.Accounts, a.totps, b.totps)
		}
		r.want(exitcode.OK, "login", "resume", "--totp", "--account", userB, "--json")
		if len(b.totps) != 1 || len(a.totps) != 0 || !slices.Equal(hub.calls(), []string{"totp 00000001"}) {
			t.Errorf("codes a %v b %v, calls %v; want b's code only", a.totps, b.totps, hub.calls())
		}
	})
}

// Rulings R14 and R15: with a daemon, the account-changing commands go
// through it (its Hub), never the in-process accounts; with two accounts
// and none named they exit 2 listing them (R31), sending nothing.
func TestAccountChangesGoThroughTheDaemon(t *testing.T) {
	t.Parallel()
	r := newRig(t, nil)
	hub := newStubHub(&stubAPI{statusPhase: "ready"}, &stubAPI{statusPhase: "ready"})
	hub.accounts[0].address, hub.accounts[1].address = userA, userB
	startDaemon(t, r, hub, nil)
	noInProcess(t, r)
	for _, args := range [][]string{{"logout"}, {"account", "caldav", "off"}, {"account", "restart"}, {"account", "remove", "--yes"}} {
		r.want(exitcode.Usage, append(args, "--json")...)
		var data struct {
			Accounts []service.AccountInfo `json:"accounts"`
		}
		mustUnmarshal(t, r.envelope().Data, &data)
		if len(data.Accounts) != 2 {
			t.Errorf("pcb %s: data.accounts = %+v, want both", strings.Join(args, " "), data.Accounts)
		}
	}
	if calls := hub.calls(); len(calls) != 0 {
		t.Fatalf("refused commands reached the Hub: %v", calls)
	}
	r.want(exitcode.OK, "logout", "--account", userB)
	r.want(exitcode.OK, "account", "caldav", "off", "--account", "B@X")
	r.want(exitcode.OK, "account", "restart", "--account", "00000001")
	if !strings.Contains(r.out.String(), "Account "+userB+" (00000001) restarted.") {
		t.Errorf("restart: stdout %q", r.out.String())
	}
	r.want(exitcode.OK, "account", "remove", "--yes", "--account", userB)
	want := []string{"signout 00000001", "caldav 00000001 off", "restart 00000001", "remove 00000001"}
	if got := hub.calls(); !slices.Equal(got, want) {
		t.Errorf("Hub calls = %v, want %v", got, want)
	}
	if list := accountList(t, r); len(list) != 1 || list[0].Address != userA {
		t.Errorf("account list after the removal = %+v, want a only", list)
	}
}

// Without a daemon, pcb account restart has no loop to restart: it says
// when the account is served again.
func TestAccountRestartOffline(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{statusPhase: "ready"})
	r.hub = newStubHub(r.svc)
	r.want(exitcode.OK, "account", "restart")
	if want := "No pcb daemon is running: " + stubAccountID + " is served again at the next pcb serve.\n"; r.out.String() != want {
		t.Errorf("stdout %q, want %q", r.out.String(), want)
	}
	if got := r.hub.calls(); !slices.Equal(got, []string{"restart " + stubAccountID}) {
		t.Errorf("Hub calls = %v, want the restart", got)
	}
}

// Only the commands that act on one account take --account (exit 2
// elsewhere), and only their help offers it.
func TestOnlySelectingCommandsTakeAccount(t *testing.T) {
	t.Parallel()
	r := stubRig(t, &stubAPI{statusPhase: "ready"})
	for _, args := range [][]string{{"doctor"}, {"version"}, {"account", "list"}, {"serve"}} {
		r.want(exitcode.Usage, append(args, "--account", "a@x", "--json")...)
		if e := r.envelope().Error; e == nil || !strings.Contains(e.Message, "takes no --account") {
			t.Errorf("pcb %s --account: error = %+v", strings.Join(args, " "), e)
		}
	}
	for _, c := range []struct {
		args  []string
		offer bool
	}{{[]string{"doctor", "--help"}, false}, {[]string{"calendar", "list", "--help"}, true}, {[]string{"login", "--help"}, false}, {[]string{"login", "resume", "--help"}, true}} {
		r.want(exitcode.OK, c.args...)
		if got := strings.Contains(r.out.String(), "      --account string"); got != c.offer {
			t.Errorf("pcb %s: offers --account %v, want %v:\n%s", strings.Join(c.args, " "), got, c.offer, r.out.String())
		}
	}
}
