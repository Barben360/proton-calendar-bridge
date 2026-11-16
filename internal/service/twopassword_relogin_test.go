package service_test

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/Barben360/proton-calendar-bridge/internal/exitcode"
	"github.com/Barben360/proton-calendar-bridge/internal/proton/protontest"
	"github.com/Barben360/proton-calendar-bridge/internal/service"
	"github.com/Barben360/proton-calendar-bridge/internal/store"
)

// staleKeyPass is the needs-login reason of a relogin whose sealed key
// passphrase no longer opens the keys.
const staleKeyPass = "the mailbox password or the account keys changed"

// loginTwoPasswordUnattended logs in a two-password account with unattended
// relogin, its mailbox password up front and, with 2FA, the TOTP URI.
func (e *env) loginTwoPasswordUnattended(t *testing.T, twoFA bool) {
	t.Helper()
	r := e.req()
	r.UnattendedRelogin = true
	r.MailboxPassword = e.f.MailboxPassword
	if twoFA {
		r.TOTP = e.f.TOTPURI
	}
	if res := e.login(t, r); res.State != service.StateReady {
		t.Fatalf("Login = %+v, want ready", res)
	}
}

// A relogin of a two-password account opens the keys with the sealed key
// passphrase: no salts, no scope unlock, no mailbox password.
func TestTwoPasswordReloginReusesKeyPass(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true, TwoPassword: true, LockedSalts: true})
	e.f.AddCalendar(e.f.Username, protontest.Calendar{Name: "A"})
	e.loginTwoPasswordUnattended(t, true)
	old := e.account(t)
	e.f.RevokeSessions()
	e.clk.advance(61 * time.Second)
	at := e.clk.Now()
	srp, salts, unlock := e.f.Calls("POST", "/auth/v4"), e.f.Calls("GET", "/core/v4/keys/salts"), e.f.Calls("PUT", "/core/v4/users/unlock")
	if got := e.calendars(t); len(got) != 1 || got[0].Keys != "ok" {
		t.Fatalf("calendars = %+v", got)
	}
	if n := e.f.Calls("POST", "/auth/v4") - srp; n != 1 {
		t.Errorf("%d new POST /auth/v4, want 1 (the relogin)", n)
	}
	if n := e.f.Calls("GET", "/core/v4/keys/salts") - salts; n != 0 {
		t.Errorf("%d GET /core/v4/keys/salts during the relogin, want 0", n)
	}
	if n := e.f.Calls("PUT", "/core/v4/users/unlock") - unlock; n != 0 {
		t.Errorf("%d PUT /core/v4/users/unlock during the relogin, want 0", n)
	}
	acc := e.account(t)
	if acc.UID == old.UID || !sessionAlive(t, e, tokensOf(acc)) {
		t.Error("the new session is not what is sealed")
	}
	if !bytes.Equal(acc.KeyPass, old.KeyPass) || !keysOpen(t, e, acc) {
		t.Error("the sealed KeyPass changed, or no longer opens the keys")
	}
	if want := (store.Relogin{Password: e.f.Password, TOTPURI: e.f.TOTPURI}); acc.Relogin == nil || *acc.Relogin != want {
		t.Errorf("Relogin = %+v; want the login password and the TOTP URI only", acc.Relogin)
	}
	if st := e.loadState(t); st.Phase != store.PhaseReady || !st.LastRelogin.Equal(at) || !st.LastLogin.Equal(t0) {
		t.Errorf("state = %+v, want ready, relogin at %v, login at %v", st, at, t0)
	}
}

// The sealed key passphrase no longer opens the keys (the mailbox password
// changed): needs-login, and no further automatic attempt.
func TestReloginStaleKeyPassNeedsLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoPassword: true})
	e.loginTwoPasswordUnattended(t, false)
	e.f.SetMailboxPassword(e.f.Username, "another-mailbox-password")
	e.f.RevokeSessions()
	e.clk.advance(61 * time.Second)
	srp, logouts := e.f.Calls("POST", "/auth/v4"), e.f.Calls("DELETE", "/auth/v4")
	_, err := e.svc.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != staleKeyPass || ee.Hint != "run pcb login" {
		t.Errorf("Calendars = %q (hint %q), want %q (run pcb login)", ee.Error(), ee.Hint, staleKeyPass)
	}
	if st := e.loadState(t); st.Phase != store.PhaseNeedsLogin || st.Reason != staleKeyPass {
		t.Errorf("state = %q (%q), want needs-login (%q)", st.Phase, st.Reason, staleKeyPass)
	}
	if n := e.f.Calls("DELETE", "/auth/v4") - logouts; n != 1 {
		t.Errorf("%d DELETE /auth/v4, want 1 (the relogin's unsealed session revoked)", n)
	}
	e.clk.advance(2 * time.Minute)
	_, err = e.svc.Calendars(t.Context())
	if ee := wantExit(t, err, exitcode.MustAct); ee.Error() != staleKeyPass || ee.Hint != "run pcb login" {
		t.Errorf("second call = %q (hint %q)", ee.Error(), ee.Hint)
	}
	if n := e.f.Calls("POST", "/auth/v4") - srp; n != 1 {
		t.Errorf("%d login attempts, want exactly 1", n)
	}
}

// Unattended relogin of a two-password account seals the login password and
// the TOTP URI, never the mailbox password.
func TestUnattendedTwoPasswordSealsNoMailboxPassword(t *testing.T) {
	t.Parallel()
	e := newEnv(t, protontest.Options{TwoFA: true, TwoPassword: true})
	e.loginTwoPasswordUnattended(t, true)
	a := e.account(t)
	if want := (store.Relogin{Password: e.f.Password, TOTPURI: e.f.TOTPURI}); a.Relogin == nil || *a.Relogin != want {
		t.Errorf("Relogin = %+v; want the login password and the TOTP URI only", a.Relogin)
	}
	sealed, err := json.Marshal(e.accounts(t))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(e.f.MailboxPassword)) {
		t.Error("the sealed store contains the mailbox password")
	}
}
